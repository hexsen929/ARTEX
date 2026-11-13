package db

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

type IntentRecovery struct {
	Attempts     int    `json:"attempts"`
	Limit        int    `json:"limit"`
	Status       string `json:"status"`
	Code         string `json:"code"`
	LastRunToken string `json:"last_run_token,omitempty"`
	At           string `json:"at,omitempty"`
	NextAt       string `json:"next_at,omitempty"`
	Message      string `json:"message"`
}

type recoveryPayload struct {
	Token      string               `json:"worker_run_token"`
	Result     WorkerResult         `json:"worker_result"`
	Checkpoint *ExecutionCheckpoint `json:"execution_checkpoint"`
	Previous   *ExecutionCheckpoint `json:"previous_checkpoint"`
	Recovery   *IntentRecovery      `json:"automatic_model_recovery"`
}

func IsTransientModelOutcome(code string) bool {
	switch code {
	case "transport_failure", "provider_failure", "rate_limited", "cooling_down":
		return true
	}
	return false
}

func completeRecoveryCheckpoint(cp *ExecutionCheckpoint, token string, settled bool) bool {
	return cp != nil && token != "" && cp.RunToken == token && (!settled || cp.Settled) && !cp.Incomplete && len(cp.Pending) == 0 && cp.ToolCalls >= 0 && cp.ToolResults >= 0 && cp.ToolCalls == cp.ToolResults+len(cp.NotExecuted)
}

// MayRetryIntentRun gates the older in-process retry path as well: a finished
// model request does not prove that every streamed tool finished. The caller
// reloads this node before constructing a fresh session from the same transcript.
func MayRetryIntentRun(node *Node, token string) bool {
	if node == nil || node.State != "running" {
		return false
	}
	var p recoveryPayload
	if json.Unmarshal(node.Payload, &p) != nil || p.Token != token {
		return false
	}
	return completeRecoveryCheckpoint(p.Checkpoint, token, false)
}

// IntentRecoveryInfo is shared by Planner metadata and the frontend DTO. A
// transient provider failure is a platform recovery state, not a request that
// the user fill in internal fields or repeatedly recreate an equivalent check.
func IntentRecoveryInfo(node *Node) *IntentRecovery {
	if node == nil {
		return nil
	}
	var p recoveryPayload
	if json.Unmarshal(node.Payload, &p) != nil {
		return nil
	}
	if node.State != "blocked" || !IsTransientModelOutcome(p.Result.Code) {
		if node.State == "open" && p.Recovery != nil {
			return p.Recovery
		}
		return nil
	}
	info := IntentRecovery{Limit: 2, Status: "checking", Code: p.Result.Code, Message: "模型调用中断，系统正在核对原执行记录并安排恢复；保留原意图与已有结果。"}
	if p.Recovery != nil {
		info = *p.Recovery
		info.Code = p.Result.Code
		if info.LastRunToken == p.Token && (info.Status == "result_uncertain" || info.Status == "disabled" || info.Status == "waiting") {
			return &info
		}
	}
	if info.Limit <= 0 || info.Attempts >= info.Limit {
		info.Status = "waiting"
		info.Message = "本轮自动恢复次数已用完，已保留进度；检查模型服务后可在总览或当前会话恢复原意图。"
	} else if p.Checkpoint != nil && (p.Checkpoint.Incomplete || len(p.Checkpoint.Pending) > 0) {
		info.Status = "checking"
		info.Message = "系统正在核对中断时的工具记录；确认执行结果前不会重复操作。"
	} else {
		info.Status = "retry_wait"
		info.Message = "模型调用中断，系统将退避后自动续接原会话，无需新建意图。"
	}
	return &info
}

// RecoverTransientIntents runs inside the worker polling loop, independently of
// model-generated planning. Task/archive and exploration locks serialize the
// decision with pause/delete/archive and another recovery process. Both the
// retry count and run generation survive restarts. No model or tool runs here.
func (s *ExplorationStore) RecoverTransientIntents(ctx context.Context, now time.Time, retryLimit int, delay time.Duration) ([]int64, error) {
	retryLimit = max(0, min(retryLimit, 8))
	if delay < 15*time.Second {
		delay = 15 * time.Second
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	var taskID int64
	var paused, queued bool
	var status string
	err = tx.QueryRowContext(ctx, `SELECT id,paused,queued,status FROM tasks WHERE exploration_id=$1 AND deleted_at IS NULL AND archived_at IS NULL FOR UPDATE`, s.expID).Scan(&taskID, &paused, &queued, &status)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if paused || queued || (status != "running" && status != "created") {
		return nil, nil
	}
	if err = LockTaskEvidenceTx(tx, taskID); err != nil {
		if errors.Is(err, ErrTaskArchiveState) {
			return nil, nil
		}
		return nil, err
	}
	var exp int64
	if err = tx.QueryRowContext(ctx, `SELECT id FROM explorations WHERE id=$1 FOR NO KEY UPDATE`, s.expID).Scan(&exp); err != nil {
		return nil, err
	}
	rows, err := tx.QueryContext(ctx, `SELECT id,payload,completed_at FROM exploration_nodes WHERE exploration_id=$1 AND kind='intent' AND state='blocked' AND payload#>>'{worker_result,code}' IN ('transport_failure','provider_failure','rate_limited','cooling_down') AND (coalesce(payload#>>'{automatic_model_recovery,last_run_token}','')<>coalesce(payload->>'worker_run_token','') OR coalesce(payload#>>'{automatic_model_recovery,limit}','')<>$2::text OR coalesce(payload#>>'{automatic_model_recovery,status}','') NOT IN ('result_uncertain','disabled','waiting')) ORDER BY updated_at DESC,id DESC LIMIT 128 FOR UPDATE`, s.expID, fmt.Sprint(retryLimit))
	if err != nil {
		return nil, err
	}
	type candidate struct {
		id        int64
		raw       []byte
		completed sql.NullTime
	}
	var candidates []candidate
	for rows.Next() {
		var c candidate
		if err = rows.Scan(&c.id, &c.raw, &c.completed); err != nil {
			rows.Close()
			return nil, err
		}
		candidates = append(candidates, c)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, err
	}
	var recovered []int64
	for _, c := range candidates {
		if len(recovered) >= 4 {
			break
		}
		var p recoveryPayload
		if json.Unmarshal(c.raw, &p) != nil || !c.completed.Valid || p.Checkpoint == nil || p.Token == "" || p.Checkpoint.RunToken != p.Token || !p.Checkpoint.Settled {
			continue
		}
		attempts := 0
		if p.Recovery != nil {
			attempts = p.Recovery.Attempts
		}
		if attempts < 0 {
			continue
		}
		park := func(status, message, nextAt string) error {
			r := IntentRecovery{Attempts: attempts, Limit: retryLimit, Status: status, Code: p.Result.Code, LastRunToken: p.Token, NextAt: nextAt, Message: message}
			if p.Recovery != nil && p.Recovery.Status == r.Status && p.Recovery.LastRunToken == p.Token && p.Recovery.NextAt == r.NextAt && p.Recovery.Limit == retryLimit {
				return nil
			}
			raw, _ := json.Marshal(r)
			_, err := tx.ExecContext(ctx, `UPDATE exploration_nodes SET payload=jsonb_set(payload,'{automatic_model_recovery}',$1::jsonb) WHERE id=$2 AND exploration_id=$3 AND state='blocked'`, string(raw), c.id, s.expID)
			return err
		}
		if retryLimit == 0 {
			if err = park("disabled", "模型意图自动恢复已在重试设置中关闭；已有进度保留，可在这里恢复原意图。", ""); err != nil {
				return nil, err
			}
			continue
		}
		if attempts >= retryLimit {
			if err = park("waiting", "本轮自动恢复次数已用完，已有进度保留；可在模型服务恢复后继续原意图。", ""); err != nil {
				return nil, err
			}
			continue
		}
		if p.Checkpoint.Incomplete {
			if err = park("result_uncertain", "中断时的执行记录未完整保存，系统保留原记录并停止重复操作；请查看具体结果后恢复此意图。", ""); err != nil {
				return nil, err
			}
			continue
		}
		// An earlier unknown operation must not disappear behind a later zero-tool run.
		if p.Previous != nil && (p.Previous.Incomplete || len(p.Previous.Pending) > 0) {
			if err = park("result_uncertain", "更早的一次运行仍有未确认的操作，已保留全部记录，系统不会重复执行。", ""); err != nil {
				return nil, err
			}
			continue
		}
		start, parseErr := time.Parse(time.RFC3339Nano, p.Checkpoint.StartedAt)
		if parseErr != nil || start.After(c.completed.Time) {
			continue
		}
		wait := min(delay*time.Duration(1<<min(attempts, 5)), 5*time.Minute)
		next := c.completed.Time.Add(wait)
		if now.Before(next) {
			if err = park("retry_wait", "模型调用中断，系统正在退避等待，随后续接原会话，无需新建意图。", next.UTC().Format(time.RFC3339Nano)); err != nil {
				return nil, err
			}
			continue
		}
		var hardFailure bool
		if err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM activity WHERE exploration_id=$1 AND node_id=$2 AND kind='tool_result' AND is_error AND metadata->>'worker_run_token'=$3 AND (detail LIKE 'Blocked by hook:%' OR detail LIKE 'Tool call was not permitted:%' OR detail LIKE 'approval_wait:%'))`, s.expID, c.id, p.Token).Scan(&hardFailure); err != nil {
			return nil, err
		}
		if hardFailure {
			if err = park("result_uncertain", "原运行还存在审批或权限拦截，系统保留拦截条件，不会自动重放操作。", ""); err != nil {
				return nil, err
			}
			continue
		}
		if len(p.Checkpoint.Pending) > 0 {
			ok, err := s.reconcileEmptyBashStream(ctx, tx, c.id, p.Checkpoint, start, c.completed.Time)
			if err != nil {
				return nil, err
			}
			if !ok {
				if err = park("result_uncertain", "上次工具结果尚未确认，系统已保留原记录并停止重复操作；可在这里查看记录后恢复此意图。", ""); err != nil {
					return nil, err
				}
				continue
			}
		}
		if !completeRecoveryCheckpoint(p.Checkpoint, p.Token, true) {
			continue
		}
		recovery := IntentRecovery{Attempts: attempts + 1, Limit: retryLimit, Status: "queued", Code: p.Result.Code, LastRunToken: p.Token, At: now.UTC().Format(time.RFC3339Nano), Message: "临时模型故障已由系统核对，自动续接原意图、会话与已有结果。"}
		change, _ := json.Marshal(map[string]any{"automatic_model_recovery": recovery, "execution_checkpoint": p.Checkpoint, "continuation_note": "系统已核对上次工具调用与返回。沿用原会话及产物，只继续未完成的步骤；not_executed标记的空参数调用没有可执行命令，不重放旧请求。"})
		if _, err = tx.ExecContext(ctx, `UPDATE exploration_nodes SET state='open',completed_at=NULL,blocked_reason=NULL,payload=payload||$1::jsonb,content_version=content_version+1,updated_at=now() WHERE exploration_id=$2 AND id=$3 AND state='blocked'`, string(change), s.expID, c.id); err != nil {
			return nil, err
		}
		metadata, _ := json.Marshal(map[string]any{"worker_run_token": p.Token, "automatic_model_recovery": recovery})
		if _, err = s.appendActivityRow(ctx, tx, Activity{NodeID: &c.id, Worker: "system", Kind: "status", Summary: recovery.Message, Detail: fmt.Sprintf("自动恢复 %d/%d；原执行与结果保留，未重放已完成操作。", recovery.Attempts, retryLimit), Metadata: metadata}); err != nil {
			return nil, err
		}
		recovered = append(recovered, c.id)
	}
	if err = tx.Commit(); err != nil {
		return nil, err
	}
	return recovered, nil
}

// Old Norma v0.4.3 flushes an unfinished streamed Bash block as {} on EOF.
// The built-in Bash validates its required command BEFORE any process can start;
// its handler independently rejects a missing command too. Only reconcile this
// exact case when there is no persisted Bash schema/default override and every
// event in the checkpoint generation is accounted for. Preserve the original
// call and its absent result; record a separate not_executed classification.
func (s *ExplorationStore) reconcileEmptyBashStream(ctx context.Context, tx *sql.Tx, id int64, cp *ExecutionCheckpoint, start, end time.Time) (bool, error) {
	if len(cp.Pending) > 4 || cp.ToolCalls > 256 || cp.ToolCalls-cp.ToolResults != len(cp.Pending) || len(cp.NotExecuted) > 0 {
		return false, nil
	}
	for _, name := range cp.Pending {
		if name != "Bash" {
			return false, nil
		}
	}
	var override bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM tools WHERE key='Bash')`).Scan(&override); err != nil {
		return false, err
	}
	if override {
		return false, nil
	}
	rows, err := tx.QueryContext(ctx, `SELECT kind,coalesce(tool,''),coalesce(tool_use_id,''),coalesce(detail,''),created_at FROM activity WHERE exploration_id=$1 AND node_id=$2 AND kind IN ('tool_use','tool_result') AND metadata->>'worker_run_token'=$3 ORDER BY id LIMIT 513`, s.expID, id, cp.RunToken)
	if err != nil {
		return false, err
	}
	defer rows.Close()
	calls, results := map[string]string{}, map[string]bool{}
	used, returned := 0, 0
	for rows.Next() {
		var kind, name, call, detail string
		var at time.Time
		if err = rows.Scan(&kind, &name, &call, &detail, &at); err != nil {
			return false, err
		}
		if call == "" || at.Before(start) || at.After(end) {
			return false, nil
		}
		if kind == "tool_use" {
			if _, exists := calls[call]; exists {
				return false, nil
			}
			calls[call] = name
			used++
			if _, pending := cp.Pending[call]; pending {
				var input map[string]json.RawMessage
				if name != "Bash" || json.Unmarshal([]byte(detail), &input) != nil || input == nil || len(input) != 0 {
					return false, nil
				}
			}
		} else {
			if results[call] || calls[call] != name {
				return false, nil
			}
			results[call] = true
			returned++
		}
	}
	if err = rows.Err(); err != nil {
		return false, err
	}
	rows.Close()
	if used != cp.ToolCalls || returned != cp.ToolResults {
		return false, nil
	}
	for call := range calls {
		if !results[call] && cp.Pending[call] != "Bash" {
			return false, nil
		}
	}
	var terminal string
	if err = tx.QueryRowContext(ctx, `SELECT coalesce(detail,'') FROM activity WHERE exploration_id=$1 AND node_id=$2 AND kind='result' AND is_error AND created_at BETWEEN $3 AND $4 ORDER BY id DESC LIMIT 1`, s.expID, id, start, end.Add(time.Second)).Scan(&terminal); errors.Is(err, sql.ErrNoRows) {
		return false, nil
	} else if err != nil {
		return false, err
	}
	if !strings.Contains(terminal, "`unexpected EOF`") {
		return false, nil
	}
	cp.NotExecuted = map[string]string{}
	for call := range cp.Pending {
		cp.NotExecuted[call] = "stream_arguments_missing"
	}
	cp.Pending = map[string]string{}
	return true, nil
}

// The goalless completion path must not declare a task finished while the host
// is waiting to recover a transiently interrupted original intent.
func (s *ExplorationStore) HasPendingModelRecovery(ctx context.Context, limit int) (bool, error) {
	if limit <= 0 {
		return false, nil
	}
	var pending bool
	err := s.db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM exploration_nodes WHERE exploration_id=$1 AND kind='intent' AND state='blocked' AND payload#>>'{worker_result,code}' IN ('transport_failure','provider_failure','rate_limited','cooling_down') AND coalesce(payload#>>'{automatic_model_recovery,status}','') NOT IN ('result_uncertain','disabled','waiting') AND CASE WHEN coalesce(payload#>>'{automatic_model_recovery,attempts}','0') ~ '^[0-9]{1,3}$' THEN coalesce(payload#>>'{automatic_model_recovery,attempts}','0')::integer < $2 ELSE false END)`, s.expID, limit).Scan(&pending)
	return pending, err
}
