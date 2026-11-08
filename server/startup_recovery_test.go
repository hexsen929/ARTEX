package server

import (
	"fmt"
	"testing"
	"time"
)

func TestExecutionStartupRecoveryFailureBlocksAllStartPaths(t *testing.T) {
	t.Run("pause_saved", func(t *testing.T) { testStartupRecoveryFailure(t, false) })
	t.Run("pause_write_failed", func(t *testing.T) { testStartupRecoveryFailure(t, true) })
}

func testStartupRecoveryFailure(t *testing.T, rejectPause bool) {
	m := proxyTestManager(t)
	task, err := m.CreateTask("startup recovery guard", "fixture", nil, 3600, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = m.DeleteTask(task.ID, DeleteTaskOptions{}) })
	id, err := task.Store.AddIntent(map[string]any{"summary": "fixture"}, 5, nil, "test")
	if err != nil {
		t.Fatal(err)
	}
	if ok, err := task.Store.ClaimIntent(id, "worker"); err != nil || !ok {
		t.Fatal(ok, err)
	}
	token, err := task.Store.BindIntentRun(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	s := newAdmissionTestServer(m, nil)
	t.Cleanup(func() {
		for _, item := range m.List() {
			s.engine.StopTask(item.ID)
		}
	})
	if err := s.engine.journalWorkerOutcome(id, &pendingWorkerOutcome{taskID: task.ID, token: token, state: "done", code: "completed", message: "already executed", worker: "worker"}); err != nil {
		t.Fatal(err)
	}
	name := fmt.Sprintf("startup_outcome_reject_%d", time.Now().UnixNano())
	if _, err := m.pg.Exec(`CREATE FUNCTION ` + name + `() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN IF NEW.id=` + fmt.Sprint(id) + ` AND NEW.state='done' THEN RAISE EXCEPTION 'fixture'; END IF; RETURN NEW; END $$`); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = m.pg.Exec(`DROP FUNCTION IF EXISTS ` + name + `()`) })
	if _, err := m.pg.Exec(`CREATE TRIGGER ` + name + ` BEFORE UPDATE ON exploration_nodes FOR EACH ROW EXECUTE FUNCTION ` + name + `()`); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = m.pg.Exec(`DROP TRIGGER IF EXISTS ` + name + ` ON exploration_nodes`) })
	if rejectPause {
		pauseName := name + "_pause"
		if _, err := m.pg.Exec(`CREATE FUNCTION ` + pauseName + `() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN IF NEW.id=` + task.ID + ` AND NEW.paused THEN RAISE EXCEPTION 'pause fixture'; END IF; RETURN NEW; END $$`); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _, _ = m.pg.Exec(`DROP FUNCTION IF EXISTS ` + pauseName + `()`) })
		if _, err := m.pg.Exec(`CREATE TRIGGER ` + pauseName + ` BEFORE UPDATE ON tasks FOR EACH ROW EXECUTE FUNCTION ` + pauseName + `()`); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _, _ = m.pg.Exec(`DROP TRIGGER IF EXISTS ` + pauseName + ` ON tasks`) })
	}
	// A fresh process has no cached task handle. LoadExisting intentionally
	// skips handles already present in the manager, so discard only this fixture.
	m.mu.Lock()
	delete(m.tasks, task.ID)
	m.mu.Unlock()
	s.restoreTaskRuntimes()
	restored, ok := m.Task(task.ID)
	if !ok || restored == task {
		t.Fatal("task was not reloaded from persistent state")
	}
	if !s.engine.OutcomeRecoveryBlocked(task.ID) || s.engine.Started(task.ID) || s.engine.ReadyFor(restored) {
		t.Fatal("second startup loop bypassed failed recovery")
	}
	s.engine.Resume(restored)
	s.engine.Run(s.ctx, restored)
	s.engine.startDeadlineCoordinator(s.ctx, restored)
	if !s.engine.IsPaused(task.ID) || s.engine.Started(task.ID) {
		t.Fatal("direct startup/resume bypassed failed recovery")
	}
	if _, present := s.engine.coordStarted.Load(task.ID); present {
		t.Fatal("deadline coordinator bypassed failed recovery")
	}
	if err := s.engine.runDetachedIntent(s.ctx, restored, id, "fixture", "fixture", "fixture"); err == nil {
		t.Fatal("side conversation bypassed failed recovery")
	}
	if _, err := s.applyTaskControl(restored, "resume"); err == nil {
		t.Fatal("API reported successful resume during failed recovery")
	}
	if _, err := s.admitTaskWhenScheduled(restored, "resume", true, true); err == nil {
		t.Fatal("calendar admission bypassed failed recovery")
	}
	if _, err := s.admitTaskWhenScheduledWithReason(restored, "resume", true, false, "paused_by_proxy", true); err == nil {
		t.Fatal("proxy recovery bypassed failed recovery")
	}
	if node, _ := restored.Store.GetNode(id); node.State != "running" {
		t.Fatal("uncommitted result was reset for replay")
	}
	if _, err := m.pg.Exec(`DROP TRIGGER ` + name + ` ON exploration_nodes`); err != nil {
		t.Fatal(err)
	}
	if rejectPause {
		if restored.lifecycleSnapshot().Paused {
			t.Fatal("fixture did not reject pause persistence")
		}
		if _, err := m.pg.Exec(`DROP TRIGGER ` + name + `_pause ON tasks`); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.applyTaskControl(restored, "resume"); err != nil {
		t.Fatal("manual resume did not recover saved result", err)
	}
	if s.engine.OutcomeRecoveryBlocked(task.ID) {
		t.Fatal("resolved recovery barrier retained")
	}
	if node, _ := restored.Store.GetNode(id); node.State != "done" {
		t.Fatal("execution was reopened instead of restoring its result")
	}
}
