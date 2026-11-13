package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"strconv"
	"sync"
	"time"

	"github.com/Autumn-27/artex/agent"
	"github.com/Autumn-27/artex/db"
	"github.com/Autumn-27/artex/intercept"
)

var errConversationRunBusy = errors.New("会话已有运行，未重复执行")

// A run owns its cancellation and inflight leases, never just a mutable map key.
// Owners are selected from database relations or scheduler metadata, not prose.
type taskConversationRun struct {
	ctx            context.Context
	cancel         context.CancelCauseFunc
	conversationID int64
	busyKey        string
	owners         []string
	done           chan struct{}
	release        func()
	once           sync.Once
}

func conversationAdmissionError() error {
	return errors.New("任务关联会话来源、代理或运行状态未就绪，未执行本轮操作")
}

func snapshotFindingTask(raw json.RawMessage) (*int64, error) {
	var snapshot struct {
		Finding *struct {
			TaskID *int64 `json:"task_id"`
		} `json:"finding"`
	}
	if json.Unmarshal(raw, &snapshot) != nil || snapshot.Finding == nil {
		return nil, conversationAdmissionError()
	}
	return snapshot.Finding.TaskID, nil
}

// bindConversationTask never changes shared ChatAgent state. Explanation turns
// preserve provenance for deletion/drain, without creating a proxy listener.
func (s *Server) bindConversationTask(ctx context.Context, id int64, role string, execute bool) (context.Context, bool, error) {
	parent, err := s.m.pg.GetTask(id)
	if err != nil {
		return ctx, false, conversationAdmissionError()
	}
	if parent == nil {
		if execute {
			return ctx, false, conversationAdmissionError()
		}
		return ctx, false, nil
	}
	taskID := strconv.FormatInt(id, 10)
	if s.engine != nil && s.engine.IsDeleting(taskID) {
		return ctx, false, conversationAdmissionError()
	}
	if execute {
		runtime, ok := s.m.Task(taskID)
		if !ok || runtime == nil || runtime.Store == nil || runtime.Store.ID() != parent.ExplorationID {
			return ctx, false, conversationAdmissionError()
		}
		if role == "reporter" && (parent.Paused || (s.engine != nil && s.engine.IsPaused(taskID))) {
			return ctx, false, conversationAdmissionError()
		}
		addr, ca, err := s.m.TaskProxyForTaskChecked(ctx, id)
		if err != nil {
			return ctx, false, conversationAdmissionError()
		}
		if s.m.taskProxy == nil {
			addr, ca = s.m.ProxyAddr(), ""
			if s.m.TrafficEnabled() {
				ca = s.m.ProxyCACert()
				if addr == "" {
					return ctx, false, conversationAdmissionError()
				}
			}
		}
		ctx = agent.WithChatProxyOverride(ctx, addr, ca)
	} else {
		ctx = agent.WithChatProxyOverride(ctx, "", "")
	}
	ctx = agent.WithRunInfo(ctx, agent.RunInfo{TaskID: id, ExplorationID: parent.ExplorationID})
	return intercept.WithTaskContext(ctx, taskID, role, nil), true, nil
}

func (s *Server) conversationTaskContext(ctx context.Context, c *db.Conversation, hints []int64) (context.Context, []int64, error) {
	if bound, owners, ok, err := s.operatorConversationContext(ctx, c); ok {
		return bound, owners, err
	}
	var origin *int64
	execute := false
	switch c.AgentKey {
	case db.FindingVerifierAgentKey:
		record, err := s.m.pg.FindingCheckForConversation(ctx, c.ID)
		if err != nil {
			return ctx, nil, conversationAdmissionError()
		}
		if record == nil {
			if _, expected := s.findingCheckTasks.Load(c.ID); expected {
				return ctx, nil, conversationAdmissionError()
			}
			owners, err := s.trustedConversationOwners(hints)
			return agent.WithExplanationOnly(ctx), owners, err
		}
		if record.Status == "running" {
			return ctx, nil, errConversationRunBusy
		}
		execute = record.Status == "pending"
		origin, err = snapshotFindingTask(record.Snapshot)
		if err != nil {
			return ctx, nil, err
		}
		finding, lookupErr := s.m.pg.GetFinding(record.FindingID)
		if lookupErr != nil {
			return ctx, nil, conversationAdmissionError()
		}
		matches := finding != nil && finding.TaskID != nil && origin != nil && *origin == *finding.TaskID
		if execute && !matches {
			return ctx, nil, conversationAdmissionError()
		}
		if !matches {
			origin = nil
		}
	case db.FindingRetestAgentKey:
		record, err := s.m.pg.FindingRetestForConversation(ctx, c.ID)
		if err != nil {
			return ctx, nil, conversationAdmissionError()
		}
		if record == nil {
			if _, expected := s.findingCheckTasks.Load(c.ID); expected {
				return ctx, nil, conversationAdmissionError()
			}
			owners, err := s.trustedConversationOwners(hints)
			return agent.WithExplanationOnly(ctx), owners, err
		}
		if record.Status == "running" {
			return ctx, nil, errConversationRunBusy
		}
		execute = record.Status == "pending"
		origin, err = snapshotFindingTask(record.Snapshot)
		if err != nil {
			return ctx, nil, err
		}
		finding, lookupErr := s.m.pg.GetFinding(record.FindingID)
		if lookupErr != nil {
			return ctx, nil, conversationAdmissionError()
		}
		matches := finding != nil && finding.TaskID != nil && origin != nil && *origin == *finding.TaskID
		if execute && !matches {
			return ctx, nil, conversationAdmissionError()
		}
		if !matches {
			origin = nil
		}
	case "reporter":
		var err error
		ctx, err = s.findingReportTaskContext(ctx, c)
		if err != nil {
			return ctx, nil, conversationAdmissionError()
		}
		if scope, ok := ctx.Value(findingReportScopeKey{}).(findingReportScope); ok && scope.binding != nil {
			for _, hint := range hints {
				if hint > 0 && hint != scope.binding.TaskID {
					return ctx, nil, conversationAdmissionError()
				}
			}
			bound, _, err := s.bindConversationTask(ctx, scope.binding.TaskID, c.AgentKey, true)
			return bound, []int64{scope.binding.TaskID}, err
		}
		// Scheduler-owned aggregate provenance is lifecycle metadata only. It
		// does not turn a multi-owner batch into its first task's execution.
		owners := []int64{}
		seen := map[int64]bool{}
		for _, id := range hints {
			if id <= 0 || seen[id] {
				continue
			}
			if err := s.reportSourceTaskRunnable(id); err != nil {
				return ctx, nil, conversationAdmissionError()
			}
			parent, err := s.m.pg.GetTask(id)
			if err != nil || parent == nil {
				return ctx, nil, conversationAdmissionError()
			}
			runtime, ok := s.m.Task(strconv.FormatInt(id, 10))
			if !ok || runtime == nil || runtime.Store == nil || runtime.Store.ID() != parent.ExplorationID {
				return ctx, nil, conversationAdmissionError()
			}
			owners, seen[id] = append(owners, id), true
		}
		if len(owners) == 1 {
			bound, _, err := s.bindConversationTask(ctx, owners[0], c.AgentKey, true)
			return bound, owners, err
		}
		return ctx, owners, nil
	default:
		// Trust scheduler metadata for lifecycle only. A custom/Auto event
		// does not become native task execution or inherit a chosen proxy.
		owners, err := s.trustedConversationOwners(hints)
		return ctx, owners, err
	}
	if !execute {
		ctx = agent.WithExplanationOnly(ctx)
	}
	if origin == nil || *origin <= 0 {
		if execute {
			return ctx, nil, conversationAdmissionError()
		}
		owners, err := s.trustedConversationOwners(hints)
		return ctx, owners, err
	}
	for _, hint := range hints {
		if hint > 0 && hint != *origin {
			return ctx, nil, conversationAdmissionError()
		}
	}
	ctx, exists, err := s.bindConversationTask(ctx, *origin, c.AgentKey, execute)
	if err != nil || !exists {
		return ctx, nil, err
	}
	return ctx, []int64{*origin}, nil
}

// Caller holds concMu, matching pause/delete/automatic-check admission.
func (s *Server) prepareConversationRunLocked(c *db.Conversation, busyKey string, hints []int64) (*taskConversationRun, error) {
	if c == nil || s.conversationClosing || s.ctx == nil || s.ctx.Err() != nil || s.m == nil || s.m.pg == nil {
		return nil, conversationAdmissionError()
	}
	s.chatMu.Lock()
	_, exists := s.taskConversationRuns.Load(c.ID)
	s.chatMu.Unlock()
	if exists {
		return nil, errConversationRunBusy
	}
	ctx, cancel := context.WithCancelCause(intercept.WithConvID(s.ctx, c.ID))
	ctx, owners, err := s.conversationTaskContext(ctx, c, hints)
	if err != nil {
		cancel(agent.AbortChatTurnFinished)
		return nil, err
	}
	releases := []string{}
	for _, id := range owners {
		key := strconv.FormatInt(id, 10)
		if s.engine == nil || !s.engine.beginTaskOperation(key) {
			for _, old := range releases {
				s.engine.decInflight(old)
			}
			cancel(agent.AbortChatTurnFinished)
			return nil, conversationAdmissionError()
		}
		releases = append(releases, key)
	}
	run := &taskConversationRun{ctx: ctx, cancel: cancel, conversationID: c.ID, busyKey: busyKey, owners: releases, done: make(chan struct{})}
	run.release = func() {
		for _, id := range releases {
			s.engine.decInflight(id)
		}
	}
	s.chatMu.Lock()
	if s.chatBusy == nil {
		s.chatBusy = map[string]bool{}
	}
	if s.chatCancel == nil {
		s.chatCancel = map[string]context.CancelCauseFunc{}
	}
	s.taskConversationRuns.Store(c.ID, run)
	s.chatBusy[busyKey], s.chatCancel[busyKey] = true, cancel
	if len(releases) == 1 {
		s.findingCheckTasks.Store(c.ID, releases[0])
	}
	s.chatMu.Unlock()
	return run, nil
}

func (s *Server) finishConversationRun(run *taskConversationRun) {
	run.once.Do(func() {
		run.cancel(agent.AbortChatTurnFinished)
		s.chatMu.Lock()
		current := s.taskConversationRuns.CompareAndDelete(run.conversationID, run)
		// This release performs only atomic inflight bookkeeping. Keep busy
		// true until both it and the done signal are complete, so observers
		// cannot mistake a partly cleaned-up run for an idle conversation.
		run.release()
		close(run.done)
		if current {
			s.findingCheckTasks.Delete(run.conversationID)
			delete(s.chatBusy, run.busyKey)
			delete(s.chatCancel, run.busyKey)
		}
		s.chatMu.Unlock()
	})
}

func (s *Server) rejectConversationRun(c *db.Conversation, busyKey string, err error) {
	if c == nil {
		return
	}
	s.chatMu.Lock()
	if _, active := s.taskConversationRuns.Load(c.ID); !active {
		s.findingCheckTasks.Delete(c.ID)
		delete(s.chatBusy, busyKey)
		delete(s.chatCancel, busyKey)
	}
	s.chatMu.Unlock()
	if errors.Is(err, errConversationRunBusy) {
		return
	}
	if s.m == nil || s.m.pg == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	const reason = "任务关联会话未启动：来源、代理或运行上下文未就绪，未执行本轮操作"
	for _, table := range []string{"finding_checks", "finding_retests"} {
		_, _ = s.m.pg.ExecContext(ctx, `UPDATE `+table+` SET status='failed',error=$2,finished_at=now() WHERE conversation_id=$1 AND status='pending'`, c.ID, reason)
	}
	_, _ = s.m.pg.ExecContext(ctx, `INSERT INTO conversation_activities(conversation_id,worker,kind,summary,detail,is_error) VALUES($1,$2,'text',$3,$3,true)`, c.ID, c.AgentKey, reason)
}

func (s *Server) taskConversationWaiters(taskID string) []*taskConversationRun {
	runs := []*taskConversationRun{}
	s.chatMu.Lock()
	s.taskConversationRuns.Range(func(_, value any) bool {
		run, ok := value.(*taskConversationRun)
		if ok {
			for _, owner := range run.owners {
				if owner == taskID {
					runs = append(runs, run)
					break
				}
			}
		}
		return true
	})
	s.chatMu.Unlock()
	return runs
}

func (s *Server) cancelTaskConversationRuns(taskID string, cause error) []*taskConversationRun {
	runs := s.taskConversationWaiters(taskID)
	for _, run := range runs {
		run.cancel(cause)
	}
	return runs
}

func waitConversationRuns(ctx context.Context, runs []*taskConversationRun) error {
	for _, run := range runs {
		select {
		case <-run.done:
		case <-ctx.Done():
			return fmt.Errorf("关联会话仍在收尾，请稍后重试")
		}
	}
	return nil
}

// Only scheduler-owned fields, never headers rendered into a prompt.
func triggerTaskOwners(item triggeredRun) []int64 {
	if len(item.taskIDs) > 0 {
		return append([]int64(nil), item.taskIDs...)
	}
	if item.taskID > 0 {
		return []int64{item.taskID}
	}
	return nil
}

func (s *Server) stopTaskConversationRuns() {
	s.concMu.Lock()
	s.conversationClosing = true
	runs := []*taskConversationRun{}
	s.chatMu.Lock()
	s.taskConversationRuns.Range(func(_, value any) bool {
		if run, ok := value.(*taskConversationRun); ok {
			runs = append(runs, run)
		}
		return true
	})
	s.chatMu.Unlock()
	for _, run := range runs {
		run.cancel(agent.AbortShutdown)
	}
	s.concMu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), taskDeleteDrainTimeout)
	defer cancel()
	if err := waitConversationRuns(ctx, runs); err != nil {
		log.Print("[conversation] shutdown wait timed out; some runs have not confirmed exit")
	}
}

// Recheck only the already-bound parent; never resolve a different task after
// inflight ownership was registered. Tool/model setup happens after this gate.
func (s *Server) validateConversationFindingParent(ctx context.Context, findingID int64) error {
	if agent.ExplanationOnly(ctx) {
		return conversationAdmissionError()
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	ri := agent.RunInfoFrom(ctx)
	finding, err := s.m.pg.GetFinding(findingID)
	if err != nil || finding == nil || finding.TaskID == nil || *finding.TaskID != ri.TaskID || ri.TaskID <= 0 {
		return conversationAdmissionError()
	}
	parent, err := s.m.pg.GetTask(ri.TaskID)
	if err != nil || parent == nil || parent.ExplorationID != ri.ExplorationID {
		return conversationAdmissionError()
	}
	if s.engine != nil && s.engine.IsDeleting(strconv.FormatInt(ri.TaskID, 10)) {
		return conversationAdmissionError()
	}
	return nil
}

// A failed admission never seals a different process's already-running row.
func (s *Server) sealUnstartedConversation(runCtx context.Context, c *db.Conversation, table, status string) {
	if table != "finding_checks" && table != "finding_retests" {
		return
	}
	if runCtx.Err() != nil {
		status = "stopped"
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_, _ = s.m.pg.ExecContext(ctx, `UPDATE `+table+` SET status=$2,error='运行上下文未就绪或已停止，未执行本轮操作',finished_at=now() WHERE conversation_id=$1 AND status='pending'`, c.ID, status)
}

// Host-side scheduler provenance only. No caller text is parsed and no runtime
// task context, tool registry or traffic route is granted by this helper.
func (s *Server) trustedConversationOwners(hints []int64) ([]int64, error) {
	owners := []int64{}
	seen := map[int64]bool{}
	for _, id := range hints {
		if id < 0 {
			return nil, conversationAdmissionError()
		}
		if id == 0 || seen[id] {
			continue
		}
		if err := s.reportSourceTaskRunnable(id); err != nil {
			return nil, conversationAdmissionError()
		}
		parent, err := s.m.pg.GetTask(id)
		if err != nil || parent == nil {
			return nil, conversationAdmissionError()
		}
		runtime, ok := s.m.Task(strconv.FormatInt(id, 10))
		if !ok || runtime == nil || runtime.Store == nil || runtime.Store.ID() != parent.ExplorationID {
			return nil, conversationAdmissionError()
		}
		owners = append(owners, id)
		seen[id] = true
	}
	return owners, nil
}

// Cancellation wins at every pre-start failure point. The injected operations
// are the same validation/CAS used by the runner, and allow deterministic
// synthetic regression barriers without adding global runtime test hooks.
func (s *Server) beginFindingConversation(ctx context.Context, c *db.Conversation, table string, validate func() error, start func() (bool, error)) bool {
	if ctx.Err() != nil {
		s.sealUnstartedConversation(ctx, c, table, "stopped")
		return false
	}
	if err := validate(); err != nil {
		s.sealUnstartedConversation(ctx, c, table, "failed")
		return false
	}
	if ctx.Err() != nil {
		s.sealUnstartedConversation(ctx, c, table, "stopped")
		return false
	}
	started, err := start()
	if err != nil {
		s.sealUnstartedConversation(ctx, c, table, "failed")
		return false
	}
	return started
}
