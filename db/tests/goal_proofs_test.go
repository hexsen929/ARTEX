//go:build ignore

package db

import (
	"encoding/json"
	"errors"
	"testing"
)

func goalProofFixture(t *testing.T) (*DB, *Task, *ExplorationStore, int64, int64) {
	t.Helper()
	d, task, es := completionFixture(t)
	g, err := es.AddGoal(map[string]any{"text": "obtain the saved flag"}, "human")
	if err != nil {
		t.Fatal(err)
	}
	e, err := es.AddNode(KindFact, map[string]any{"summary": "flag retrieved", "detail": "flag{verified_saved_result}"}, 0, "confirmed", "worker", nil)
	if err != nil {
		t.Fatal(err)
	}
	return d, task, es, g, e
}

func TestGoalProofAtomicAuditAndCorrection(t *testing.T) {
	d, _, es, g, e := goalProofFixture(t)
	snapshot, err := es.GoalProofSnapshot(t.Context(), g, e)
	if err != nil {
		t.Fatal(err)
	}
	if err = es.CommitGoalProof(t.Context(), snapshot, true, "planner", "saved flag matches goal"); err != nil {
		t.Fatal(err)
	}
	n, err := es.GetNode(g)
	if err != nil {
		t.Fatal(err)
	}
	var audit []GoalAudit
	if json.Unmarshal(n.GoalAudit, &audit) != nil || len(audit) != 1 || audit[0].Event != "prove" || audit[0].Reason == "" || n.State != "met" {
		t.Fatalf("missing proof audit: %+v %s", n, n.GoalAudit)
	}
	var count int
	if err = d.QueryRow(`SELECT count(*) FROM exploration_edges WHERE exploration_id=$1 AND src_id=$2 AND dst_id=$3 AND rel='proves'`, es.ID(), e, g).Scan(&count); err != nil || count != 1 {
		t.Fatalf("missing proof edge: %d %v", count, err)
	}
	// Re-submitting unchanged evidence for stop decisions must not invalidate a
	// pending completion snapshot by bumping the goal version again.
	snapshot, err = es.GoalProofSnapshot(t.Context(), g, e)
	if err != nil {
		t.Fatal(err)
	}
	if err = es.CommitGoalProof(t.Context(), snapshot, true, "planner", "same saved result"); err != nil {
		t.Fatal(err)
	}
	again, _ := es.GetNode(g)
	if again.ContentVersion != n.ContentVersion || string(again.GoalAudit) != string(n.GoalAudit) {
		t.Fatal("unchanged proof created state churn")
	}
	if err = es.ReopenGoal(t.Context(), g, n.ContentVersion, "human", "flag belongs to a different objective"); err != nil {
		t.Fatal(err)
	}
	n, _ = es.GetNode(g)
	json.Unmarshal(n.GoalAudit, &audit)
	if n.State != "open" || len(audit) != 2 || audit[1].Event != "reopen" || len(audit[1].PreviousProofs) != 1 {
		t.Fatalf("correction lost original proof: %s", n.GoalAudit)
	}
	if evidence, _ := es.GetNode(e); evidence == nil || evidence.State != "confirmed" {
		t.Fatal("correction removed evidence")
	}
	if err = es.ReopenGoal(t.Context(), g, n.ContentVersion-1, "human", "stale correction"); !errors.Is(err, ErrGoalProofChanged) {
		t.Fatalf("stale correction applied: %v", err)
	}
}

func TestGoalProofRejectsStaleInputAndAtomicFailure(t *testing.T) {
	for _, change := range []string{"goal", "evidence", "pause", "deleted", "edge_failure"} {
		t.Run(change, func(t *testing.T) {
			d, task, es, g, e := goalProofFixture(t)
			snapshot, err := es.GoalProofSnapshot(t.Context(), g, e)
			if err != nil {
				t.Fatal(err)
			}
			switch change {
			case "goal":
				err = es.UpdateGoalPayload(g, "obtain two flags", "")
			case "evidence":
				_, err = d.Exec(`UPDATE exploration_nodes SET payload='{"summary":"not retrieved"}' WHERE id=$1`, e)
			case "pause":
				err = d.SetPaused(task.ID, true)
			case "deleted":
				_, err = d.Exec(`DELETE FROM exploration_nodes WHERE id=$1`, e)
			case "edge_failure":
				_, err = d.Exec(`CREATE FUNCTION test_goal_edge_failure() RETURNS trigger AS $$ BEGIN RAISE EXCEPTION 'injected edge failure'; END $$ LANGUAGE plpgsql; CREATE TRIGGER test_goal_edge_failure BEFORE INSERT ON exploration_edges FOR EACH ROW EXECUTE FUNCTION test_goal_edge_failure()`)
				t.Cleanup(func() {
					_, _ = d.Exec(`DROP TRIGGER IF EXISTS test_goal_edge_failure ON exploration_edges; DROP FUNCTION IF EXISTS test_goal_edge_failure()`)
				})
			}
			if err != nil {
				t.Fatal(err)
			}
			err = es.CommitGoalProof(t.Context(), snapshot, true, "planner", "stale judgment")
			if err == nil {
				t.Fatal("stale or failed transaction committed")
			}
			n, _ := es.GetNode(g)
			if n.State == "met" {
				t.Fatal("failed proof marked goal met")
			}
			var count int
			if err = d.QueryRow(`SELECT count(*) FROM exploration_edges WHERE exploration_id=$1 AND dst_id=$2 AND rel='proves'`, es.ID(), g).Scan(&count); err != nil || count != 0 {
				t.Fatalf("partial proof survived: %d %v", count, err)
			}
		})
	}
}

func TestGoalProofRejectsNegativeAndEditingReopens(t *testing.T) {
	_, _, es, g, e := goalProofFixture(t)
	snapshot, err := es.GoalProofSnapshot(t.Context(), g, e)
	if err != nil {
		t.Fatal(err)
	}
	if err = es.CommitGoalProof(t.Context(), snapshot, false, "planner", "no flag obtained"); err != nil {
		t.Fatal(err)
	}
	n, _ := es.GetNode(g)
	if n.State != "open" {
		t.Fatal("negative review marked met")
	}
	snapshot, err = es.GoalProofSnapshot(t.Context(), g, e)
	if err != nil {
		t.Fatal(err)
	}
	if err = es.CommitGoalProof(t.Context(), snapshot, true, "planner", "saved flag"); err != nil {
		t.Fatal(err)
	}
	if err = es.UpdateGoalPayload(g, "obtain a different flag", ""); err != nil {
		t.Fatal(err)
	}
	n, _ = es.GetNode(g)
	var audit []GoalAudit
	json.Unmarshal(n.GoalAudit, &audit)
	if n.State != "open" || len(audit) != 3 || audit[2].Event != "edit" || len(audit[2].PreviousProofs) != 1 {
		t.Fatalf("edited goal retained old success: %s", n.GoalAudit)
	}
}

func TestGoalProofDirectSourceIsReadOnlyAndRechecked(t *testing.T) {
	d, task, es, g, e := goalProofFixture(t)
	source, err := d.CreateTask("source", "saved evidence", nil, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.DeleteTask(source.ID) })
	se := d.Exploration(source.ExplorationID)
	fact, err := se.AddNode(KindFact, map[string]any{"summary": "source saved flag"}, 0, "confirmed", "worker", nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = d.Exec(`INSERT INTO task_relations(task_id,source_task_id) VALUES($1,$2)`, task.ID, source.ID); err != nil {
		t.Fatal(err)
	}
	snapshot, err := es.GoalProofSnapshot(t.Context(), g, fact)
	if err != nil || !snapshot.Evidence.Inherited {
		t.Fatalf("valid source unavailable: %+v %v", snapshot, err)
	}
	if _, err = d.Exec(`DELETE FROM task_relations WHERE task_id=$1 AND source_task_id=$2`, task.ID, source.ID); err != nil {
		t.Fatal(err)
	}
	if err = es.CommitGoalProof(t.Context(), snapshot, true, "planner", "removed relation"); !errors.Is(err, ErrGoalProofChanged) {
		t.Fatalf("removed source accepted: %v", err)
	}
	foreignGoal, err := se.AddGoal(map[string]any{"text": "source goal"}, "human")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = es.GoalProofSnapshot(t.Context(), foreignGoal, e); !errors.Is(err, ErrGoalProofChanged) {
		t.Fatal("source goal became writable")
	}
}
