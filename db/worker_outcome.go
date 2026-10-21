package db

import (
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
func (s *ExplorationStore) SetIntentOutcome(id int64, state, code, message string) error {
	raw, _ := json.Marshal(map[string]any{"code": code, "message": message, "at": time.Now().UTC().Format(time.RFC3339)})
	res, err := s.db.Exec(`UPDATE exploration_nodes SET state=$1,
 blocked_reason=CASE WHEN $1='blocked' THEN $2 ELSE NULL END,
 payload=jsonb_set(payload,'{worker_result}',$3::jsonb),completed_at=now(),content_version=content_version+1
 WHERE id=$4 AND exploration_id=$5 AND kind='intent' AND state='running'`, state, code, string(raw), id, s.expID)
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
