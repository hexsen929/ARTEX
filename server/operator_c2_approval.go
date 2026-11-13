package server

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/Autumn-27/artex/intercept"
	"time"
)

func (s *Server) requestOperatorC2Approval(taskID, sessionID int64, kind string, payload []byte) {
	if s.c2Ops == nil || s.m == nil || s.m.pg == nil {
		return
	}
	s.c2Ops.mu.Lock()
	if s.c2Ops.closed {
		s.c2Ops.mu.Unlock()
		return
	}
	ctx, cancel := context.WithCancel(s.c2Ops.ctx)
	if s.c2Ops.approvals == nil {
		s.c2Ops.approvals = map[int64]context.CancelFunc{}
	}
	s.c2Ops.approvals[taskID] = cancel
	s.c2Ops.wg.Add(1)
	s.c2Ops.mu.Unlock()
	go func() {
		defer s.c2Ops.wg.Done()
		defer cancel()
		defer func() { s.c2Ops.mu.Lock(); delete(s.c2Ops.approvals, taskID); s.c2Ops.mu.Unlock() }()
		allowed := false
		reason := "C2 approval subsystem unavailable"
		if s.m.interceptor != nil {
			var body any
			if json.Unmarshal(payload, &body) != nil {
				body = map[string]any{}
			}
			raw, _ := json.Marshal(map[string]any{"c2_task_id": taskID, "session_id": sessionID, "type": kind, "payload": body})
			allowed = s.m.interceptor.HandleAsk(ctx, 0, intercept.Decision{Action: "ask", Message: fmt.Sprintf("C2 %s 操作需要人工审批（会话 #%d / 任务 #%d）", kind, sessionID, taskID), TimeoutEnabled: false}, "c2_"+kind, raw)
			reason = "C2 approval rejected or cancelled"
		}
		// A cancel, shutdown or another decision owns the terminal state. Never
		// resurrect a cancelled task after an approval response arrives late.
		status, approval := "blocked", "rejected"
		if allowed && ctx.Err() == nil {
			status, approval = "queued", "approved"
			reason = ""
		}
		saveCtx, stop := context.WithTimeout(context.Background(), 5*time.Second)
		defer stop()
		_, _ = s.m.pg.ExecContext(saveCtx, `UPDATE c2_tasks SET status=$2,approval_status=$3,error=$4 WHERE id=$1 AND status='awaiting_approval'`, taskID, status, approval, reason)
	}()
}
func (s *Server) cancelOperatorC2Approval(id int64) {
	if s.c2Ops == nil {
		return
	}
	s.c2Ops.mu.Lock()
	cancel := s.c2Ops.approvals[id]
	s.c2Ops.mu.Unlock()
	if cancel != nil {
		cancel()
	}
}
func (s *Server) runOperatorC2Sweep(ctx context.Context) {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			_, _ = s.m.pg.ExecContext(ctx, `UPDATE c2_sessions SET status='dead' WHERE status='active' AND last_check_in < now()-interval '180 seconds'`)
		}
	}
}
