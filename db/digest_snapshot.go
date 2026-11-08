package db

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
)

var ErrDigestSnapshotChanged = errors.New("compaction source changed; summary was not saved")

// A summary must describe the same source snapshot at commit time as at model
// input time. Lock its source rows while publishing coverage, including anchors.
func (s *ExplorationStore) AddDigestFromSnapshot(ctx context.Context, payload map[string]any, members []int64, sources map[int64]*Node) (int64, error) {
	if len(members) < 2 {
		return 0, ErrDigestSnapshotChanged
	}
	ids := make([]int64, 0, len(sources))
	for id, node := range sources {
		if node == nil || node.ID != id {
			return 0, ErrDigestSnapshotChanged
		}
		ids = append(ids, id)
	}
	for _, id := range members {
		if sources[id] == nil {
			return 0, ErrDigestSnapshotChanged
		}
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	raw, err := json.Marshal(payload)
	if err != nil {
		return 0, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	rows, err := tx.QueryContext(ctx, `SELECT id,kind,state,payload FROM exploration_nodes WHERE exploration_id=$1 AND id=ANY($2::bigint[]) ORDER BY id FOR SHARE`, s.expID, ids)
	if err != nil {
		return 0, err
	}
	count := 0
	for rows.Next() {
		var id int64
		var kind, state string
		var current []byte
		if err := rows.Scan(&id, &kind, &state, &current); err != nil {
			rows.Close()
			return 0, err
		}
		old := sources[id]
		if old == nil || old.Kind != kind || old.State != state || !bytes.Equal(old.Payload, current) {
			rows.Close()
			return 0, ErrDigestSnapshotChanged
		}
		count++
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return 0, err
	}
	if count != len(ids) {
		return 0, ErrDigestSnapshotChanged
	}
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	var id int64
	if err := tx.QueryRowContext(ctx, `INSERT INTO exploration_nodes(exploration_id,kind,payload,priority,state,origin) VALUES($1,$2,$3,0,$4,'compactor') RETURNING id`, s.expID, KindDigest, string(raw), StateDigestActive).Scan(&id); err != nil {
		return 0, err
	}
	for _, member := range members {
		if member == id {
			return 0, fmt.Errorf("invalid digest member")
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO exploration_edges(exploration_id,src_id,rel,dst_id) VALUES($1,$2,$3,$4) ON CONFLICT DO NOTHING`, s.expID, id, RelCovers, member); err != nil {
			return 0, err
		}
	}
	return id, tx.Commit()
}
