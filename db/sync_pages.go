package db

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
)

type SyncPage struct {
	Items []json.RawMessage
	Count int
	Done  bool
	Last  bool
}

func (d *DB) GetSyncPage(ctx context.Context, runID int64, target, kind string, page int) (*SyncPage, error) {
	var raw []byte
	p := &SyncPage{}
	err := d.QueryRowContext(ctx, `SELECT items,item_count,done,last_page FROM sync_pages WHERE run_id=$1 AND target=$2 AND asset_type=$3 AND page=$4`, runID, target, kind, page).Scan(&raw, &p.Count, &p.Done, &p.Last)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return p, json.Unmarshal(raw, &p.Items)
}

func (d *DB) SaveSyncPage(ctx context.Context, runID int64, target, kind string, page int, p *SyncPage) error {
	raw, err := json.Marshal(p.Items)
	if err != nil {
		return err
	}
	tx, err := d.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	res, err := tx.ExecContext(ctx, `INSERT INTO sync_pages(run_id,target,asset_type,page,items,last_page,item_count) VALUES($1,$2,$3,$4,$5,$6,$7) ON CONFLICT DO NOTHING`, runID, target, kind, page, string(raw), p.Last, len(p.Items))
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n != 0 {
		res, err = tx.ExecContext(ctx, `UPDATE sync_runs SET fetched=fetched+$2 WHERE id=$1 AND status='running'`, runID, len(p.Items))
		if err := checkSyncRunUpdate(res, err); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (d *DB) FinishSyncPage(ctx context.Context, runID int64, target, kind string, page int) error {
	_, err := d.ExecContext(ctx, `UPDATE sync_pages SET done=true,items='[]' WHERE run_id=$1 AND target=$2 AND asset_type=$3 AND page=$4`, runID, target, kind, page)
	return err
}

func (d *DB) SyncItemRecorded(ctx context.Context, runID int64, target, kind string, index int) (bool, error) {
	var exists bool
	err := d.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM sync_observations WHERE run_id=$1 AND target=$2 AND asset_type=$3 AND item_index=$4)`, runID, target, kind, index).Scan(&exists)
	return exists, err
}

func (d *DB) ResumeSyncRun(ctx context.Context, id int64) error {
	res, err := d.ExecContext(ctx, `UPDATE sync_runs SET status='running',finished_at=NULL WHERE id=$1 AND status IN ('interrupted','failed','partial')`, id)
	return checkSyncRunUpdate(res, err)
}
