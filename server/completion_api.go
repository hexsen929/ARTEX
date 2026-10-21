package server

import (
	"encoding/json"
	"errors"
	"github.com/Autumn-27/artex/db"
	"net/http"
	"strconv"
	"strings"
)

func (s *Server) retryTaskCompletion(w http.ResponseWriter, r *http.Request) {
	t, ok := s.m.Task(r.PathValue("id"))
	if !ok {
		writeErr(w, 404, "task not found")
		return
	}
	var input struct {
		AttemptID int64  `json:"attempt_id"`
		Reason    string `json:"reason"`
	}
	if json.NewDecoder(r.Body).Decode(&input) != nil || input.AttemptID <= 0 || strings.TrimSpace(input.Reason) == "" {
		writeErr(w, 400, "收官轮次和恢复原因必填")
		return
	}
	s.engine.deleteMu.RLock()
	defer s.engine.deleteMu.RUnlock()
	s.engine.completionMu.Lock()
	defer s.engine.completionMu.Unlock()
	if s.engine.IsDeleting(t.ID) || s.engine.isCompleting(t.ID) || s.engine.IsPaused(t.ID) || isTerminalStatus(t.lifecycleSnapshot().Status) {
		writeErr(w, 409, "任务仍在收尾、暂停或已结束，不能重新复核")
		return
	}
	id, _ := strconv.ParseInt(t.ID, 10, 64)
	if err := s.m.pg.ResetTaskCompletionFailure(r.Context(), id, input.AttemptID, "human", strings.TrimSpace(input.Reason)); err != nil {
		if errors.Is(err, db.ErrCompletionChanged) {
			writeErr(w, 409, "收官状态已变化，请刷新后再试")
		} else {
			writeErr(w, 500, err.Error())
		}
		return
	}
	s.engine.completionReplan.Delete(t.ID)
	t.Notify()
	writeJSON(w, 200, map[string]bool{"requested": true})
}
