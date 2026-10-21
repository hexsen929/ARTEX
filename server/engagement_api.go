package server

import (
	"encoding/json"
	"net/http"
	"strconv"

	"github.com/Autumn-27/artex/db"
)

func (s *Server) updateEngagement(w http.ResponseWriter, r *http.Request) {
	t, ok := s.m.Task(r.PathValue("id"))
	if !ok {
		writeErr(w, 404, "task not found")
		return
	}
	if !s.engine.beginTaskOperation(t.ID) {
		writeErr(w, 409, "任务正在删除")
		return
	}
	defer s.engine.decInflight(t.ID)
	var change db.EngagementChange
	if json.NewDecoder(r.Body).Decode(&change) != nil {
		writeErr(w, 400, "invalid JSON")
		return
	}
	s.concMu.Lock()
	defer s.concMu.Unlock()
	if !evidenceEditQuiescent(s, t) {
		writeErr(w, 409, "请先暂停任务并等待工作退出，再更新范围、目标和约束")
		return
	}
	id, _ := strconv.ParseInt(t.ID, 10, 64)
	if err := s.m.pg.UpdateEngagement(r.Context(), id, change); err != nil {
		writeErr(w, 409, err.Error())
		return
	}
	t.setContract(change.Goal, change.Description)
	t.NotifyGoalEdited("任务契约", change.Reason)
	writeJSON(w, 200, map[string]any{"ok": true, "paused": true, "note": "目标、约束与范围已同步保存；保留旧契约审计，恢复后按新契约执行与复核。"})
}

func evidenceEditQuiescent(s *Server, t *Task) bool {
	// This HTTP edit owns one operation admission; all other execution must leave.
	return s.engine.IsPaused(t.ID) && s.engine.inflightCount(t.ID) <= 1
}
