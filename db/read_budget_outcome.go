package db

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"
)

const LegacyTrafficReadBudgetMessage = "tool_loop_guard: 本轮流量检索达到读取上限，已停止继续翻页；已有记录保留，尚未查完，不表示任务完成。请人工核对检索目的与进度后再处理。"

// Repair only the former code-owned read-limit outcome. Never resume execution,
// alter activity history, or reinterpret approval/authentication/unknown stops.
// The exact final result and current checkpoint must agree. Missing results
// may only belong to local traffic_search reads; they are never replayed.
func (s *ExplorationStore) ReclassifyLegacyReadBudgetStops(ctx context.Context) (int, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id FROM exploration_nodes WHERE exploration_id=$1 AND kind='intent'
AND state='blocked' AND payload#>>'{worker_result,code}'='tool_loop_guard' ORDER BY id LIMIT 100`, s.expID)
	if err != nil {
		return 0, err
	}
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return 0, err
		}
		ids = append(ids, id)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return 0, err
	}
	changed := 0
	for _, id := range ids {
		ok, err := s.reclassifyReadBudgetStop(ctx, id)
		if err != nil {
			return changed, err
		}
		if ok {
			changed++
		}
	}
	return changed, nil
}

func (s *ExplorationStore) reclassifyReadBudgetStop(ctx context.Context, id int64) (bool, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	var taskID int64
	if err = tx.QueryRowContext(ctx, `SELECT id FROM tasks WHERE exploration_id=$1 AND deleted_at IS NULL AND archived_at IS NULL`, s.expID).Scan(&taskID); errors.Is(err, sql.ErrNoRows) {
		return false, nil
	} else if err != nil {
		return false, err
	}
	if err = LockTaskEvidenceTx(tx, taskID); err != nil {
		return false, err
	}
	var raw []byte
	var completed sql.NullTime
	err = tx.QueryRowContext(ctx, `SELECT payload,completed_at FROM exploration_nodes WHERE id=$1 AND exploration_id=$2 AND kind='intent'
AND state='blocked' AND payload#>>'{worker_result,code}'='tool_loop_guard' FOR UPDATE`, id, s.expID).Scan(&raw, &completed)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	var p struct {
		Token      string               `json:"worker_run_token"`
		Checkpoint *ExecutionCheckpoint `json:"execution_checkpoint"`
		Attempt    int                  `json:"execution_attempt"`
		Previous   json.RawMessage      `json:"previous_checkpoint"`
	}
	if json.Unmarshal(raw, &p) != nil || p.Checkpoint == nil || !completed.Valid {
		return false, nil
	}
	cp := p.Checkpoint
	start, err := time.Parse(time.RFC3339Nano, cp.StartedAt)
	if err != nil || p.Token == "" || cp.RunToken != p.Token || !cp.Settled || cp.Incomplete || cp.ToolCalls-cp.ToolResults != len(cp.Pending) || start.After(completed.Time) {
		return false, nil
	}
	for _, tool := range cp.Pending {
		if tool != "traffic_search" {
			return false, nil
		}
	}
	var detail string
	var at time.Time
	var failed bool
	var resultToken string
	err = tx.QueryRowContext(ctx, `SELECT coalesce(detail,''),created_at,is_error,coalesce(metadata->>'worker_run_token','') FROM activity WHERE exploration_id=$1 AND node_id=$2 AND kind='result' ORDER BY id DESC LIMIT 1`, s.expID, id).Scan(&detail, &at, &failed, &resultToken)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if !failed || detail != LegacyTrafficReadBudgetMessage || at.Before(start) || at.After(completed.Time) || completed.Time.Sub(at) > 10*time.Second {
		return false, nil
	}
	// Old releases omitted the token on terminal activity. Accept such rows
	// only for a first execution with no previous checkpoint: no older run can
	// supply a late terminal. Ambiguous multi-run records remain unchanged.
	if resultToken != "" {
		if resultToken != p.Token {
			return false, nil
		}
	} else if p.Attempt != 1 || (len(p.Previous) > 0 && string(p.Previous) != "null") {
		return false, nil
	}
	_, err = tx.ExecContext(ctx, `UPDATE exploration_nodes SET state='exhausted',blocked_reason=NULL,
payload=jsonb_set(payload,'{worker_result}',(payload->'worker_result')||jsonb_build_object('code','traffic_read_budget','legacy_code','tool_loop_guard',
'message','旧版读取上限已归为读取批次结束；原始结果及未返回的读取标记保留，由Planner读取已有记录协调，无需用户逐页核对，不重新执行。')),content_version=content_version+1
WHERE id=$1 AND exploration_id=$2`, id, s.expID)
	if err != nil {
		return false, err
	}
	if err = tx.Commit(); err != nil {
		return false, err
	}
	return true, nil
}
