package db

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/json"
	"time"
)

type WorkerResult struct {
	Code    string `json:"code"`
	Message string `json:"message"`
	At      string `json:"at"`
}

// SetIntentOutcome atomically records the terminal state and its explanation.
// CAS prevents a late worker from overwriting a pause/delete/control transition.
func (s *ExplorationStore) SetIntentOutcome(id int64, state, code, message string, tokens ...string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	token := "__legacy_unchecked__"
	if len(tokens) != 0 {
		token = tokens[0]
	}
	return s.SetIntentRunOutcome(ctx, id, token, state, code, message)
}

func (s *ExplorationStore) SetIntentRunOutcome(ctx context.Context, id int64, token, state, code, message string) error {
	raw, _ := json.Marshal(map[string]any{"code": code, "message": message, "at": time.Now().UTC().Format(time.RFC3339)})
	res, err := s.db.ExecContext(ctx, `UPDATE exploration_nodes SET state=$1,
 blocked_reason=CASE WHEN $1='blocked' THEN $2 ELSE NULL END,
 payload=jsonb_set(payload,'{worker_result}',$3::jsonb) || CASE WHEN payload#>>'{execution_checkpoint,run_token}'=$6 THEN jsonb_build_object('execution_checkpoint',(payload->'execution_checkpoint')||'{"settled":true}'::jsonb) ELSE '{}'::jsonb END,completed_at=now(),content_version=content_version+1
 WHERE id=$4 AND exploration_id=$5 AND kind='intent' AND state='running'
 AND ($6='__legacy_unchecked__' OR coalesce(payload->>'worker_run_token','')=$6)`, state, code, string(raw), id, s.expID, token)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrIntentStateConflict
	}
	return nil
}

// Each execution gets an opaque generation; a delayed completion cannot settle
// a later rerun of the same intent, even if the worker slot name is reused.
func (s *ExplorationStore) BindIntentRun(ctx context.Context, id int64) (string, error) {
	token := rand.Text()
	cp, _ := json.Marshal(ExecutionCheckpoint{RunToken: token, StartedAt: time.Now().UTC().Format(time.RFC3339Nano), Pending: map[string]string{}})
	var saved string
	err := s.db.QueryRowContext(ctx, `UPDATE exploration_nodes SET payload=payload||jsonb_build_object('worker_run_token',$1::text,'previous_checkpoint',payload->'execution_checkpoint','execution_checkpoint',$4::jsonb,'execution_attempt',coalesce((payload->>'execution_attempt')::integer,0)+1)
WHERE id=$2 AND exploration_id=$3 AND kind='intent' AND state='running' RETURNING payload->>'worker_run_token'`, token, id, s.expID, string(cp)).Scan(&saved)
	if err == sql.ErrNoRows {
		return "", ErrIntentStateConflict
	}
	return saved, err
}

type RunningIntentRun struct {
	ID        int64
	Token     string
	UpdatedAt time.Time
}

func (s *ExplorationStore) RunningIntentRuns(ctx context.Context) ([]RunningIntentRun, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id,coalesce(payload->>'worker_run_token',''),updated_at FROM exploration_nodes
WHERE exploration_id=$1 AND kind='intent' AND state='running'`, s.expID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []RunningIntentRun
	for rows.Next() {
		var item RunningIntentRun
		if err := rows.Scan(&item.ID, &item.Token, &item.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, item)
	}
	return out, rows.Err()
}
