package server

import (
	"context"
	"fmt"
	"testing"
	"time"
)

func TestExecutionOutcomeRetryAndOrphanRecovery(t *testing.T) {
	m := proxyTestManager(t)
	task, err := m.CreateTask("recovery fixture", "fixture", nil, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _, _ = m.DeleteTask(task.ID, DeleteTaskOptions{}) }()
	e := NewEngine(m)
	create := func() (int64, string) {
		id, err := task.Store.AddIntent(map[string]any{"summary": "fixture"}, 5, nil, "test")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := task.Store.ClaimIntent(id, "worker"); err != nil {
			t.Fatal(err)
		}
		token, err := task.Store.BindIntentRun(t.Context(), id)
		if err != nil {
			t.Fatal(err)
		}
		return id, token
	}
	id, token := create()
	name := fmt.Sprintf("outcome_reject_%d", time.Now().UnixNano())
	_, err = m.pg.Exec(`CREATE FUNCTION ` + name + `() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN IF NEW.id=` + fmt.Sprint(id) + ` AND NEW.state='done' THEN RAISE EXCEPTION 'fixture'; END IF; RETURN NEW; END $$`)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = m.pg.Exec(`DROP FUNCTION IF EXISTS ` + name + `()`) })
	_, err = m.pg.Exec(`CREATE TRIGGER ` + name + ` BEFORE UPDATE ON exploration_nodes FOR EACH ROW EXECUTE FUNCTION ` + name + `()`)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = m.pg.Exec(`DROP TRIGGER IF EXISTS ` + name + ` ON exploration_nodes`) })
	out := &pendingWorkerOutcome{taskID: task.ID, token: token, state: "done", code: "completed", message: "saved fixture", worker: "worker"}
	e.outcomes.Store(id, out)
	e.reconcileWorkerOutcomes(t.Context(), task)
	if _, ok := e.outcomes.Load(id); !ok {
		t.Fatal("rejected outcome was lost")
	}
	if node, _ := task.Store.GetNode(id); node.State != "running" {
		t.Fatal("failed save published terminal state")
	}
	_, err = m.pg.Exec(`DROP TRIGGER ` + name + ` ON exploration_nodes`)
	if err != nil {
		t.Fatal(err)
	}
	out.mu.Lock()
	out.next = time.Time{}
	out.mu.Unlock()
	e.reconcileWorkerOutcomes(t.Context(), task)
	if _, ok := e.outcomes.Load(id); ok {
		t.Fatal("committed outcome retained")
	}
	if node, _ := task.Store.GetNode(id); node.State != "done" {
		t.Fatal("retry did not persist completion")
	}
	orphan, _ := create()
	live, _ := create()
	_, cancel := context.WithCancelCause(t.Context())
	e.registerWork(live, cancel)
	defer func() { _, finish := e.detachWork(live); finish(nil) }()
	e.reconcileWorkerOutcomesAt(t.Context(), task, time.Now().Add(3*time.Minute))
	if node, _ := task.Store.GetNode(orphan); node.State != "blocked" || node.BlockedReason != "execution_state_lost" {
		t.Fatal("orphan still running", node)
	}
	if node, _ := task.Store.GetNode(live); node.State != "running" {
		t.Fatal("live worker was parked")
	}
}
