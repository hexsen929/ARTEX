package db

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"time"
)

func (s *ExplorationStore) RunningIntents() ([]*Node, error) {
	rows, err := s.db.Query(`SELECT `+nodeCols+` FROM exploration_nodes WHERE exploration_id=$1 AND kind='intent' AND state='running' ORDER BY id`, s.expID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanNodes(rows)
}

func (s *ExplorationStore) IntentExecutionCounts() (map[string]int, error) {
	rows, err := s.db.Query(`SELECT state,coalesce(payload#>>'{worker_result,code}',blocked_reason,''),count(*) FROM exploration_nodes WHERE exploration_id=$1 AND kind='intent' GROUP BY state,coalesce(payload#>>'{worker_result,code}',blocked_reason,'')`, s.expID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]int{}
	for rows.Next() {
		var state, code string
		var count int
		if err := rows.Scan(&state, &code, &count); err != nil {
			return nil, err
		}
		out[state] += count
		if code != "" && (state == "done" || state == "blocked" || state == "exhausted" || state == "stopped") {
			out["reason:"+code] += count
		}
	}
	return out, rows.Err()
}

// Serialize frontier admission per exploration. A batch or concurrent planners
// cannot overfill it; repeated identical active work returns its existing ID.
func (s *ExplorationStore) AddPlannerIntent(payload map[string]any, priority int, anchors, parents []int64, limit int) (int64, error) {
	summary, _ := payload["summary"].(string)
	if strings.TrimSpace(summary) == "" {
		return 0, fmt.Errorf("summary 不能为空")
	}
	ids := append([]int64{}, anchors...)
	slices.Sort(ids)
	ids = slices.Compact(ids)
	identity, _ := json.Marshal([]any{strings.Join(strings.Fields(summary), " "), ids})
	key := fmt.Sprintf("%x", sha256.Sum256(identity))
	copyPayload := map[string]any{}
	for k, v := range payload {
		copyPayload[k] = v
	}
	copyPayload["dispatch_key"] = key
	checkKey := ""
	if value, ok := payload["check_identity"]; ok {
		encoded, err := json.Marshal(value)
		if err != nil {
			return 0, err
		}
		var identity CheckIdentity
		if err := json.Unmarshal(encoded, &identity); err != nil {
			return 0, err
		}
		normalized, computed, err := identity.Normalize(ids)
		if err != nil {
			return 0, err
		}
		copyPayload["check_identity"], copyPayload["check_key"] = normalized, computed
		checkKey = computed
	}
	raw, _ := json.Marshal(copyPayload)
	anchorJSON, _ := json.Marshal(ids)
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
	var existing int64
	err = tx.QueryRowContext(ctx, `SELECT id FROM exploration_nodes WHERE exploration_id=$1 AND kind='intent'
AND (($5<>'' AND payload->>'check_key'=$5) OR (state IN ('open','running') AND $5='' AND (payload->>'dispatch_key'=$2 OR (payload->>'summary'=$3 AND coalesce(payload->'asset_ids','[]'::jsonb)=$4::jsonb))))
ORDER BY id LIMIT 1`, s.expID, key, summary, string(anchorJSON), checkKey).Scan(&existing)
	if err == nil {
		return existing, tx.Commit()
	}
	if err != sql.ErrNoRows {
		return 0, err
	}
	var count int
	if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM exploration_nodes WHERE exploration_id=$1 AND kind='intent' AND state='open'`, s.expID).Scan(&count); err != nil {
		return 0, err
	}
	if count >= limit {
		return 0, fmt.Errorf("frontier 已满(%d open)，等待现有工作或作废过期open；不要为腾位终止正常运行的Worker", limit)
	}
	var id int64
	if err := tx.QueryRowContext(ctx, `INSERT INTO exploration_nodes(exploration_id,kind,payload,priority,state,origin) VALUES($1,'intent',$2,$3,'open','planner') RETURNING id`, s.expID, string(raw), priority).Scan(&id); err != nil {
		return 0, err
	}
	for _, asset := range ids {
		if _, err := tx.ExecContext(ctx, `INSERT INTO exploration_anchors(node_id,asset_id) VALUES($1,$2) ON CONFLICT DO NOTHING`, id, asset); err != nil {
			return 0, err
		}
	}
	for _, parent := range parents {
		if _, err := tx.ExecContext(ctx, `INSERT INTO exploration_edges(exploration_id,src_id,rel,dst_id) VALUES($1,$2,$3,$4) ON CONFLICT DO NOTHING`, s.expID, parent, RelDerivedFrom, id); err != nil {
			return 0, err
		}
	}
	return id, tx.Commit()
}
