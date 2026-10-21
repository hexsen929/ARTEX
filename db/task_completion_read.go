package db

import "context"

// TaskCompletionSummaries keeps list polling independent of potentially large
// evidence snapshots. The detail endpoint reads the saved review and work list.
func (d *DB) TaskCompletionSummaries(ctx context.Context) (map[int64]*TaskCompletion, error) {
	rows, err := d.QueryContext(ctx, `SELECT task_id,attempt_id,phase,reason,started_at,deadline_at,updated_at,review_deadline_at,failure_kind,failure_count,next_retry_at,recovery_action FROM task_completions`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[int64]*TaskCompletion{}
	for rows.Next() {
		c := new(TaskCompletion)
		if err := rows.Scan(&c.TaskID, &c.AttemptID, &c.Phase, &c.Reason, &c.StartedAt, &c.DeadlineAt, &c.UpdatedAt, &c.ReviewDeadlineAt, &c.FailureKind, &c.FailureCount, &c.NextRetryAt, &c.RecoveryAction); err != nil {
			return nil, err
		}
		out[c.TaskID] = c
	}
	return out, rows.Err()
}
