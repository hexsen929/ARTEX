package db

import (
	"context"
	"database/sql"
	"errors"
)

// expireExhaustedMemoryLeases retires a bounded set of abandoned third leases.
// This is not another claim/HTTP attempt and never refunds quota or retries.
// Source -> job ordering matches invalidation/completion; no content is built.
func expireExhaustedMemoryLeases(ctx context.Context, tx *sql.Tx) error {
	rows, err := tx.QueryContext(ctx, `SELECT job.id,job.source_ref FROM memory_jobs job
		JOIN memory_sources source ON source.id=job.source_ref
		WHERE job.state='running' AND job.attempts>=3
		AND (job.lease_until IS NULL OR job.lease_until<=now())
		AND source.source_kind IN ('intent_stage','task_stage')
		ORDER BY job.source_ref,job.id LIMIT 20`)
	if err != nil {
		return err
	}
	type candidate struct{ id, ref int64 }
	candidates := []candidate{}
	for rows.Next() {
		var c candidate
		if err := rows.Scan(&c.id, &c.ref); err != nil {
			rows.Close()
			return err
		}
		candidates = append(candidates, c)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	for _, c := range candidates {
		var admission MemoryAdmission
		err := tx.QueryRowContext(ctx, `SELECT origin_task_id,origin_conversation_id,agent_key,session_id
			FROM memory_sources WHERE id=$1 FOR UPDATE SKIP LOCKED`, c.ref).Scan(
			&admission.TaskID, &admission.ConversationID, &admission.AgentKey, &admission.SessionID)
		if errors.Is(err, sql.ErrNoRows) {
			continue
		}
		if err != nil {
			return err
		}
		var version int64
		var fingerprint string
		err = tx.QueryRowContext(ctx, `UPDATE memory_jobs SET state='skipped',result='lease_expired',
			lease_token='',lease_until=NULL,updated_at=now()
			WHERE id=$1 AND state='running' AND attempts>=3 AND (lease_until IS NULL OR lease_until<=now())
			RETURNING source_version,source_fingerprint`, c.id).Scan(&version, &fingerprint)
		if errors.Is(err, sql.ErrNoRows) {
			continue
		}
		if err != nil {
			return err
		}
		// A newer source generation/result must not be overwritten by expiry of
		// an older lease. Do not manufacture a completion watermark for either.
		if _, err := tx.ExecContext(ctx, `UPDATE memory_sources SET last_result='lease_expired',last_result_at=now(),last_job_id=$2
			WHERE id=$1 AND source_version=$3 AND source_fingerprint=$4 AND last_job_id<=$2`,
			c.ref, c.id, version, fingerprint); err != nil {
			return err
		}
		if err := memoryAuditTx(ctx, tx, 0, 0, c.id, admission, "extract_skipped"); err != nil {
			return err
		}
	}
	return nil
}
