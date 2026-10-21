//go:build ignore

package db

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"
)

func TestCompletionRetryPersistsAndCannotBeBypassed(t *testing.T) {
	d, task, es := completionFixture(t)
	intent, err := es.AddNode(KindIntent, map[string]any{"summary": "same pending work"}, 0, "running", "planner", nil)
	if err != nil {
		t.Fatal(err)
	}
	decisions := []CompletionDisposition{{IntentID: intent, Kind: "irrelevant", Reason: "outside scope"}}
	for count := 1; count <= 3; count++ {
		c, err := d.BeginTaskCompletion(t.Context(), task.ID, "submit", decisions, time.Now().Add(time.Minute), false, "profile-a")
		if err != nil {
			t.Fatal(err)
		}
		c, err = d.ReviewTaskCompletion(t.Context(), task.ID, c.AttemptID)
		if err != nil {
			t.Fatal(err)
		}
		if c.ReviewDeadlineAt == nil || time.Until(*c.ReviewDeadlineAt) < 170*time.Second {
			t.Fatalf("review uses drain deadline: %+v", c)
		}
		if err := d.RecordTaskCompletionFailure(t.Context(), task.ID, c.AttemptID, "transient", "provider failed"); err != nil {
			t.Fatal(err)
		}
		if err := d.RecordTaskCompletionFailure(t.Context(), task.ID, c.AttemptID, "transient", "duplicate persistence"); err != nil {
			t.Fatal(err)
		}
		if err := d.InterruptTaskCompletion(t.Context(), task.ID, c.AttemptID, "failed", true); err != nil {
			t.Fatal(err)
		}
		if err := es.SetIntentState(intent, "open"); err != nil {
			t.Fatal(err)
		}
		if err := d.RecoverTaskCompletion(t.Context(), task.ID); err != nil {
			t.Fatal(err)
		}
		stored, err := d.TaskCompletion(t.Context(), task.ID)
		if err != nil || stored.FailureCount != count {
			t.Fatalf("restart reset count: %+v %v", stored, err)
		}
		_, err = d.BeginTaskCompletion(t.Context(), task.ID, "planner resubmits", decisions, time.Now().Add(time.Minute), false, "profile-a")
		var limited *CompletionRetryError
		if !errors.As(err, &limited) {
			t.Fatalf("resubmission bypassed policy: %v", err)
		}
		if count < 3 {
			if stored.NextRetryAt == nil {
				t.Fatal("missing backoff")
			}
			_, err = d.Exec(`UPDATE task_completions SET next_retry_at=now()-interval '1 second' WHERE task_id=$1`, task.ID)
			if err != nil {
				t.Fatal(err)
			}
		} else if stored.NextRetryAt != nil || stored.RecoveryAction != "manual_retry" {
			t.Fatalf("unbounded retry: %+v", stored)
		}
	}
	// A new fact is real input change and restores eligibility for review.
	if _, err := es.AddNode(KindFact, map[string]any{"summary": "new saved evidence"}, 0, "confirmed", "worker", nil); err != nil {
		t.Fatal(err)
	}
	c, err := d.BeginTaskCompletion(t.Context(), task.ID, "new evidence", decisions, time.Now().Add(time.Minute), false, "profile-a")
	if err != nil || c.FailureCount != 0 {
		t.Fatalf("real changes cannot recover: %+v %v", c, err)
	}
}

func TestCompletionDeterministicFailureAndAuditedRecovery(t *testing.T) {
	d, task, _ := completionFixture(t)
	c, err := d.BeginTaskCompletion(t.Context(), task.ID, "submit", nil, time.Now().Add(time.Minute), false, "profile-a")
	if err != nil {
		t.Fatal(err)
	}
	c, err = d.ReviewTaskCompletion(t.Context(), task.ID, c.AttemptID)
	if err != nil {
		t.Fatal(err)
	}
	if err := d.RecordTaskCompletionFailure(t.Context(), task.ID, c.AttemptID, "history", "missing reasoning"); err != nil {
		t.Fatal(err)
	}
	if err := d.InterruptTaskCompletion(t.Context(), task.ID, c.AttemptID, "repair history", true); err != nil {
		t.Fatal(err)
	}
	_, err = d.BeginTaskCompletion(t.Context(), task.ID, "wording changed", nil, time.Now().Add(time.Minute), false, "profile-a")
	var limited *CompletionRetryError
	if !errors.As(err, &limited) || limited.Next != nil {
		t.Fatalf("deterministic error retried: %v", err)
	}
	if err := d.ResetTaskCompletionFailure(t.Context(), task.ID, c.AttemptID, "human", "recovered context"); err != nil {
		t.Fatal(err)
	}
	c, err = d.BeginTaskCompletion(t.Context(), task.ID, "manual recovery", nil, time.Now().Add(time.Minute), false, "profile-a")
	if err != nil {
		t.Fatal(err)
	}
	var input CompletionInput
	json.Unmarshal(c.Snapshot, &input)
	if len(input.RecoveryHistory) != 1 || input.RecoveryHistory[0].FailureKind != "history" {
		t.Fatalf("lost audit: %s", c.Snapshot)
	}
}

func TestCompletionRejectsVerdictPastReviewDeadline(t *testing.T) {
	d, task, _ := completionFixture(t)
	c, err := d.BeginTaskCompletion(t.Context(), task.ID, "submit", nil, time.Now().Add(time.Hour), false)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(time.Second))
	defer cancel()
	c, err = d.ReviewTaskCompletion(ctx, task.ID, c.AttemptID, CompletionReviewPolicy{Budget: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	if c.ReviewDeadlineAt == nil || time.Until(*c.ReviewDeadlineAt) > time.Second {
		t.Fatal("parent budget ignored")
	}
	if _, err = d.Exec(`UPDATE task_completions SET review_deadline_at=now()-interval '1 second' WHERE task_id=$1`, task.ID); err != nil {
		t.Fatal(err)
	}
	if err = d.FinishTaskCompletion(t.Context(), task.ID, c.AttemptID, c.InputDigest, "done", "late"); !errors.Is(err, ErrCompletionChanged) {
		t.Fatalf("late verdict committed: %v", err)
	}
	stored, _ := d.GetTask(task.ID)
	if stored.Status == "done" {
		t.Fatal("late verdict marked done")
	}
}

func TestCompletionAlternatingFailuresAndProfileRecovery(t *testing.T) {
	d, task, _ := completionFixture(t)
	for _, kind := range []string{"transient", "budget", "transient", "budget", "transient"} {
		c, err := d.BeginTaskCompletion(t.Context(), task.ID, "submit", nil, time.Now().Add(time.Minute), false, "profile-a")
		if err != nil {
			t.Fatal(err)
		}
		if err = d.RecordTaskCompletionFailure(t.Context(), task.ID, c.AttemptID, kind, kind); err != nil {
			t.Fatal(err)
		}
		if err = d.InterruptTaskCompletion(t.Context(), task.ID, c.AttemptID, "failed", true); err != nil {
			t.Fatal(err)
		}
		if kind != "transient" || c.FailureCount < 2 {
			if _, err = d.Exec(`UPDATE task_completions SET next_retry_at=now()-interval '1 second' WHERE task_id=$1`, task.ID); err != nil {
				t.Fatal(err)
			}
		}
	}
	c, _ := d.TaskCompletion(t.Context(), task.ID)
	var counts map[string]int
	if json.Unmarshal(c.FailureCounts, &counts) != nil || counts["transient"] != 3 || counts["budget"] != 2 || c.NextRetryAt != nil {
		t.Fatalf("alternating errors reset or bypassed counts: %+v", c)
	}
	c, err := d.BeginTaskCompletion(t.Context(), task.ID, "fixed model profile", nil, time.Now().Add(time.Minute), false, "profile-b")
	if err != nil || c.FailureCount != 0 {
		t.Fatalf("configuration repair cannot recover: %+v %v", c, err)
	}
}

func TestCompletionRetryArchiveRetainsFailureAudit(t *testing.T) {
	d, task, _ := completionFixture(t)
	c, err := d.BeginTaskCompletion(t.Context(), task.ID, "submit", nil, time.Now().Add(time.Minute), false, "profile-a")
	if err != nil {
		t.Fatal(err)
	}
	if err = d.RecordTaskCompletionFailure(t.Context(), task.ID, c.AttemptID, "history", "missing history"); err != nil {
		t.Fatal(err)
	}
	if err = d.InterruptTaskCompletion(t.Context(), task.ID, c.AttemptID, "failed", true); err != nil {
		t.Fatal(err)
	}
	c, err = d.TaskCompletion(t.Context(), task.ID)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal([]*TaskCompletion{c})
	if err != nil {
		t.Fatal(err)
	}
	tx, err := d.BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if _, err = tx.Exec(`DELETE FROM task_completions WHERE task_id=$1`, task.ID); err != nil {
		t.Fatal(err)
	}
	if err = restoreTaskCompletion(tx, task.ID, raw); err != nil {
		t.Fatal(err)
	}
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}
	restored, err := d.TaskCompletion(t.Context(), task.ID)
	if err != nil || restored.FailureCount != 1 || restored.FailureKind != "history" || restored.RecoveryAction != "repair_history" || restored.InputDigest != "" || restored.AttemptID <= c.AttemptID {
		t.Fatalf("restore lost policy or retained old authority: %+v %v", restored, err)
	}
	_, err = d.BeginTaskCompletion(t.Context(), task.ID, "resume", nil, time.Now().Add(time.Minute), false, "profile-a")
	var limited *CompletionRetryError
	if !errors.As(err, &limited) {
		t.Fatalf("archive restore bypassed failure: %v", err)
	}
}
