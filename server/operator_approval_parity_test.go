package server

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/Autumn-27/artex/intercept"
	"testing"
	"time"
)

func TestOperatorC2OriginalApprovalFlowAndCancellation(t *testing.T) {
	s := operatorFixture(t)
	s.m.interceptor = intercept.New(s.m.pg)
	old, exists, err := s.m.pg.GetSetting("guard_auto_allow_enabled")
	if err != nil {
		t.Fatal(err)
	}
	s.m.interceptor.SetAutoAllow(false, 1)
	t.Cleanup(func() {
		if exists {
			s.m.pg.SetSetting("guard_auto_allow_enabled", old)
		} else {
			s.m.pg.DeleteSetting("guard_auto_allow_enabled")
		}
	})
	code, response := operatorRequest(t, s, "POST", "/api/c2/listeners", map[string]any{"name": "approval fixture", "kind": "http_beacon", "bind_host": "127.0.0.1", "bind_port": 18891})
	if code != 200 {
		t.Fatal(code, response)
	}
	listener := int64(response["id"].(float64))
	defer s.m.pg.Exec("DELETE FROM c2_listeners WHERE id=$1", listener)
	var sid int64
	if err = s.m.pg.QueryRow(`INSERT INTO c2_sessions(listener_id,implant_uuid,hostname) VALUES($1,$2,'fixture') RETURNING id`, listener, fmt.Sprint(time.Now().UnixNano())).Scan(&sid); err != nil {
		t.Fatal(err)
	}
	shell, status, err := s.queueOperatorC2Task(t.Context(), sid, json.RawMessage(`{"type":"shell","command":"fixture-not-executed"}`), "ai")
	if err != nil || status != "queued" || shell <= 0 {
		t.Fatal(status, err)
	}
	for _, cancelled := range []bool{false, true} {
		raw := json.RawMessage(`{"type":"upload","path":"/fixture-only/file","data":"Zml4dHVyZQ=="}`)
		id, status, err := s.queueOperatorC2Task(t.Context(), sid, raw, "manual")
		if err != nil || status != "awaiting_approval" {
			t.Fatal(status, err)
		}
		ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
		done := make(chan string, 1)
		go func() {
			status, err := s.awaitOperatorC2Approval(ctx, id, sid, "upload", []byte(`{"path":"/fixture-only/file","data":"Zml4dHVyZQ=="}`))
			if err != nil {
				done <- "error: " + err.Error()
				return
			}
			done <- status
		}()
		pending := int64(0)
		deadline := time.Now().Add(2 * time.Second)
		for time.Now().Before(deadline) {
			err = s.m.pg.QueryRow(`SELECT id FROM intercept_pending WHERE status='pending' AND tool_name='c2_task:upload' AND tool_input->>'c2_task_id'=$1`, fmt.Sprint(id)).Scan(&pending)
			if pending > 0 {
				break
			}
			time.Sleep(10 * time.Millisecond)
		}
		if pending == 0 {
			cancel()
			t.Fatalf("C2 did not enter the original central approval queue: %v", err)
		}
		t.Cleanup(func() { s.m.pg.Exec("DELETE FROM intercept_pending WHERE id=$1", pending) })
		if cancelled {
			cancel()
		} else {
			status, response := operatorRequest(t, s, "POST", fmt.Sprintf("/api/c2/tasks/%d/approve", id), map[string]any{})
			if status != 200 {
				cancel()
				t.Fatal(status, response)
			}
		}
		got := <-done
		cancel()
		want := "queued"
		if cancelled {
			want = "blocked"
		}
		if got != want {
			t.Fatal(got, want)
		}
	}
}

func TestOperatorC2IndependentConversationKeepsExactSessionBinding(t *testing.T) {
	s := operatorFixture(t)
	var lid, sid, other int64
	if err := s.m.pg.QueryRow(`INSERT INTO c2_listeners(name,kind,bind_host,bind_port,encryption_key,implant_token) VALUES('fixture','http_beacon','127.0.0.1',18891,'fixture','fixture') RETURNING id`).Scan(&lid); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = s.m.pg.Exec("DELETE FROM c2_listeners WHERE id=$1", lid) })
	for _, dest := range []*int64{&sid, &other} {
		if err := s.m.pg.QueryRow(`INSERT INTO c2_sessions(listener_id,implant_uuid,hostname) VALUES($1,$2,'fixture') RETURNING id`, lid, fmt.Sprint(time.Now().UnixNano())).Scan(dest); err != nil {
			t.Fatal(err)
		}
	}
	code, response := operatorRequest(t, s, "POST", fmt.Sprintf("/api/c2/sessions/%d/chat", sid), map[string]any{})
	if code != 200 {
		t.Fatal(code, response)
	}
	cid := int64(response["conversation_id"].(float64))
	defer s.m.pg.Exec("DELETE FROM conversations WHERE id=$1", cid)
	c, err := s.m.pg.GetConversation(cid)
	if err != nil || c == nil {
		t.Fatal(err)
	}
	if c.AgentKey != "mainagent" {
		t.Fatal("original independent C2 role changed", c.AgentKey)
	}
	ctx, owners, bound, err := s.operatorConversationContext(t.Context(), c)
	if err != nil || !bound || len(owners) != 0 || ctx.Value(operatorTaskChatKey{}) != nil {
		t.Fatal(owners, bound, err)
	}
	if err = s.checkC2TaskBinding(ctx, sid); err != nil {
		t.Fatal(err)
	}
	if err = s.checkC2TaskBinding(ctx, other); err == nil {
		t.Fatal("C2 chat escaped its bound session")
	}
}
