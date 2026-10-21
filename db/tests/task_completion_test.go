//go:build ignore

package db

import (
	"encoding/json"
	"errors"
	"fmt"
	"testing"
	"time"
)

func completionFixture(t *testing.T) (*DB, *Task, *ExplorationStore) {
	t.Helper()
	d, _, _ := testSetup(t)
	t.Cleanup(func() { _ = d.Close() })
	task, err := d.CreateTask("completion test", "verify all necessary work", nil, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.DeleteTask(task.ID) })
	return d, task, d.Exploration(task.ExplorationID)
}

func TestCompletionRejectsEveryUnfinishedState(t *testing.T) {
	for _, state := range []string{"open", "running", "paused", "blocked", "exhausted", "stopped", "deleted"} {
		t.Run(state, func(t *testing.T) {
			d, task, es := completionFixture(t)
			_, err := es.AddNode(KindIntent, map[string]any{"summary": "required"}, 0, state, "planner", nil)
			if err != nil {
				t.Fatal(err)
			}
			if _, err = d.BeginTaskCompletion(t.Context(), task.ID, "goals done", nil, time.Now().Add(time.Minute), false); err == nil {
				t.Fatal("unfinished required work accepted")
			}
			if c, _ := d.TaskCompletion(t.Context(), task.ID); c != nil {
				t.Fatalf("rejected request installed barrier: %+v", c)
			}
		})
	}
}

func TestCompletionPreservesCheckpointAndStopReason(t *testing.T) {
	d, task, es := completionFixture(t)
	intent, err := es.AddNode(KindIntent, map[string]any{"summary": "duplicate check"}, 0, "running", "planner", nil)
	if err != nil {
		t.Fatal(err)
	}
	fact, err := es.AddNode(KindFact, map[string]any{"summary": "existing evidence"}, 0, "confirmed", "worker", nil)
	if err != nil {
		t.Fatal(err)
	}
	decision := []CompletionDisposition{{IntentID: intent, Kind: "duplicate", Reason: "covered by saved evidence", ReferenceID: fact}}
	c, err := d.BeginTaskCompletion(t.Context(), task.ID, "complete", decision, time.Now().Add(time.Minute), false)
	if err != nil {
		t.Fatal(err)
	}
	if err := es.SetIntentState(intent, "exhausted"); err != nil {
		t.Fatal(err)
	}
	if _, err := es.AddNode(KindFact, map[string]any{"summary": "checkpoint result"}, 0, "confirmed", "worker", nil); err != nil {
		t.Fatal(err)
	}
	c, err = d.ReviewTaskCompletion(t.Context(), task.ID, c.AttemptID)
	if err != nil {
		t.Fatal(err)
	}
	var snapshot CompletionInput
	if err = json.Unmarshal(c.Snapshot, &snapshot); err != nil || len(snapshot.Work) != 1 || snapshot.Work[0].IntentID != intent || snapshot.Work[0].State != "running" {
		t.Fatalf("recovery snapshot: %s err=%v", c.Snapshot, err)
	}
	if err = d.FinishTaskCompletion(t.Context(), task.ID, c.AttemptID, c.InputDigest, "done", "verified saved result"); err != nil {
		t.Fatal(err)
	}
	node, _ := es.GetNode(intent)
	if node.State != "stopped" {
		t.Fatalf("waived work must not become done: %+v", node)
	}
	var payload map[string]any
	_ = json.Unmarshal(node.Payload, &payload)
	if payload["completion_stop_reason"] != "covered by saved evidence" {
		t.Fatalf("missing stop provenance: %s", node.Payload)
	}
	stored, _ := d.GetTask(task.ID)
	if stored.Status != "done" {
		t.Fatalf("task not committed: %+v", stored)
	}
}

func TestCompletionRejectsStaleReviewAndPause(t *testing.T) {
	for _, change := range []string{"new intent", "new goal", "pause", "source evidence", "scope asset", "scope expansion", "constraint added", "constraint edited", "constraint deleted"} {
		t.Run(change, func(t *testing.T) {
			d, task, es := completionFixture(t)
			var source *Task
			var sourceFact, assetID int64
			var constraintID int64
			if change == "constraint edited" || change == "constraint deleted" {
				var err error
				constraintID, err = es.AddConstraint("deny", "Only inspect the public login page", "human")
				if err != nil {
					t.Fatal(err)
				}
			}
			if change == "source evidence" {
				var err error
				source, err = d.CreateTask("source", "context", nil, 0, 0)
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = d.DeleteTask(source.ID) })
				if _, err = d.Exec(`INSERT INTO task_relations(task_id,source_task_id) VALUES($1,$2)`, task.ID, source.ID); err != nil {
					t.Fatal(err)
				}
				sourceFact, err = d.Exploration(source.ExplorationID).AddNode(KindFact, map[string]any{"summary": "before"}, 0, "confirmed", "worker", nil)
				if err != nil {
					t.Fatal(err)
				}
			}
			if change == "scope asset" {
				var err error
				assetID, err = d.Assets().UpsertApp(UpsertAppReq{Name: fmt.Sprintf("completion-%d", task.ID)})
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { deleteAsset(d, assetID) })
				_, err = es.AddNode(KindFact, map[string]any{"summary": "anchored"}, 0, "confirmed", "worker", []int64{assetID})
				if err != nil {
					t.Fatal(err)
				}
			}
			c, err := d.BeginTaskCompletion(t.Context(), task.ID, "done", nil, time.Now().Add(time.Minute), false)
			if err != nil {
				t.Fatal(err)
			}
			c, err = d.ReviewTaskCompletion(t.Context(), task.ID, c.AttemptID)
			if err != nil {
				t.Fatal(err)
			}
			if constraintID > 0 {
				var input CompletionInput
				if err := json.Unmarshal(c.Snapshot, &input); err != nil {
					t.Fatal(err)
				}
				var snapshot struct {
					Constraints []Constraint `json:"constraints"`
				}
				if err := json.Unmarshal(input.Data, &snapshot); err != nil || len(snapshot.Constraints) != 1 || snapshot.Constraints[0].ID != constraintID {
					t.Fatalf("review is missing the operation constraint: %s error=%v", input.Data, err)
				}
			}
			switch change {
			case "new intent":
				_, err = es.AddIntent(map[string]any{"summary": "new required work"}, 0, nil, "user")
			case "new goal":
				_, err = es.AddGoal(map[string]any{"summary": "new goal"}, "user")
			case "pause":
				err = d.SetPaused(task.ID, true)
			case "source evidence":
				_, err = d.Exec(`UPDATE exploration_nodes SET payload='{"summary":"changed"}' WHERE id=$1`, sourceFact)
			case "scope asset":
				_, err = d.Exec(`UPDATE assets SET app_description='changed' WHERE id=$1`, assetID)
			case "scope expansion":
				_, err = d.Exec(`INSERT INTO task_scope(task_id,kind,domain) VALUES($1,'root_domain','new.example')`, task.ID)
			case "constraint added":
				_, err = es.AddConstraint("allow", "Inspect authenticated endpoints too", "human")
			case "constraint edited":
				err = es.UpdateConstraint(constraintID, "allow", "Inspect authenticated endpoints too")
			case "constraint deleted":
				err = es.DeleteConstraint(constraintID)
			}
			if err != nil {
				t.Fatal(err)
			}
			if err = d.FinishTaskCompletion(t.Context(), task.ID, c.AttemptID, c.InputDigest, "done", "old review"); !errors.Is(err, ErrCompletionChanged) {
				t.Fatalf("stale review error=%v", err)
			}
			stored, _ := d.GetTask(task.ID)
			if stored.Status == "done" {
				t.Fatal("stale decision committed")
			}
		})
	}
}

func TestCompletionRecoveryAndTimeoutRemainHonest(t *testing.T) {
	d, task, es := completionFixture(t)
	id, err := es.AddNode(KindIntent, map[string]any{"summary": "still necessary"}, 0, "running", "planner", nil)
	if err != nil {
		t.Fatal(err)
	}
	c, err := d.BeginTaskCompletion(t.Context(), task.ID, "timeout", nil, time.Now().Add(time.Minute), true)
	if err != nil {
		t.Fatal(err)
	}
	if err = es.SetIntentState(id, "exhausted"); err != nil {
		t.Fatal(err)
	}
	if err = d.RecoverTaskCompletion(t.Context(), task.ID); err != nil {
		t.Fatal(err)
	}
	node, _ := es.GetNode(id)
	if node.State != "open" {
		t.Fatalf("recovery stranded work: %s", node.State)
	}
	c, err = d.BeginTaskCompletion(t.Context(), task.ID, "timeout", nil, time.Now().Add(time.Minute), true)
	if err != nil {
		t.Fatal(err)
	}
	c, err = d.ReviewTaskCompletion(t.Context(), task.ID, c.AttemptID)
	if err != nil {
		t.Fatal(err)
	}
	if err = d.FinishTaskCompletion(t.Context(), task.ID, c.AttemptID, c.InputDigest, "done", "cannot claim"); !errors.Is(err, ErrCompletionChanged) {
		t.Fatalf("unfinished work allowed done: %v", err)
	}
	if err = d.FinishTaskCompletion(t.Context(), task.ID, c.AttemptID, c.InputDigest, "timeout", "necessary check not completed"); err != nil {
		t.Fatal(err)
	}
	stored, _ := d.GetTask(task.ID)
	if stored.Status != "timeout" {
		t.Fatal("timeout missing")
	}
}

func TestCompletionTimeoutIncludesNewWorkAndReasons(t *testing.T) {
	d, task, es := completionFixture(t)
	c, err := d.BeginTaskCompletion(t.Context(), task.ID, "timeout", nil, time.Now().Add(time.Minute), true)
	if err != nil {
		t.Fatal(err)
	}
	id, err := es.AddIntent(map[string]any{"summary": "late necessary check"}, 0, nil, "user")
	if err != nil {
		t.Fatal(err)
	}
	c, err = d.ReviewTaskCompletion(t.Context(), task.ID, c.AttemptID)
	if err != nil {
		t.Fatal(err)
	}
	if err = d.FinishTaskCompletion(t.Context(), task.ID, c.AttemptID, c.InputDigest, "timeout", "unfinished"); err != nil {
		t.Fatal(err)
	}
	c, err = d.TaskCompletion(t.Context(), task.ID)
	if err != nil {
		t.Fatal(err)
	}
	var input CompletionInput
	if err = json.Unmarshal(c.Snapshot, &input); err != nil {
		t.Fatal(err)
	}
	if len(input.Work) != 1 || input.Work[0].IntentID != id || input.Work[0].Reason == "" {
		t.Fatalf("new unfinished work omitted from report: %s", c.Snapshot)
	}
}
