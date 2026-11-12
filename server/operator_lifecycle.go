package server

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"github.com/Autumn-27/artex/db"
)

func (s *Server) closeOperatorModules() {
	if s.c2Ops == nil {
		return
	}
	s.c2Ops.mu.Lock()
	ids := []int64{}
	for id := range s.c2Ops.running {
		ids = append(ids, id)
	}
	s.c2Ops.mu.Unlock()
	for _, id := range ids {
		s.stopOperatorC2(id)
	}
	s.c2Ops.wg.Wait()
}
func (s *Server) operatorConversationContext(ctx context.Context, c *db.Conversation) (context.Context, []int64, bool, error) {
	for _, kind := range []string{"webshell", "c2"} {
		var b map[string]int64
		err := s.m.pg.OperatorSetting(ctx, fmt.Sprintf("conversation.%s.%d", kind, c.ID), &b)
		if errors.Is(err, sql.ErrNoRows) {
			continue
		}
		if err != nil {
			return ctx, nil, true, conversationAdmissionError()
		}
		if b["task_id"] <= 0 || b["session_id"] <= 0 || c.AgentKey != "pentest" {
			return ctx, nil, true, conversationAdmissionError()
		}
		parent, err := s.m.pg.GetTask(b["task_id"])
		if err != nil || parent == nil || parent.Paused || parent.Status == "done" || parent.Status == "failed" || parent.Status == "timeout" {
			return ctx, nil, true, conversationAdmissionError()
		}
		if kind == "webshell" {
			if s.sessStore == nil {
				return ctx, nil, true, conversationAdmissionError()
			}
			ok, e := s.sessStore.TaskCanUse(ctx, b["session_id"], b["task_id"])
			if e != nil || !ok {
				return ctx, nil, true, conversationAdmissionError()
			}
		} else {
			var status string
			if e := s.m.pg.QueryRowContext(ctx, "SELECT status FROM c2_sessions WHERE id=$1", b["session_id"]).Scan(&status); e != nil || status == "killed" {
				return ctx, nil, true, conversationAdmissionError()
			}
		}
		bound, _, err := s.bindConversationTask(ctx, b["task_id"], c.AgentKey, true)
		return bound, []int64{b["task_id"]}, true, err
	}
	return ctx, nil, false, nil
}
