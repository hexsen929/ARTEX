package db

import (
	"encoding/json"
	"fmt"
	"testing"
	"time"
)

func TestExecutionGoalProofRejectsInferenceAndCommitsAtomically(t *testing.T) {
	d, err := Open(testDSN(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.Close() })
	exp, err := d.CreateExploration("goal proof fixture", "fixture")
	if err != nil {
		t.Fatal(err)
	}
	defer d.Exec(`DELETE FROM explorations WHERE id=$1`, exp)
	s := d.Exploration(exp)
	goal, err := s.AddGoal(map[string]any{"text": "observable fixture"}, "test")
	if err != nil {
		t.Fatal(err)
	}
	fact, err := s.AddNode(KindFact, map[string]any{"summary": "fixture", "confidence": "inferred"}, 5, "confirmed", "test", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.CommitGoalProof(goal, fact, "hypothesis only"); err == nil {
		t.Fatal("inference proved goal")
	}
	if _, err := d.Exec(`UPDATE exploration_nodes SET payload=jsonb_set(payload,'{confidence}','"observed"') WHERE id=$1`, fact); err != nil {
		t.Fatal(err)
	}
	name := fmt.Sprintf("proof_reject_%d", time.Now().UnixNano())
	if _, err := d.Exec(`CREATE FUNCTION ` + name + `() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN IF NEW.id=` + fmt.Sprint(goal) + ` AND NEW.state='met' THEN RAISE EXCEPTION 'fixture'; END IF; RETURN NEW; END $$`); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = d.Exec(`DROP FUNCTION IF EXISTS ` + name + `()`) })
	if _, err := d.Exec(`CREATE TRIGGER ` + name + ` BEFORE UPDATE ON exploration_nodes FOR EACH ROW EXECUTE FUNCTION ` + name + `()`); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = d.Exec(`DROP TRIGGER IF EXISTS ` + name + ` ON exploration_nodes`) })
	if err := s.CommitGoalProof(goal, fact, "fixture observed"); err == nil {
		t.Fatal("rejected write reported success")
	}
	var edges int
	if err := d.QueryRow(`SELECT count(*) FROM exploration_edges WHERE exploration_id=$1 AND rel=$2`, exp, RelProves).Scan(&edges); err != nil || edges != 0 {
		t.Fatal("failed proof left an edge", edges, err)
	}
	if _, err := d.Exec(`DROP TRIGGER ` + name + ` ON exploration_nodes`); err != nil {
		t.Fatal(err)
	}
	if err := s.CommitGoalProof(goal, fact, "fixture observed"); err != nil {
		t.Fatal(err)
	}
	node, err := s.GetNode(goal)
	if err != nil || node.State != "met" {
		t.Fatal(node, err)
	}
	var payload map[string]any
	_ = json.Unmarshal(node.Payload, &payload)
	if payload["proof"] == nil {
		t.Fatal("proof basis missing")
	}
}
