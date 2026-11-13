package server

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/Autumn-27/artex/intercept"
	"time"
)

func (s *Server) awaitOperatorC2Approval(parent context.Context, taskID, sessionID int64, kind string, payload []byte) (string, error) {
	if s.c2Ops == nil || s.m == nil || s.m.pg == nil {
		return "blocked", fmt.Errorf("C2 approval subsystem unavailable")
	}
	s.c2Ops.mu.Lock()
	if s.c2Ops.closed {
		s.c2Ops.mu.Unlock()
		return "blocked", fmt.Errorf("C2 subsystem closing")
	}
	ctx, cancel := context.WithCancel(parent)
	stopGlobal := context.AfterFunc(s.c2Ops.ctx, cancel)
	if s.c2Ops.approvals == nil {
		s.c2Ops.approvals = map[int64]context.CancelFunc{}
	}
	s.c2Ops.approvals[taskID] = cancel
	s.c2Ops.wg.Add(1)
	s.c2Ops.mu.Unlock()
	defer s.c2Ops.wg.Done()
	defer cancel()
	defer stopGlobal()
	defer func() { s.c2Ops.mu.Lock(); delete(s.c2Ops.approvals, taskID); s.c2Ops.mu.Unlock() }()
	allowed := false
	reason := "C2 approval subsystem unavailable"
	var listenerID int64
	_ = s.m.pg.QueryRowContext(ctx, "SELECT listener_id FROM c2_sessions WHERE id=$1", sessionID).Scan(&listenerID)
	if s.m.interceptor != nil {
		var body any
		if json.Unmarshal(payload, &body) != nil {
			body = map[string]any{}
		}
		raw, _ := json.Marshal(map[string]any{"c2_task_id": taskID, "c2_listener_id": listenerID, "c2_session_id": sessionID, "task_type": kind, "payload": body})
		allowed = s.m.interceptor.HandleAsk(ctx, 0, intercept.Decision{Action: "ask", Message: fmt.Sprintf("C2 会话 #%d 请求执行危险任务 %q，需要人工审批", sessionID, kind), TimeoutEnabled: false}, "c2_task:"+kind, raw)
		reason = "C2 approval rejected or cancelled"
	}
	status, approval := "blocked", "rejected"
	if allowed && ctx.Err() == nil {
		status, approval = "queued", "approved"
		reason = ""
	}
	saveCtx, stop := context.WithTimeout(context.Background(), 5*time.Second)
	defer stop()
	var saved string
	err := s.m.pg.QueryRowContext(saveCtx, `UPDATE c2_tasks SET status=$2,approval_status=$3,error=$4 WHERE id=$1 AND status='awaiting_approval' RETURNING status`, taskID, status, approval, reason).Scan(&saved)
	if err != nil {
		if e := s.m.pg.QueryRowContext(saveCtx, "SELECT status FROM c2_tasks WHERE id=$1", taskID).Scan(&saved); e != nil {
			return "blocked", e
		}
		return saved, nil
	}
	return saved, nil
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
