package db

import (
	"encoding/json"
	"fmt"
	"sync"
	"testing"
	"time"
)

func continuityFixture(t *testing.T) (*DB, *ExplorationStore) {
	t.Helper()
	d, err := Open(testDSN(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.Close() })
	exp, err := d.CreateExploration("check continuity fixture", "fixture")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = d.Exec(`DELETE FROM explorations WHERE id=$1`, exp) })
	return d, d.Exploration(exp)
}

func TestExecutionStableCheckIdentitySurvivesWordingAndFailures(t *testing.T) {
	_, s := continuityFixture(t)
	identity := CheckIdentity{Resource: "fixture/object", Operation: "read", IdentityRef: "account-A", Question: "ownership-check"}
	var wg sync.WaitGroup
	ids := make(chan int64, 12)
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			id, err := s.AddPlannerIntent(map[string]any{"summary": fmt.Sprintf("wording %d", i), "check_identity": identity}, 5, nil, nil, 1)
			if err != nil {
				t.Error(err)
				return
			}
			ids <- id
		}(i)
	}
	wg.Wait()
	close(ids)
	var id int64
	for next := range ids {
		if id != 0 && next != id {
			t.Fatal("paraphrase created a second execution", id, next)
		}
		id = next
	}
	if id == 0 {
		t.Fatal("no check created")
	}
	if _, err := s.ClaimIntent(id, "worker"); err != nil {
		t.Fatal(err)
	}
	token, err := s.BindIntentRun(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SetIntentOutcome(id, "blocked", "transport_failure", "fixture", token); err != nil {
		t.Fatal(err)
	}
	reused, err := s.AddPlannerIntent(map[string]any{"summary": "eighth restart wording", "check_identity": identity}, 5, nil, nil, 1)
	if err != nil || reused != id {
		t.Fatal("blocked check duplicated", reused, err)
	}
	if _, err := s.ContinuePlannerIntent(id, "provider might work", 1); err == nil {
		t.Fatal("planner revived infrastructure failure")
	}
	node, err := s.GetNode(id)
	if err != nil || node.State != "blocked" {
		t.Fatal(node, err)
	}
	identity.IdentityRef = "account-B"
	other, err := s.AddPlannerIntent(map[string]any{"summary": "same question, distinct identity", "check_identity": identity}, 5, nil, nil, 1)
	if err != nil || other == id {
		t.Fatal("different identity incorrectly merged", other, err)
	}
	// Existing checks remain queryable even when the frontier is full.
	reused, err = s.AddPlannerIntent(map[string]any{"summary": "same open check", "check_identity": identity}, 5, nil, nil, 1)
	if err != nil || reused != other {
		t.Fatal("full frontier blocked reuse", reused, err)
	}
}

func TestExecutionCheckpointAtomicityAndContinuation(t *testing.T) {
	d, s := continuityFixture(t)
	id, err := s.AddIntent(map[string]any{"summary": "fixture"}, 5, nil, "test")
	if err != nil {
		t.Fatal(err)
	}
	start := func() string {
		t.Helper()
		if ok, err := s.ClaimIntent(id, "worker"); err != nil || !ok {
			t.Fatal(ok, err)
		}
		token, err := s.BindIntentRun(t.Context(), id)
		if err != nil {
			t.Fatal(err)
		}
		return token
	}
	token := start()
	event := func(kind, call, generation string) (int64, error) {
		metadata, _ := json.Marshal(map[string]any{"worker_run_token": generation})
		return s.AppendActivity(Activity{NodeID: &id, Kind: kind, Tool: "fixture", ToolUseID: call, Metadata: metadata})
	}
	read := func() ExecutionCheckpoint {
		t.Helper()
		n, err := d.Exploration(s.ID()).GetNode(id)
		if err != nil {
			t.Fatal(err)
		}
		var p struct {
			Checkpoint ExecutionCheckpoint `json:"execution_checkpoint"`
		}
		if err := json.Unmarshal(n.Payload, &p); err != nil {
			t.Fatal(err)
		}
		return p.Checkpoint
	}
	first, err := event("tool_use", "a", token)
	if err != nil {
		t.Fatal(err)
	}
	duplicate, err := event("tool_use", "a", token)
	if err != nil || duplicate != first {
		t.Fatal("retry duplicated activity", duplicate, err)
	}
	if _, err := event("tool_use", "b", token); err != nil {
		t.Fatal(err)
	}
	if _, err := event("tool_result", "a", token); err != nil {
		t.Fatal(err)
	}
	cp := read()
	if cp.ToolCalls != 2 || cp.ToolResults != 1 || len(cp.Pending) != 1 || cp.Pending["b"] != "fixture" {
		t.Fatal(cp)
	}
	// Failing the checkpoint UPDATE must also roll back its activity INSERT.
	name := fmt.Sprintf("checkpoint_reject_%d", time.Now().UnixNano())
	if _, err := d.Exec(`CREATE FUNCTION ` + name + `() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN IF NEW.id=` + fmt.Sprint(id) + ` THEN RAISE EXCEPTION 'fixture'; END IF; RETURN NEW; END $$`); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = d.Exec(`DROP FUNCTION IF EXISTS ` + name + `()`) })
	if _, err := d.Exec(`CREATE TRIGGER ` + name + ` BEFORE UPDATE ON exploration_nodes FOR EACH ROW EXECUTE FUNCTION ` + name + `()`); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = d.Exec(`DROP TRIGGER IF EXISTS ` + name + ` ON exploration_nodes`) })
	if _, err := event("tool_result", "b", token); err == nil {
		t.Fatal("fixture did not reject checkpoint write")
	}
	var count int
	if err := d.QueryRow(`SELECT count(*) FROM activity WHERE node_id=$1 AND kind='tool_result' AND tool_use_id='b'`, id).Scan(&count); err != nil || count != 0 {
		t.Fatal("partial activity survived rollback", count, err)
	}
	if _, err := d.Exec(`DROP TRIGGER ` + name + ` ON exploration_nodes`); err != nil {
		t.Fatal(err)
	}
	if err := s.SetIntentOutcome(id, "exhausted", "run_budget", "fixture", token); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ContinuePlannerIntent(id, "next step", 8); err == nil {
		t.Fatal("unknown tool result automatically resumed")
	}
	if ok, err := s.ReopenIntent(id); err != nil || !ok {
		t.Fatal(ok, err)
	}
	newToken := start()
	if _, err := event("tool_result", "b", token); err != nil {
		t.Fatal(err)
	}
	cp = read()
	if cp.RunToken != newToken || cp.ToolCalls != 0 || cp.ToolResults != 0 {
		t.Fatal("late result changed new run", cp)
	}
	for i := 0; i < 2; i++ {
		if err := s.SetIntentOutcome(id, "exhausted", "turn_budget", "fixture", newToken); err != nil {
			t.Fatal(err)
		}
		continued, err := s.ContinuePlannerIntent(id, "reuse fixture and finish next step", 8)
		if err != nil || continued != id {
			t.Fatal("continuation lost identity", continued, err)
		}
		if again, err := s.ContinuePlannerIntent(id, "duplicate request", 8); err != nil || again != id {
			t.Fatal(again, err)
		}
		newToken = start()
	}
	if err := s.SetIntentOutcome(id, "exhausted", "turn_budget", "fixture", newToken); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ContinuePlannerIntent(id, "third automatic restart", 8); err == nil {
		t.Fatal("continuation limit bypassed")
	}
}
