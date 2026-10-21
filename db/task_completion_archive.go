package db

import (
	"database/sql"
	"encoding/json"
	"fmt"
)

// Historical snapshots remain audit material. Restoring one never grants a
// previous process authority to commit its completion decision.
func restoreTaskCompletion(tx *sql.Tx, taskID int64, raw json.RawMessage) error {
	if len(raw) == 0 {
		return nil // Archives produced before completion tracking.
	}
	var records []TaskCompletion
	if err := json.Unmarshal(raw, &records); err != nil {
		return fmt.Errorf("restore task completion: %w", err)
	}
	if len(records) == 0 {
		return nil
	}
	if len(records) != 1 || records[0].TaskID != taskID {
		return fmt.Errorf("restore task completion: task identity mismatch")
	}
	c := records[0]
	if c.AttemptID < 0 || c.AttemptID == 1<<63-1 {
		return fmt.Errorf("restore task completion: invalid attempt")
	}
	if c.Phase == "draining" || c.Phase == "reviewing" {
		var input CompletionInput
		if err := json.Unmarshal(c.Snapshot, &input); err != nil {
			return err
		}
		for _, work := range input.Work {
			if work.State != "running" {
				continue
			}
			if _, err := tx.Exec(`UPDATE exploration_nodes SET state='open',completed_at=NULL
			 WHERE id=$1 AND exploration_id=(SELECT exploration_id FROM tasks WHERE id=$2)
			 AND kind='intent' AND state IN ('stopped','exhausted')`, work.IntentID, taskID); err != nil {
				return err
			}
		}
		c.Phase = "interrupted"
		c.Reason = "归档恢复使旧收官决定失效；已保留当时快照，等待重新复核"
	}
	if len(c.Snapshot) == 0 {
		c.Snapshot = json.RawMessage(`{}`)
	}
	if len(c.FailureCounts) == 0 {
		c.FailureCounts = json.RawMessage(`{}`)
	}
	if c.FailureCount < 0 {
		return fmt.Errorf("restore task completion: invalid failure count")
	}
	_, err := tx.Exec(`INSERT INTO task_completions(task_id,attempt_id,phase,reason,snapshot,input_digest,started_at,deadline_at,updated_at,review_deadline_at,review_profile,failure_kind,failure_count,failure_counts,failure_digest,failure_profile,next_retry_at,recovery_action,failure_attempt_id)
	 VALUES($1,$2,$3,$4,$5,'',$6,$7,now(),$8,$9,$10,$11,$12,$13,$14,$15,$16,$17)`, taskID, c.AttemptID+1, c.Phase, c.Reason,
		string(c.Snapshot), c.StartedAt, c.DeadlineAt, c.ReviewDeadlineAt, c.ReviewProfile, c.FailureKind, c.FailureCount, string(c.FailureCounts), c.FailureDigest, c.FailureProfile, c.NextRetryAt, c.RecoveryAction, c.FailureAttemptID)
	return err
}
