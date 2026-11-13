package db

import (
	"context"
	"database/sql"
	"encoding/json"
	"time"
)

// A checkpoint describes tool dispatch/return, not whether a network request or
// vulnerability test actually succeeded. Full content stays in activity/transcript.
type ExecutionCheckpoint struct {
	NotExecuted    map[string]string `json:"not_executed,omitempty"`
	RunToken       string            `json:"run_token"`
	StartedAt      string            `json:"started_at"`
	UpdatedAt      string            `json:"updated_at"`
	ToolCalls      int               `json:"tool_calls"`
	ToolResults    int               `json:"tool_results"`
	Pending        map[string]string `json:"pending"`
	LastTool       string            `json:"last_tool,omitempty"`
	LastActivityID int64             `json:"last_activity_id,omitempty"`
	LastResultID   int64             `json:"last_result_id,omitempty"`
	Incomplete     bool              `json:"incomplete,omitempty"`
	Settled        bool              `json:"settled"`
}

type activityRowWriter interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

func (s *ExplorationStore) appendActivityRow(ctx context.Context, writer activityRowWriter, a Activity) (int64, error) {
	metadata := a.Metadata
	if len(metadata) == 0 {
		metadata = json.RawMessage(`{}`)
	}
	var id int64
	err := writer.QueryRowContext(ctx, `INSERT INTO activity(exploration_id,node_id,worker,kind,tool,tool_use_id,is_error,summary,detail,metadata,input_tokens,output_tokens,cache_read_tokens,cache_write_tokens,main_seg)
VALUES($1,$2,NULLIF($3,''),NULLIF($4,''),NULLIF($5,''),NULLIF($6,''),$7,NULLIF($8,''),NULLIF($9,''),$10,$11,$12,$13,$14,$15) RETURNING id`, s.expID, a.NodeID, utf8Clean(a.Worker), utf8Clean(a.Kind), utf8Clean(a.Tool), utf8Clean(a.ToolUseID), a.IsError, utf8Clean(a.Summary), utf8Clean(a.Detail), metadata, a.InputTokens, a.OutputTokens, a.CacheReadTokens, a.CacheWriteTokens, a.MainSeg).Scan(&id)
	return id, err
}

func (s *ExplorationStore) appendCheckpointActivity(a Activity, token string) (int64, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	var raw []byte
	var state, currentToken string
	err = tx.QueryRowContext(ctx, `SELECT state,coalesce(payload->>'worker_run_token',''),coalesce(payload->'execution_checkpoint','{}'::jsonb) FROM exploration_nodes WHERE id=$1 AND exploration_id=$2 AND kind='intent' FOR UPDATE`, a.NodeID, s.expID).Scan(&state, &currentToken, &raw)
	if err != nil {
		return 0, err
	}
	if a.ToolUseID != "" {
		var existing int64
		err := tx.QueryRowContext(ctx, `SELECT id FROM activity WHERE exploration_id=$1 AND node_id=$2 AND kind=$3 AND tool_use_id=$4 AND metadata->>'worker_run_token'=$5 ORDER BY id DESC LIMIT 1`, s.expID, a.NodeID, a.Kind, a.ToolUseID, token).Scan(&existing)
		if err == nil {
			return existing, tx.Commit()
		}
		if err != sql.ErrNoRows {
			return 0, err
		}
	}
	id, err := s.appendActivityRow(ctx, tx, a)
	if err != nil {
		return 0, err
	}
	// Late events remain in the audit trail, but cannot change a newer execution.
	if state == "running" && currentToken == token {
		var cp ExecutionCheckpoint
		if err := json.Unmarshal(raw, &cp); err != nil {
			return 0, err
		}
		if cp.RunToken != token {
			cp = ExecutionCheckpoint{RunToken: token, Incomplete: true}
		}
		if cp.Pending == nil {
			cp.Pending = map[string]string{}
		}
		cp.UpdatedAt, cp.LastActivityID, cp.LastTool = time.Now().UTC().Format(time.RFC3339Nano), id, a.Tool
		if a.ToolUseID == "" {
			cp.Incomplete = true
		}
		if a.Kind == "tool_use" {
			cp.ToolCalls++
			if len(cp.Pending) < 64 && a.ToolUseID != "" {
				cp.Pending[a.ToolUseID] = a.Tool
			} else {
				cp.Incomplete = true
			}
		} else {
			cp.ToolResults++
			if _, ok := cp.Pending[a.ToolUseID]; !ok {
				cp.Incomplete = true
			}
			delete(cp.Pending, a.ToolUseID)
			cp.LastResultID = id
		}
		encoded, _ := json.Marshal(cp)
		if _, err := tx.ExecContext(ctx, `UPDATE exploration_nodes SET payload=jsonb_set(payload,'{execution_checkpoint}',$1::jsonb)||'{"execution_history_required":true}'::jsonb WHERE id=$2 AND exploration_id=$3`, string(encoded), a.NodeID, s.expID); err != nil {
			return 0, err
		}
	}
	return id, tx.Commit()
}
