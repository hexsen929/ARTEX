package db

import (
	"context"
	"database/sql"
	"errors"
)

// NextFailedSyncItem reads one raw record at a time to bound memory, including
// records skipped for missing fields. It never selects successful observations.
func (d *DB) NextFailedSyncItem(ctx context.Context, runID, afterID int64) (*SyncObservation, error) {
	item := &SyncObservation{}
	var raw []byte
	err := d.QueryRowContext(ctx, `SELECT id,target,source_target_id,asset_type,item_index,source_asset_id,raw,raw_omitted
FROM sync_observations WHERE run_id=$1 AND id>$2 AND status IN ('failed','skipped') ORDER BY id LIMIT 1`, runID, afterID).
		Scan(&item.ID, &item.Target, &item.SourceTargetID, &item.AssetType, &item.ItemIndex, &item.SourceAssetID, &raw, &item.RawOmitted)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	item.Raw = raw
	return item, err
}
