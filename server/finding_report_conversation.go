package server

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"

	"github.com/Autumn-27/artex/agent"
	"github.com/Autumn-27/artex/db"
	"github.com/Autumn-27/artex/intercept"
)

// The route resolves an authoritative host binding; it never dispatches a run.
// Visiting a panel cannot silently regenerate a report or contact a target/model.
func (s *Server) findingReportConversation(w http.ResponseWriter, r *http.Request) {
	id, ok := pathInt(r, "id")
	if !ok || id <= 0 {
		writeErr(w, http.StatusBadRequest, "bad finding id")
		return
	}
	if s.m == nil || s.m.pg == nil {
		writeErr(w, http.StatusServiceUnavailable, "数据库不可用")
		return
	}
	f, err := s.m.pg.GetFinding(id)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if f == nil {
		writeErr(w, http.StatusNotFound, "finding not found")
		return
	}
	if f.TaskID == nil || *f.TaskID <= 0 {
		writeErr(w, http.StatusConflict, "漏洞未关联真实来源任务")
		return
	}
	c, b, err := s.m.pg.EnsureFindingReportConversation(r.Context(), id)
	if err != nil {
		writeErr(w, http.StatusConflict, "报告会话未就绪: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"conversation_id": c.ID, "task_id": b.TaskID, "finding_id": b.FindingID})
}

type findingReportScopeKey struct{}
type findingReportScope struct {
	// A preexisting ordinary scheduled batch may create multiple exact links as
	// it finishes reports. It is not retroactively narrowed mid-run; later human
	// turns must resolve an unambiguous dedicated finding instead.
	generic bool
	binding *db.FindingReportBinding
}

func (s *Server) findingReportTaskContext(ctx context.Context, c *db.Conversation) (context.Context, error) {
	b, err := s.m.pg.FindingReportBindingForConversation(ctx, c.ID)
	if err != nil {
		return ctx, err
	}
	if b == nil {
		return context.WithValue(ctx, findingReportScopeKey{}, findingReportScope{generic: true}), nil
	}
	if c.AgentKey != "reporter" {
		return ctx, errors.New("报告会话角色已变更")
	}
	parent, err := s.m.pg.GetTask(b.TaskID)
	if err != nil {
		return ctx, fmt.Errorf("读取报告来源任务失败: %w", err)
	}
	if parent == nil {
		return ctx, errors.New("报告来源任务已删除或归档")
	}
	if err := s.reportSourceTaskRunnable(parent.ID); err != nil {
		return ctx, err
	}
	taskID := strconv.FormatInt(parent.ID, 10)
	if s.engine != nil && s.engine.IsDeleting(taskID) {
		return ctx, errors.New("报告来源任务正在删除或归档")
	}
	addr, ca, err := s.m.TaskProxyForTaskChecked(ctx, parent.ID)
	if err != nil {
		return ctx, fmt.Errorf("报告任务代理未就绪，未执行本轮操作: %w", err)
	}
	if s.m.taskProxy == nil {
		if s.m.TrafficEnabled() {
			addr, ca = s.m.ProxyAddr(), s.m.ProxyCACert()
			if addr == "" {
				return ctx, errors.New("报告流量录制代理未就绪，未执行本轮操作")
			}
		} else {
			addr, ca = s.m.GlobalProxy(), ""
		}
	}
	ctx = agent.WithRunInfo(ctx, agent.RunInfo{TaskID: parent.ID, ExplorationID: parent.ExplorationID})
	ctx = intercept.WithTaskContext(ctx, taskID, "reporter", nil)
	ctx = agent.WithChatProxyOverride(ctx, addr, ca)
	return context.WithValue(ctx, findingReportScopeKey{}, findingReportScope{binding: b}), nil
}

func (s *Server) checkFindingReportConversationScope(ctx context.Context, findingID int64) error {
	if scope, ok := ctx.Value(findingReportScopeKey{}).(findingReportScope); ok && scope.generic {
		return nil
	}
	convID := intercept.ConvIDFromContext(ctx)
	if convID <= 0 {
		return nil
	}
	b, err := s.m.pg.FindingReportBindingForConversation(ctx, convID)
	if err != nil {
		return err
	}
	if b == nil {
		return nil
	}
	if b.FindingID != findingID {
		return errors.New("报告会话仅可修改其实际关联的漏洞")
	}
	return nil
}

// Rechecked after registering the task-associated cancellation map, closing the
// race between a concurrent task pause and the start of a reporter turn.
func (s *Server) reportSourceTaskRunnable(taskID int64) error {
	parent, err := s.m.pg.GetTask(taskID)
	if err != nil {
		return fmt.Errorf("读取报告来源任务状态失败: %w", err)
	}
	if parent == nil {
		return errors.New("报告来源任务已删除或归档")
	}
	id := strconv.FormatInt(taskID, 10)
	if parent.Paused || (s.engine != nil && s.engine.IsPaused(id)) {
		return errors.New("报告来源任务已暂停，未执行本轮操作；可继续查看报告或先恢复任务")
	}
	if s.engine != nil && s.engine.IsDeleting(id) {
		return errors.New("报告来源任务正在删除或归档")
	}
	return nil
}
