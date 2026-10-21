package db

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
)

type CompletionRetryError struct {
	Kind   string
	Count  int
	Next   *time.Time
	Action string
}

func (e *CompletionRetryError) Error() string {
	return fmt.Sprintf("收官复核受限：%s，累计失败 %d 次；恢复动作：%s", e.Kind, e.Count, e.Action)
}

func (d *DB) TaskCompletionRetryDue(ctx context.Context, taskID int64) (bool, error) {
	var due bool
	err := d.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM task_completions WHERE task_id=$1 AND phase='interrupted' AND next_retry_at<=now())`, taskID).Scan(&due)
	return due, err
}

type CompletionRecovery struct {
	At           time.Time `json:"at"`
	Actor        string    `json:"actor"`
	Reason       string    `json:"reason"`
	FailureKind  string    `json:"failure_kind"`
	FailureCount int       `json:"failure_count"`
}

// Execution-state churn during drain/reopen does not constitute new evidence.
// Stable graph payload, completed work, scope, goals and proof changes do.
func completionRetryDigest(input CompletionInput) (string, error) {
	var data map[string]any
	decoder := json.NewDecoder(bytes.NewReader(input.Data))
	decoder.UseNumber()
	if err := decoder.Decode(&data); err != nil {
		return "", err
	}
	if nodes, ok := data["nodes"].([]any); ok {
		for _, raw := range nodes {
			if n, ok := raw.(map[string]any); ok {
				delete(n, "proof_review") // Audit prose is not new evidence.
				if n["kind"] != KindIntent {
					continue
				}
				delete(n, "content_version")
				if n["state"] != "done" {
					n["state"] = "pending"
				}
			}
		}
	}
	type decision struct {
		Intent    int64
		Kind      string
		Reference int64
	}
	var decisions []decision
	for _, w := range input.Work {
		if w.Kind != "" {
			decisions = append(decisions, decision{w.IntentID, w.Kind, w.ReferenceID})
		}
	}
	sort.Slice(decisions, func(i, j int) bool { return decisions[i].Intent < decisions[j].Intent })
	raw, err := json.Marshal(struct {
		Data      map[string]any
		Decisions []decision
	}{data, decisions})
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:]), nil
}

func completionFailureRecovery(kind string, count int, now time.Time, taskID int64) (*time.Time, string) {
	switch kind {
	case "transient", "budget", "no_verdict", "snapshot":
		if count <= 2 {
			delay := 15 * time.Second
			if count == 2 {
				delay = 60 * time.Second
			}
			next := now.Add(delay + time.Duration(taskID%5)*time.Second)
			return &next, "retry"
		}
		return nil, "manual_retry"
	case "evidence":
		return nil, "repair_evidence"
	case "history":
		return nil, "repair_history"
	case "configuration":
		return nil, "configure_model"
	case "drain":
		return nil, "wait_for_exit"
	default:
		return nil, "manual_retry"
	}
}

// RecordTaskCompletionFailure is a short local transaction. Counters for every
// failure category survive alternating error types and process restarts.
func (d *DB) RecordTaskCompletionFailure(ctx context.Context, taskID, attemptID int64, kind, reason string, gaps ...CompletionGoalGap) error {
	return d.completionTransaction(ctx, taskID, func(tx *sql.Tx) error {
		c, err := scanCompletion(tx.QueryRowContext(ctx, `SELECT `+completionColumns+` FROM task_completions WHERE task_id=$1 AND attempt_id=$2 AND phase IN ('draining','reviewing','interrupted')`, taskID, attemptID))
		if err != nil || c == nil {
			return errors.Join(err, ErrCompletionChanged)
		}
		if c.FailureAttemptID == attemptID {
			return nil
		}
		var input CompletionInput
		if err := json.Unmarshal(c.Snapshot, &input); err != nil {
			return err
		}
		input.GoalGaps = append([]CompletionGoalGap(nil), gaps...)
		snapshot, err := json.Marshal(input)
		if err != nil {
			return err
		}
		digest, err := completionRetryDigest(input)
		if err != nil {
			return err
		}
		counts := map[string]int{}
		if c.FailureDigest == digest && c.FailureProfile == c.ReviewProfile {
			if err := json.Unmarshal(c.FailureCounts, &counts); err != nil {
				return err
			}
		}
		counts[kind]++
		next, action := completionFailureRecovery(kind, counts[kind], time.Now(), taskID)
		raw, err := json.Marshal(counts)
		if err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `UPDATE task_completions SET failure_kind=$3,failure_count=$4,failure_counts=$5,failure_digest=$6,failure_profile=review_profile,next_retry_at=$7,recovery_action=$8,reason=$9,updated_at=now(),failure_attempt_id=$2,snapshot=$10 WHERE task_id=$1 AND attempt_id=$2`, taskID, attemptID, kind, counts[kind], string(raw), digest, next, action, reason, string(snapshot))
		return err
	})
}

func (d *DB) ResetTaskCompletionFailure(ctx context.Context, taskID, attemptID int64, actor, reason string) error {
	if strings.TrimSpace(reason) == "" || strings.TrimSpace(actor) == "" {
		return fmt.Errorf("恢复必须有操作者和原因")
	}
	return d.completionTransaction(ctx, taskID, func(tx *sql.Tx) error {
		c, err := scanCompletion(tx.QueryRowContext(ctx, `SELECT `+completionColumns+` FROM task_completions WHERE task_id=$1 AND attempt_id=$2 AND phase='interrupted'`, taskID, attemptID))
		if err != nil || c == nil {
			return errors.Join(err, ErrCompletionChanged)
		}
		var input CompletionInput
		if err := json.Unmarshal(c.Snapshot, &input); err != nil {
			return err
		}
		if input.RecoveryPending {
			return ErrCompletionChanged
		}
		input.RecoveryHistory = append(input.RecoveryHistory, CompletionRecovery{At: time.Now().UTC(), Actor: actor, Reason: reason, FailureKind: c.FailureKind, FailureCount: c.FailureCount})
		raw, err := json.Marshal(input)
		if err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `UPDATE task_completions SET failure_kind='',failure_count=0,failure_counts='{}',failure_digest='',failure_profile='',next_retry_at=NULL,recovery_action='',snapshot=$3,reason=$4,updated_at=now() WHERE task_id=$1 AND attempt_id=$2`, taskID, attemptID, string(raw), reason)
		return err
	})
}
