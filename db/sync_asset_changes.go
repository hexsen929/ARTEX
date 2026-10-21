package db

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
)

// SyncAssetChange describes one canonical asset touched by a source record.
// The immutable numeric ID remains useful in history after the live row is gone.
type SyncAssetChange struct {
	AssetID int64  `json:"asset_id"`
	Type    string `json:"type"`
	Outcome string `json:"outcome"`
	Direct  bool   `json:"direct"`
}

type SyncUniqueAssets struct {
	Total     int `json:"total"`
	Created   int `json:"created"`
	Updated   int `json:"updated"`
	Unchanged int `json:"unchanged"`
	Direct    int `json:"direct"`
	Derived   int `json:"derived"`
}

type syncAssetSnapshot struct {
	where string
	args  []any
	id    int64
	kind  string
	value []byte
}

type syncAssetChanges struct {
	before map[string]syncAssetSnapshot
	err    error
}

// captureSyncAsset is a no-op for all existing AssetStore callers. Sync opts in
// inside its item transaction, under the same lock used by normal asset writes.
func (s *AssetStore) captureSyncAsset(where string, args ...any) error {
	if s.syncChanges == nil {
		return nil
	}
	if s.syncChanges.err != nil {
		return s.syncChanges.err
	}
	encoded, err := json.Marshal(args)
	if err != nil {
		s.syncChanges.err = err
		return err
	}
	key := where + string(encoded)
	if _, exists := s.syncChanges.before[key]; exists {
		return nil
	}
	snapshot, err := readSyncAssetSnapshot(s.tx, where, args)
	if err != nil {
		s.syncChanges.err = err
		return err
	}
	s.syncChanges.before[key] = snapshot
	return nil
}

func readSyncAssetSnapshot(tx *sql.Tx, where string, args []any) (syncAssetSnapshot, error) {
	snapshot := syncAssetSnapshot{where: where, args: args}
	var raw []byte
	err := tx.QueryRow(`SELECT id,type,to_jsonb(assets)-'id'-'created_at'-'updated_at'-'last_seen' FROM assets WHERE `+where+` FOR UPDATE`, args...).Scan(&snapshot.id, &snapshot.kind, &raw)
	if errors.Is(err, sql.ErrNoRows) {
		return snapshot, nil
	}
	if err != nil {
		return snapshot, err
	}
	// SQL array union operations do not promise a stable order. These fields
	// are sets in the existing merge contract; their order is not an update.
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return snapshot, err
	}
	for _, name := range []string{"task_ids", "bound_domains", "open_ports", "record_value", "technologies", "auth", "params"} {
		var values []json.RawMessage
		if json.Unmarshal(fields[name], &values) == nil && values != nil {
			sort.Slice(values, func(i, j int) bool { return bytes.Compare(values[i], values[j]) < 0 })
			fields[name], _ = json.Marshal(values)
		}
	}
	snapshot.value, err = json.Marshal(fields)
	return snapshot, err
}

func (s *AssetStore) finishSyncAssetChanges(item *SyncObservation) error {
	if s.syncChanges.err != nil {
		return s.syncChanges.err
	}
	item.Outcome, item.AssetChanges = "", nil
	for _, before := range s.syncChanges.before {
		after, err := readSyncAssetSnapshot(s.tx, before.where, before.args)
		if err != nil {
			return err
		}
		if after.id == 0 {
			return fmt.Errorf("sync asset mutation produced no canonical asset")
		}
		outcome := "unchanged"
		if before.id == 0 {
			outcome = "created"
		} else if !bytes.Equal(before.value, after.value) {
			outcome = "updated"
		}
		direct := item.AssetID != nil && after.id == *item.AssetID
		if direct {
			item.Outcome = outcome
		}
		item.AssetChanges = append(item.AssetChanges, SyncAssetChange{AssetID: after.id, Type: after.kind, Outcome: outcome, Direct: direct})
	}
	sort.Slice(item.AssetChanges, func(i, j int) bool { return item.AssetChanges[i].AssetID < item.AssetChanges[j].AssetID })
	return nil
}

func insertSyncAssetChanges(ctx context.Context, tx *sql.Tx, item *SyncObservation) error {
	for _, change := range item.AssetChanges {
		_, err := tx.ExecContext(ctx, `INSERT INTO sync_asset_changes(run_id,original_asset_id,asset_id,asset_type,outcome,direct)
VALUES($1,$2,$2,$3,$4,$5) ON CONFLICT(run_id,original_asset_id) DO UPDATE SET
outcome=CASE WHEN sync_asset_changes.outcome='created' OR EXCLUDED.outcome='created' THEN 'created'
 WHEN sync_asset_changes.outcome='updated' OR EXCLUDED.outcome='updated' THEN 'updated' ELSE 'unchanged' END,
direct=sync_asset_changes.direct OR EXCLUDED.direct`, item.RunID, change.AssetID, change.Type, change.Outcome, change.Direct)
		if err != nil {
			return err
		}
	}
	return nil
}
