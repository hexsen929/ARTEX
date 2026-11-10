package db

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"time"
)

// Stable, task-local identity. These are labels, never credentials or payloads.
// A change of wording in the summary does not create a new check.
type CheckIdentity struct {
	Resource    string `json:"resource"`
	Operation   string `json:"operation"`
	IdentityRef string `json:"identity_ref"`
	Question    string `json:"question"`
}

func (c CheckIdentity) Normalize(assets []int64) (CheckIdentity, string, error) {
	fields := []*string{&c.Resource, &c.Operation, &c.IdentityRef, &c.Question}
	for _, f := range fields {
		*f = strings.TrimSpace(*f)
		if *f == "" || len([]rune(*f)) > 400 {
			return c, "", fmt.Errorf("check_identity 的 resource/operation/identity_ref/question 均须填写且每项不超过400字；身份只填标签，不填令牌")
		}
	}
	ids := slices.Clone(assets)
	slices.Sort(ids)
	ids = slices.Compact(ids)
	raw, _ := json.Marshal([]any{c, ids})
	return c, fmt.Sprintf("%x", sha256.Sum256(raw)), nil
}

// Continue only known, settled budget exhaustion. Infrastructure, approval,
// manual stops and unknown tool results require explicit operator recovery.
// Reusing the same row preserves WorkerSessionID and all evidence references.
func (s *ExplorationStore) ContinuePlannerIntent(id int64, note string, limit int) (int64, error) {
	note = strings.TrimSpace(note)
	if note == "" || len([]rune(note)) > 2000 {
		return 0, fmt.Errorf("intent_missing_fields: 续接须填写不超过2000字的 continuation_note，说明复用产物和下一步；字段未改正前不要重复请求")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	var exp int64
	if err := tx.QueryRowContext(ctx, `SELECT id FROM explorations WHERE id=$1 FOR NO KEY UPDATE`, s.expID).Scan(&exp); err != nil {
		return 0, err
	}
	var state string
	var raw []byte
	if err := tx.QueryRowContext(ctx, `SELECT state,payload FROM exploration_nodes WHERE exploration_id=$1 AND id=$2 AND kind='intent' FOR UPDATE`, s.expID, id).Scan(&state, &raw); err != nil {
		return 0, fmt.Errorf("continue_intent_id 必须是本任务已有意图: %w", err)
	}
	if state == "open" || state == "running" {
		return id, tx.Commit()
	}
	var p struct {
		RunToken      string               `json:"worker_run_token"`
		Result        WorkerResult         `json:"worker_result"`
		Checkpoint    *ExecutionCheckpoint `json:"execution_checkpoint"`
		Continuations int                  `json:"automatic_continuations"`
	}
	if err := json.Unmarshal(raw, &p); err != nil {
		return 0, err
	}
	if state == "exhausted" && p.Result.Code == "traffic_read_budget" {
		// The work has already handed its saved results back to the Planner.
		// Do not turn coordination into replay of the same bounded read loop.
		return id, tx.Commit()
	}
	if state != "exhausted" || (p.Result.Code != "run_budget" && p.Result.Code != "turn_budget") {
		return 0, fmt.Errorf("continuation_requires_operator: 意图 #%d 保持 %s (%s)，未入队。本工具不能恢复该状态；结束本轮并等待人工处理，恢复前置后通过原意图手动继续，不要换编号重派", id, state, p.Result.Code)
	}
	if p.Checkpoint == nil || p.RunToken == "" || p.Checkpoint.RunToken != p.RunToken || p.Checkpoint.Incomplete || len(p.Checkpoint.Pending) != 0 || !p.Checkpoint.Settled {
		return 0, fmt.Errorf("意图 #%d 的最后工具结果尚不明确，请先核对原执行记录再手动继续", id)
	}
	if p.Continuations >= 2 {
		return 0, fmt.Errorf("意图 #%d 已自动续接2次，请核对验收与剩余工作后手动继续", id)
	}
	var count int
	if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM exploration_nodes WHERE exploration_id=$1 AND kind='intent' AND state='open'`, s.expID).Scan(&count); err != nil {
		return 0, err
	}
	if count >= limit {
		return 0, fmt.Errorf("frontier 已满，等待现有工作；续接未入队")
	}
	change, _ := json.Marshal(map[string]any{"continuation_note": note, "automatic_continuations": p.Continuations + 1})
	if _, err := tx.ExecContext(ctx, `UPDATE exploration_nodes SET state='open',completed_at=NULL,blocked_reason=NULL,payload=payload||$1::jsonb,content_version=content_version+1 WHERE id=$2 AND exploration_id=$3`, string(change), id, s.expID); err != nil {
		return 0, err
	}
	return id, tx.Commit()
}
