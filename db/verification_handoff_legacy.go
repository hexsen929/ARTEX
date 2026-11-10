package db

import (
	"context"
	"database/sql"
	"slices"
	"time"
)

// Only three terminal, matching, completed built-in validation rejections can
// explain the old generic guard. Never infer this from a user/provider message.
func legacyVerificationFailure(ctx context.Context, tx *sql.Tx, expID, nodeID, taskID, resultID int64, token string, start, end time.Time) (*VerificationFieldFailure, error) {
	rows, err := tx.QueryContext(ctx, `SELECT r.tool,r.is_error,r.detail,coalesce(r.metadata->>'worker_run_token',''),r.created_at,
coalesce(u.detail,''),coalesce(u.metadata->>'worker_run_token',''),u.created_at
FROM activity r LEFT JOIN LATERAL (
 SELECT detail,metadata,created_at FROM activity WHERE exploration_id=$1 AND node_id=$2 AND kind='tool_use'
 AND tool=r.tool AND tool_use_id=r.tool_use_id AND id<r.id ORDER BY id DESC LIMIT 1
) u ON true
WHERE r.exploration_id=$1 AND r.node_id=$2 AND r.kind='tool_result' AND r.id<$3 AND r.created_at >= $4
ORDER BY r.id DESC LIMIT 101`, expID, nodeID, resultID, start)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var matched *VerificationFieldFailure
	count, seen := 0, 0
	for rows.Next() {
		var tool, output, resultToken, input, inputToken string
		var failed bool
		var at time.Time
		var usedAt sql.NullTime
		if err := rows.Scan(&tool, &failed, &output, &resultToken, &at, &input, &inputToken, &usedAt); err != nil {
			return nil, err
		}
		seen++
		if seen > 100 || resultToken != token || inputToken != token || !usedAt.Valid || usedAt.Time.Before(start) || usedAt.Time.After(at) || at.Before(start) || at.After(end) {
			return nil, nil
		}
		if seen > 3 && (!failed || IsDeferredLookupRejection(tool, []byte(input), []byte(output))) {
			continue
		}
		if tool != "set_finding_verification" || !failed {
			return nil, nil
		}
		failure := ParseVerificationFieldFailure([]byte(input), []byte(output))
		if failure == nil || (matched != nil && (failure.FindingID != matched.FindingID || !slices.Equal(failure.Fields, matched.Fields))) {
			return nil, nil
		}
		matched = failure
		count++
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	rows.Close()
	if count != 3 {
		return nil, nil
	}
	var own bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM findings f JOIN exploration_nodes n ON n.id=f.node_id WHERE f.id=$1 AND f.task_id=$2 AND n.exploration_id=$3)`, matched.FindingID, taskID, expID).Scan(&own); err != nil {
		return nil, err
	}
	if !own {
		return nil, nil
	}
	return matched, nil
}
