//go:build ignore

package db

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestTaskCompletionArchiveRoundTrip(t *testing.T) {
	d, err := Open(testDSN(t))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	if err := d.EnsureLLMRecordsTable(); err != nil {
		t.Fatal(err)
	}
	if err := d.EnsureLLMUsageTable(); err != nil {
		t.Fatal(err)
	}
	for _, phase := range []string{"draining", "reviewing", "completed", "legacy"} {
		t.Run(phase, func(t *testing.T) {
			task, err := d.CreateTaskWithOptions("completion archive", "preserve saved results", TaskCreateOptions{})
			if err != nil {
				t.Fatal(err)
			}
			defer func() {
				_, _ = d.Exec(`DELETE FROM task_archives WHERE task_id=$1`, task.ID)
				_ = d.DeleteTask(task.ID)
			}()
			if err := d.SetPaused(task.ID, true); err != nil {
				t.Fatal(err)
			}
			intentID, err := d.Exploration(task.ExplorationID).AddNode(KindIntent, map[string]any{"summary": "unfinished check"}, 1, "exhausted", "worker", nil)
			if err != nil {
				t.Fatal(err)
			}
			goalID, err := d.Exploration(task.ExplorationID).AddGoal(map[string]any{"text": "preserve reviewed goal"}, "human")
			if err != nil {
				t.Fatal(err)
			}
			if _, err = d.Exec(`UPDATE exploration_nodes SET goal_audit='[{"event":"reopen","reason":"original evidence insufficient","actor":"human"}]' WHERE id=$1`, goalID); err != nil {
				t.Fatal(err)
			}
			snapshotRaw, _ := json.Marshal(CompletionInput{Work: []CompletionWork{{IntentID: intentID, State: "running", Summary: "unfinished check", CompletionDisposition: CompletionDisposition{Kind: "irrelevant", Reason: "outside original goal"}}}, Review: "saved review", Data: json.RawMessage(`{"saved_evidence":"keep me"}`)})
			if phase != "legacy" {
				_, err = d.Exec(`INSERT INTO task_completions(task_id,attempt_id,phase,reason,snapshot,input_digest,started_at,deadline_at,updated_at) VALUES($1,7,$2,'saved reason',$3,'old-authority',now(),now(),now())`, task.ID, phase, string(snapshotRaw))
				if err != nil {
					t.Fatal(err)
				}
			}
			archive, err := d.QueueTaskArchive(task.ID)
			if err != nil {
				t.Fatal(err)
			}
			claimed, err := d.ClaimTaskArchiveJob(t.Context())
			if err != nil || claimed == nil || claimed.ID != archive.ID {
				t.Fatalf("claim=%+v err=%v", claimed, err)
			}
			snapshot, err := d.SnapshotTaskArchive(task.ID)
			if err != nil {
				t.Fatal(err)
			}
			if phase != "legacy" && snapshot.DataCounts["task_completions"] != 1 {
				t.Fatal("completion missing from archive")
			}
			if err := d.CompleteTaskArchive(archive.ID, snapshot, "unused-test-archive", "test", 1, 1); err != nil {
				t.Fatal(err)
			}
			if c, err := d.TaskCompletion(t.Context(), task.ID); err != nil || c != nil {
				t.Fatalf("hot completion remains: %+v %v", c, err)
			}
			if phase == "legacy" {
				delete(snapshot.Tables, "task_completions")
				// Old archives predate the additive goal audit column.
				var legacyNodes []map[string]json.RawMessage
				if err = json.Unmarshal(snapshot.Tables["exploration_nodes"], &legacyNodes); err != nil {
					t.Fatal(err)
				}
				for _, row := range legacyNodes {
					delete(row, "goal_audit")
				}
				snapshot.Tables["exploration_nodes"], err = json.Marshal(legacyNodes)
				if err != nil {
					t.Fatal(err)
				}
			}
			if _, err := d.QueueTaskArchiveRestore(archive.ID); err != nil {
				t.Fatal(err)
			}
			if _, err := d.ClaimTaskArchiveJob(t.Context()); err != nil {
				t.Fatal(err)
			}
			if _, err := d.RestoreTaskArchive(archive.ID, snapshot, 0); err != nil {
				t.Fatal(err)
			}
			c, err := d.TaskCompletion(t.Context(), task.ID)
			if err != nil {
				t.Fatal(err)
			}
			goal, err := d.Exploration(task.ExplorationID).GetNode(goalID)
			if err != nil || goal == nil {
				t.Fatalf("goal missing after restore: %v", err)
			}
			if phase == "legacy" {
				if string(goal.GoalAudit) != "[]" {
					t.Fatalf("legacy goal fabricated audit: %s", goal.GoalAudit)
				}
			} else if !strings.Contains(string(goal.GoalAudit), "original evidence insufficient") {
				t.Fatal("archive lost goal correction audit")
			}
			if phase == "legacy" {
				if c != nil {
					t.Fatalf("legacy archive fabricated completion: %+v", c)
				}
				return
			}
			wantPhase := "interrupted"
			if phase == "completed" {
				wantPhase = "completed"
			}
			if c == nil || c.Phase != wantPhase || c.AttemptID != 8 || c.InputDigest != "" || !strings.Contains(string(c.Snapshot), "keep me") {
				t.Fatalf("unsafe/incomplete restored completion: %+v", c)
			}
			var state string
			if err := d.QueryRow(`SELECT state FROM exploration_nodes WHERE id=$1`, intentID).Scan(&state); err != nil {
				t.Fatal(err)
			}
			if phase != "completed" && state != "open" {
				t.Fatalf("interrupted work not reopened: %s", state)
			}
		})
	}
}
