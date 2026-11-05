package db

import (
	"encoding/json"
	"errors"
	"testing"
)

func TestExecutionClaimGenerationPrecedesWorkerSetup(t *testing.T) {
	d, err := Open(testDSN(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.Close() })
	exp, err := d.CreateExploration("generation transition fixture", "fixture")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = d.Exec(`DELETE FROM explorations WHERE id=$1`, exp) })
	s := d.Exploration(exp)
	id, err := s.AddIntent(map[string]any{"summary": "fixture"}, 5, nil, "test")
	if err != nil {
		t.Fatal(err)
	}
	if ok, err := s.ClaimIntent(id, "same-slot"); err != nil || !ok {
		t.Fatal(ok, err)
	}
	old, err := s.BindIntentRun(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SetIntentOutcome(id, "blocked", "fixture", "fixture", old); err != nil {
		t.Fatal(err)
	}
	if ok, err := s.ReopenIntent(id); err != nil || !ok {
		t.Fatal(ok, err)
	}
	if ok, err := s.ClaimIntent(id, "same-slot"); err != nil || !ok {
		t.Fatal(ok, err)
	}
	// Deliberately do not call BindIntentRun: replay may arrive in this gap.
	if err := s.SetIntentOutcome(id, "done", "completed", "stale", old); !errors.Is(err, ErrIntentStateConflict) {
		t.Fatal("claim retained the old generation", err)
	}
	node, err := s.GetNode(id)
	if err != nil || node.State != "running" {
		t.Fatal(node, err)
	}
	var payload map[string]any
	_ = json.Unmarshal(node.Payload, &payload)
	second := payload["worker_run_token"].(string)
	if second == "" || second == old {
		t.Fatal("claim did not publish a fresh generation")
	}
	if ok, err := s.CompareAndSetIntentState(id, "running", "paused"); err != nil || !ok {
		t.Fatal(ok, err)
	}
	if ok, err := s.CompareAndSetIntentState(id, "paused", "running"); err != nil || !ok {
		t.Fatal(ok, err)
	}
	if err := s.SetIntentOutcome(id, "done", "completed", "stale", second); !errors.Is(err, ErrIntentStateConflict) {
		t.Fatal("conversation resume retained the old generation", err)
	}
}
