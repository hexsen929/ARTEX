package db

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"
)

const LegacyTrafficReadBudgetMessage = "tool_loop_guard: 本轮流量检索达到读取上限，已停止继续翻页；已有记录保留，尚未查完，不表示任务完成。请人工核对检索目的与进度后再处理。"
const LegacyVerificationGuardMessage = "tool_loop_guard: 同类参数或恢复条件错误反复出现，本轮已停止；已有记录保留，尚未完成。请先处理明确的缺失字段或人工恢复条件，不重放同一请求。"

// Repair only proven code-owned read-limit or verification-field outcomes.
// Never resume execution, alter activity history or reinterpret real faults.
// The terminal and current checkpoint must agree. Pending tools are allowed
// only for the former read-limit path and only for local traffic_search reads.
func (s *ExplorationStore) ReclassifyLegacyToolStops(ctx context.Context) (int, error) {
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
		ok, err := s.reclassifyLegacyToolStop(ctx, id)
		if err != nil {
			return changed, err
		}
		if ok {
			changed++
		}
	}
	return changed, nil
}

func (s *ExplorationStore) reclassifyLegacyToolStop(ctx context.Context, id int64) (bool, error) {
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
	var resultID int64
	err = tx.QueryRowContext(ctx, `SELECT id,coalesce(detail,''),created_at,is_error,coalesce(metadata->>'worker_run_token','') FROM activity WHERE exploration_id=$1 AND node_id=$2 AND kind='result' ORDER BY id DESC LIMIT 1`, s.expID, id).Scan(&resultID, &detail, &at, &failed, &resultToken)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if !failed || (detail != LegacyTrafficReadBudgetMessage && detail != LegacyVerificationGuardMessage) || at.Before(start) || at.After(completed.Time) || completed.Time.Sub(at) > 10*time.Second {
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
	code := "traffic_read_budget"
	message := "旧版读取上限已归为读取批次结束；原始结果及未返回的读取标记保留，由Planner读取已有记录协调，无需用户逐页核对，不重新执行。"
	if detail == LegacyVerificationGuardMessage {
		if len(cp.Pending) != 0 {
			return false, nil
		}
		failure, lookupErr := legacyVerificationFailure(ctx, tx, s.expID, id, taskID, resultID, p.Token, start, at)
		if lookupErr != nil {
			return false, lookupErr
		}
		if failure == nil {
			return false, nil
		}
		code, message = VerificationHandoffCode, failure.Message()
	}
	_, err = tx.ExecContext(ctx, `UPDATE exploration_nodes SET state='exhausted',blocked_reason=NULL,
payload=jsonb_set(payload,'{worker_result}',(payload->'worker_result')||jsonb_build_object('code',$3::text,'legacy_code','tool_loop_guard','message',$4::text)),content_version=content_version+1
WHERE id=$1 AND exploration_id=$2`, id, s.expID, code, message)
	if err != nil {
		return false, err
	}
	if err = tx.Commit(); err != nil {
		return false, err
	}
	return true, nil
}
