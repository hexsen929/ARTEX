//go:build ignore

package db

import (
	"context"
	"database/sql"
	"encoding/json"
	"os"
	"reflect"
	"testing"
	"time"
)

// Use the existing test schema without Open's migrations or shared seed writes.
func p1EngagementFixture(t *testing.T) (*DB, *Task, *ExplorationStore) {
	t.Helper()
	dsn := os.Getenv("ARTEX_PG_DSN")
	if dsn == "" {
		t.Skip("ARTEX_PG_DSN is required for this isolated integration test")
	}
	conn, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	d := &DB{conn}
	t.Cleanup(func() { _ = d.Close() })
	if err := d.PingContext(t.Context()); err != nil {
		t.Fatal(err)
	}
	task, err := d.CreateTask(t.Name(), "original contract", nil, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if _, err := d.ExecContext(ctx, `DELETE FROM tasks WHERE id=$1`, task.ID); err != nil {
			t.Errorf("delete fixture task: %v", err)
			return
		}
		if _, err := d.ExecContext(ctx, `DELETE FROM explorations WHERE id=$1`, task.ExplorationID); err != nil {
			t.Errorf("delete fixture exploration: %v", err)
		}
	})
	if err := d.SetPaused(task.ID, true); err != nil {
		t.Fatal(err)
	}
	return d, task, d.Exploration(task.ExplorationID)
}

func p1OldIntent(t *testing.T, es *ExplorationStore, state, target string) (int64, Checkpoint) {
	t.Helper()
	cp := Checkpoint{Result: "partial", Scope: []string{target}, Evidence: "saved observation", Gaps: []string{"unfinished check"}}
	check := CheckSpec{Target: target, Mechanism: state}
	id, err := es.AddNode(KindIntent, map[string]any{
		"summary": "old work", "check": check, "check_key": check.Key(), "check_attempt": 1, "checkpoint": cp,
	}, 1, state, "planner", nil)
	if err != nil {
		t.Fatal(err)
	}
	return id, cp
}

func p1AssertRetired(t *testing.T, es *ExplorationStore, id int64, want Checkpoint) {
	t.Helper()
	node, err := es.GetNode(id)
	if err != nil || node == nil {
		t.Fatalf("read old intent: node=%v err=%v", node, err)
	}
	var saved struct {
		Retired    bool       `json:"retired_contract"`
		Reason     string     `json:"retired_reason"`
		Checkpoint Checkpoint `json:"checkpoint"`
	}
	if err := json.Unmarshal(node.Payload, &saved); err != nil {
		t.Fatal(err)
	}
	if node.State != "stopped" || !saved.Retired || saved.Reason == "" || !reflect.DeepEqual(saved.Checkpoint, want) {
		t.Fatalf("old work not retired with checkpoint preserved: state=%s payload=%s", node.State, node.Payload)
	}
	if reopened, err := es.ReopenIntent(id); err != nil || reopened {
		t.Fatalf("retired work reopened: changed=%v err=%v", reopened, err)
	}
	if claimed, err := es.ClaimIntent(id, "test-worker"); err != nil || claimed {
		t.Fatalf("retired work claimed: changed=%v err=%v", claimed, err)
	}
	if _, err := es.AddCheckIntent(t.Context(), map[string]any{"summary": "retry old target"}, 1, nil, "planner", CheckSpec{}, id, "user resumed"); err == nil {
		t.Fatal("retired check admitted through retry_of")
	}
}

func TestP1EngagementRetiresAllResumableWork(t *testing.T) {
	d, task, es := p1EngagementFixture(t)
	states := []string{"open", "paused", "blocked", "exhausted", "stopped"}
	ids := make([]int64, len(states))
	checkpoints := make([]Checkpoint, len(states))
	for i, state := range states {
		ids[i], checkpoints[i] = p1OldIntent(t, es, state, "https://old.scope.invalid/"+state)
	}
	done, _ := p1OldIntent(t, es, "done", "https://old.scope.invalid/done")
	if err := d.UpdateEngagement(t.Context(), task.ID, EngagementChange{
		Goal: "replacement contract", Reason: "target removed", Goals: []EngagementGoal{{Text: "inspect new target", GoalType: "inspection", RequiredChecks: []string{"new target"}}},
	}); err != nil {
		t.Fatal(err)
	}
	// Recreate the handle so assertions do not rely on in-memory retirement.
	es = d.Exploration(task.ExplorationID)
	for i, state := range states {
		t.Run(state, func(t *testing.T) { p1AssertRetired(t, es, ids[i], checkpoints[i]) })
	}
	node, err := es.GetNode(done)
	if err != nil || node == nil || node.State != "done" {
		t.Fatalf("completed work changed: node=%v err=%v", node, err)
	}
}

func TestP1ScopeEditRetiresOldWork(t *testing.T) {
	for _, edit := range []string{"delete", "add"} {
		t.Run(edit, func(t *testing.T) {
			d, task, es := p1EngagementFixture(t)
			as := d.Assets()
			const oldDomain = "old.scope.invalid"
			if _, err := as.AddAgentScope(task.ID, "subdomain", oldDomain, "original scope", "manual"); err != nil {
				t.Fatal(err)
			}
			var scopeID int64
			if err := d.QueryRowContext(t.Context(), `SELECT id FROM task_scope WHERE task_id=$1 AND domain=$2`, task.ID, oldDomain).Scan(&scopeID); err != nil {
				t.Fatal(err)
			}
			states := []string{"open", "paused", "blocked", "exhausted", "stopped"}
			ids := make([]int64, len(states))
			checkpoints := make([]Checkpoint, len(states))
			for i, state := range states {
				ids[i], checkpoints[i] = p1OldIntent(t, es, state, "https://"+oldDomain+"/"+state)
			}
			var err error
			if edit == "delete" {
				_, err = as.EditManualTaskScope(t.Context(), task.ID, scopeID, "", "", "remove old target")
			} else {
				_, err = as.EditManualTaskScope(t.Context(), task.ID, 0, "subdomain", "new.scope.invalid", "new scope contract")
			}
			if err != nil {
				t.Fatal(err)
			}
			if err := d.SetPaused(task.ID, false); err != nil {
				t.Fatal(err)
			}
			es = d.Exploration(task.ExplorationID)
			frontier, err := es.Frontier(20)
			if err != nil {
				t.Fatal(err)
			}
			// Exercise the claim -> AddAutoScope path used at Worker startup.
			for _, intent := range frontier {
				claimed, err := es.ClaimIntent(intent.ID, "resumed-worker")
				if err != nil {
					t.Fatal(err)
				}
				if claimed {
					if err := as.AddAutoScope(task.ID, "subdomain", oldDomain, "", ""); err != nil {
						t.Fatal(err)
					}
				}
			}
			if edit == "delete" {
				var restored bool
				if err := d.QueryRowContext(t.Context(), `SELECT EXISTS(SELECT 1 FROM task_scope WHERE task_id=$1 AND domain=$2)`, task.ID, oldDomain).Scan(&restored); err != nil {
					t.Fatal(err)
				}
				if restored {
					t.Fatal("resuming old work restored the scope removed by the user")
				}
			}
			for i := range states {
				p1AssertRetired(t, es, ids[i], checkpoints[i])
			}
			if len(frontier) != 0 {
				t.Fatalf("old work survived on the frontier: %+v", frontier)
			}
			if _, err := es.AddCheckIntent(t.Context(), map[string]any{"summary": "fresh plan"}, 1, nil, "planner",
				CheckSpec{Target: "https://new.scope.invalid/", Mechanism: "inspection"}, 0, ""); err != nil {
				t.Fatalf("fresh planning blocked by retirement: %v", err)
			}
		})
	}
}

func TestP1EngagementRetirementRollsBackWithInvalidScope(t *testing.T) {
	d, task, es := p1EngagementFixture(t)
	id, _ := p1OldIntent(t, es, "stopped", "https://old.scope.invalid/")
	if err := d.UpdateEngagement(t.Context(), task.ID, EngagementChange{
		Goal: "replacement", Reason: "invalid replacement", Goals: []EngagementGoal{{Text: "new check", GoalType: "inspection", RequiredChecks: []string{"new target"}}},
		Scopes: []ScopeInput{{Kind: "invalid", Value: "new.scope.invalid"}},
	}); err == nil {
		t.Fatal("invalid scope accepted")
	}
	node, err := es.GetNode(id)
	if err != nil || node == nil {
		t.Fatalf("read original work: node=%v err=%v", node, err)
	}
	var payload map[string]any
	if err := json.Unmarshal(node.Payload, &payload); err != nil {
		t.Fatal(err)
	}
	if _, retired := payload["retired_contract"]; retired || node.State != "stopped" {
		t.Fatalf("failed edit changed original work: %s", node.Payload)
	}
	if reopened, err := es.ReopenIntent(id); err != nil || !reopened {
		t.Fatalf("failed edit made original work unavailable: changed=%v err=%v", reopened, err)
	}
}
