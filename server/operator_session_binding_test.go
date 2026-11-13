package server

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/Autumn-27/artex/agent"
	"github.com/Autumn-27/artex/db"
	"github.com/Autumn-27/artex/session"
)

func TestOperatorWebshellConversationControlsOnlyItsBoundSession(t *testing.T) {
	s := operatorFixture(t)
	s.sessStore = db.NewSessionStore(s.m.pg, s.jwtKey)
	s.sessReg = session.NewRegistry()
	task := intranetFixtureTask(t, s.m.pg)
	create := func(taskID int64) int64 {
		id, err := s.sessStore.Create(t.Context(), &db.SessionRecord{Kind: "http_php", URL: "http://fixture.invalid/s.php", Lang: "php", CreatedByTask: taskID})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = s.sessStore.Delete(context.Background(), id) })
		s.sessReg.Add(&taskSessionFixture{id: id})
		return id
	}
	a, b, global := create(task.ID), create(task.ID), create(0)
	ctx := agent.WithRunInfo(t.Context(), agent.RunInfo{TaskID: task.ID})
	bound := context.WithValue(ctx, operatorRemoteBindingKey{}, operatorRemoteBinding{Kind: "webshell", SessionID: a, TaskID: task.ID})
	if _, err := s.lookupTaskSession(bound, a); err != nil {
		t.Fatal(err)
	}
	if _, err := s.lookupTaskSession(bound, b); err == nil {
		t.Fatal("bound chat controlled another session of the same task")
	}
	if _, err := s.lookupTaskSession(ctx, b); err != nil {
		t.Fatal("ordinary expert task lost session access", err)
	}
	if _, err := s.lookupTaskSession(ctx, global); err != nil {
		t.Fatal("live task lost original global-session access", err)
	}
	if _, err := s.lookupTaskSession(t.Context(), global); err == nil {
		t.Fatal("unbound global chat guessed a session ID")
	}
	c := &db.Conversation{ID: time.Now().UnixNano(), AgentKey: "pentest"}
	key := fmt.Sprintf("conversation.webshell.%d", c.ID)
	if err := s.m.pg.SaveOperatorSetting(t.Context(), key, map[string]int64{"session_id": global, "task_id": 0}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = s.m.pg.Exec("DELETE FROM operator_module_settings WHERE key=$1", key) })
	globalCtx, owners, ok, err := s.operatorConversationContext(t.Context(), c)
	if err != nil || !ok || len(owners) != 0 || globalCtx.Value(operatorTaskChatKey{}) != nil {
		t.Fatal("independent chat acquired a fake task", owners, ok, err)
	}
	if _, err = s.lookupTaskSession(globalCtx, global); err != nil {
		t.Fatal(err)
	}
	if _, err = s.lookupTaskSession(globalCtx, a); err == nil {
		t.Fatal("global binding escaped its exact session")
	}
	if err = s.sessStore.Delete(t.Context(), global); err != nil {
		t.Fatal(err)
	}
	if _, err = s.lookupTaskSession(globalCtx, global); err == nil {
		t.Fatal("deleted session revived through stale binding")
	}
}
