package db

import (
	"context"
	"fmt"
)

// RemainingIntent is a local intent that cannot be presumed complete. Include
// paused and deleted work so the planner can evaluate the operator's decision.
type RemainingIntent struct {
	IntentID      int64  `json:"intent_id"`
	State         string `json:"state"`
	Summary       string `json:"summary"`
	BlockedReason string `json:"blocked_reason,omitempty"`
	DeleteReason  string `json:"delete_reason,omitempty"`
}

type RemainingWorkPage struct {
	Items     []RemainingIntent `json:"items"`
	Total     int               `json:"total"`
	HasMore   bool              `json:"has_more"`
	NextAfter int64             `json:"next_after,omitempty"`
}

// ListRemainingWork uses an ascending ID cursor rather than a bounded recent
// node window. Concurrent state changes remain live; completion revalidates the
// entire graph transactionally before accepting the planner's decisions.
func (s *ExplorationStore) ListRemainingWork(ctx context.Context, after int64, limit int) (*RemainingWorkPage, error) {
	if after < 0 {
		return nil, fmt.Errorf("after must be non-negative")
	}
	if limit <= 0 {
		limit = 100
	}
	if limit > 200 {
		limit = 200
	}
	page := &RemainingWorkPage{Items: []RemainingIntent{}}
	if err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM exploration_nodes
WHERE exploration_id=$1 AND kind='intent' AND state<>'done'`, s.expID).Scan(&page.Total); err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, `SELECT id,state,COALESCE(payload->>'summary',''),
COALESCE(blocked_reason,''),COALESCE(delete_reason,'') FROM exploration_nodes
WHERE exploration_id=$1 AND kind='intent' AND state<>'done' AND id>$2 ORDER BY id LIMIT $3`, s.expID, after, limit+1)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var item RemainingIntent
		if err := rows.Scan(&item.IntentID, &item.State, &item.Summary, &item.BlockedReason, &item.DeleteReason); err != nil {
			return nil, err
		}
		page.Items = append(page.Items, item)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	page.HasMore = len(page.Items) > limit
	if page.HasMore {
		page.Items = page.Items[:limit]
		page.NextAfter = page.Items[len(page.Items)-1].IntentID
	}
	return page, nil
}
