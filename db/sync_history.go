package db

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// SyncStats counts source-record outcomes; unique canonical assets are separate.
type SyncStats struct {
	Fetched          int `json:"fetched"`
	Written          int `json:"written"`
	Created          int `json:"created"`
	Updated          int `json:"updated"`
	Unchanged        int `json:"unchanged"`
	Skipped          int `json:"skipped"`
	Failed           int `json:"failed"`
	DerivedIPWritten int `json:"derived_ip_written"`
	DerivedIPFailed  int `json:"derived_ip_failed"`
}

type SyncRun struct {
	ID          int64           `json:"id"`
	SourceMCPID *int64          `json:"source_mcp_id,omitempty"`
	Status      string          `json:"status"`
	Request     json.RawMessage `json:"request"`
	Result      json.RawMessage `json:"result"`
	StartedAt   time.Time       `json:"started_at"`
	FinishedAt  *time.Time      `json:"finished_at,omitempty"`
}

type SyncObservation struct {
	ID               int64             `json:"id"`
	RunID            int64             `json:"run_id"`
	Target           string            `json:"target"`
	SourceTargetID   string            `json:"source_target_id,omitempty"`
	AssetType        string            `json:"asset_type"`
	ItemIndex        int               `json:"item_index"`
	SourceAssetID    string            `json:"source_asset_id,omitempty"`
	Status           string            `json:"status"`
	Outcome          string            `json:"outcome,omitempty"`
	Reason           string            `json:"reason,omitempty"`
	AssetID          *int64            `json:"asset_id,omitempty"`
	AssetChanges     []SyncAssetChange `json:"asset_changes,omitempty"`
	DerivedIPWritten int               `json:"derived_ip_written"`
	DerivedIPFailed  int               `json:"derived_ip_failed"`
	Raw              json.RawMessage   `json:"raw,omitempty"`
	RawOmitted       bool              `json:"raw_omitted"`
}

// Raw responses live outside assets.extra, which is also exposed to Agent tools.
const SyncObservationRawLimit = 16 << 20

var ErrSyncRunInactive = errors.New("sync run is no longer running")

const syncRunColumns = `id, source_mcp_id, status, request,
result || jsonb_build_object('stats',jsonb_build_object(
 'fetched',fetched,'written',written,'skipped',skipped,'failed',failed,
 'created',created,'updated',updated,'unchanged',unchanged,
 'derived_ip_written',derived_ip_written,'derived_ip_failed',derived_ip_failed),
 'unique_assets',(SELECT jsonb_build_object('total',count(*),
 'created',count(*) FILTER(WHERE outcome='created'),'updated',count(*) FILTER(WHERE outcome='updated'),
 'unchanged',count(*) FILTER(WHERE outcome='unchanged'),'direct',count(*) FILTER(WHERE direct),
 'derived',count(*) FILTER(WHERE NOT direct)) FROM sync_asset_changes WHERE run_id=sync_runs.id),
 'targets',COALESCE((SELECT jsonb_agg(t || jsonb_build_object('stats',
 COALESCE(t->'stats','{}'::jsonb) || (SELECT jsonb_build_object(
 'written',count(*) FILTER(WHERE o.status='written'),
 'skipped',count(*) FILTER(WHERE o.status='skipped'),'failed',count(*) FILTER(WHERE o.status='failed'),
 'created',count(*) FILTER(WHERE o.outcome='created'),'updated',count(*) FILTER(WHERE o.outcome='updated'),
 'unchanged',count(*) FILTER(WHERE o.outcome='unchanged'),
 'derived_ip_written',COALESCE(sum(o.derived_ip_written),0),'derived_ip_failed',COALESCE(sum(o.derived_ip_failed),0))
 FROM sync_observations o WHERE o.run_id=sync_runs.id AND o.target=t->>'target' AND o.asset_type=t->>'asset_type')))
 FROM jsonb_array_elements(COALESCE(result->'targets','[]'::jsonb)) t),'[]'::jsonb),
 'synced', '{"subdomain":0,"app":0,"service":0}'::jsonb ||
 COALESCE((SELECT jsonb_object_agg(counts.asset_type,counts.n) FROM (
 SELECT asset_type,count(*) AS n FROM sync_observations
 WHERE run_id=sync_runs.id AND status='written' GROUP BY asset_type
 ) counts),'{}'::jsonb) || jsonb_build_object('ip',derived_ip_written)),
started_at, finished_at`

func scanSyncRun(row interface{ Scan(...any) error }) (*SyncRun, error) {
	var run SyncRun
	if err := row.Scan(&run.ID, &run.SourceMCPID, &run.Status, &run.Request, &run.Result, &run.StartedAt, &run.FinishedAt); err != nil {
		return nil, err
	}
	return &run, nil
}

func (d *DB) CreateSyncRun(ctx context.Context, sourceID *int64, request, result json.RawMessage) (*SyncRun, error) {
	return scanSyncRun(d.QueryRowContext(ctx, `INSERT INTO sync_runs(source_mcp_id,request,result)
VALUES($1,$2,$3) RETURNING `+syncRunColumns, sourceID, string(request), string(result)))
}

func (d *DB) UpdateSyncRun(ctx context.Context, id int64, status string, result json.RawMessage) error {
	switch status {
	case "running", "success", "partial", "failed", "interrupted":
	default:
		return fmt.Errorf("invalid sync status %q", status)
	}
	res, err := d.ExecContext(ctx, `UPDATE sync_runs SET status=$2,result=$3,
finished_at=CASE WHEN $2='running' THEN NULL ELSE now() END WHERE id=$1 AND status='running'`, id, status, string(result))
	return checkSyncRunUpdate(res, err)
}

func checkSyncRunUpdate(res sql.Result, err error) error {
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err == nil && n == 0 {
		return ErrSyncRunInactive
	}
	return err
}

func (d *DB) AddSyncFetched(ctx context.Context, id int64, count int) error {
	res, err := d.ExecContext(ctx, `UPDATE sync_runs SET fetched=fetched+$2 WHERE id=$1 AND status='running'`, id, count)
	return checkSyncRunUpdate(res, err)
}

// RecoverSyncRuns is called once before this server accepts requests, matching
// the application's single-process recovery model. It never replays source data.
func (d *DB) RecoverSyncRuns(ctx context.Context) (int64, error) {
	res, err := d.ExecContext(ctx, `UPDATE sync_runs SET status='interrupted',finished_at=now(),
result=jsonb_set(result || '{"status":"interrupted"}'::jsonb,'{errors}',
COALESCE(result->'errors','[]'::jsonb) || jsonb_build_array('同步进程已退出；已提交记录保留，本轮未完成'))
WHERE status='running'`)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

func (d *DB) GetSyncRun(ctx context.Context, id int64) (*SyncRun, error) {
	run, err := scanSyncRun(d.QueryRowContext(ctx, `SELECT `+syncRunColumns+` FROM sync_runs WHERE id=$1`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return run, err
}

func (d *DB) ListSyncRuns(ctx context.Context, limit, offset int) ([]*SyncRun, int, error) {
	var total int
	if err := d.QueryRowContext(ctx, `SELECT count(*) FROM sync_runs`).Scan(&total); err != nil {
		return nil, 0, err
	}
	rows, err := d.QueryContext(ctx, `SELECT `+syncRunColumns+` FROM sync_runs ORDER BY id DESC LIMIT $1 OFFSET $2`, limit, offset)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	runs := []*SyncRun{}
	for rows.Next() {
		run, err := scanSyncRun(rows)
		if err != nil {
			return nil, 0, err
		}
		runs = append(runs, run)
	}
	return runs, total, rows.Err()
}

// ApplySyncObservation atomically commits an asset mutation, its provenance,
// and the run's counters. Only this sync-specific entry point uses the callback;
// existing Upsert callers retain their signatures and merge semantics.
// A mutation failure rolls back the source item and all its derived assets. The
// caller can then persist a failed observation through RecordSyncObservation.
func (d *DB) ApplySyncObservation(ctx context.Context, item *SyncObservation, apply func(*AssetStore) error) error {
	tx, err := d.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck
	if err := lockCompanyScopeMutation(tx); err != nil {
		return err
	}
	store := &AssetStore{db: d, company: d.Companies(), tx: tx,
		syncChanges: &syncAssetChanges{before: map[string]syncAssetSnapshot{}}}
	if err := apply(store); err != nil {
		return err
	}
	if err := store.finishSyncAssetChanges(item); err != nil {
		return err
	}
	if err := insertSyncObservation(ctx, tx, item); err != nil {
		return err
	}
	if err := insertSyncAssetChanges(ctx, tx, item); err != nil {
		return err
	}
	return tx.Commit()
}

func (d *DB) RecordSyncObservation(ctx context.Context, item *SyncObservation) error {
	tx, err := d.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck
	if err := insertSyncObservation(ctx, tx, item); err != nil {
		return err
	}
	return tx.Commit()
}

func insertSyncObservation(ctx context.Context, tx *sql.Tx, item *SyncObservation) error {
	var raw any
	if len(item.Raw) > SyncObservationRawLimit {
		item.RawOmitted = true
	} else if len(item.Raw) > 0 {
		raw = string(item.Raw)
	}
	changes, err := json.Marshal(item.AssetChanges)
	if err != nil {
		return err
	}
	err = tx.QueryRowContext(ctx, `INSERT INTO sync_observations
(run_id,target,source_target_id,asset_type,item_index,source_asset_id,status,reason,asset_id,
derived_ip_written,derived_ip_failed,raw,raw_omitted,outcome,asset_changes)
VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15) RETURNING id`,
		item.RunID, item.Target, item.SourceTargetID, item.AssetType, item.ItemIndex, item.SourceAssetID,
		item.Status, item.Reason, item.AssetID, item.DerivedIPWritten, item.DerivedIPFailed, raw, item.RawOmitted,
		item.Outcome, string(changes)).Scan(&item.ID)
	if err != nil {
		return err
	}
	res, err := tx.ExecContext(ctx, `UPDATE sync_runs SET
written=written+CASE WHEN $2='written' THEN 1 ELSE 0 END,
skipped=skipped+CASE WHEN $2='skipped' THEN 1 ELSE 0 END,
failed=failed+CASE WHEN $2='failed' THEN 1 ELSE 0 END,
created=created+CASE WHEN $5='created' THEN 1 ELSE 0 END,
updated=updated+CASE WHEN $5='updated' THEN 1 ELSE 0 END,
unchanged=unchanged+CASE WHEN $5='unchanged' THEN 1 ELSE 0 END,
derived_ip_written=derived_ip_written+$3,derived_ip_failed=derived_ip_failed+$4
WHERE id=$1 AND status='running'`, item.RunID, item.Status, item.DerivedIPWritten, item.DerivedIPFailed, item.Outcome)
	return checkSyncRunUpdate(res, err)
}

func (d *DB) ListSyncObservations(ctx context.Context, runID int64, limit, offset int, includeRaw bool) ([]*SyncObservation, int, error) {
	var total int
	if err := d.QueryRowContext(ctx, `SELECT count(*) FROM sync_observations WHERE run_id=$1`, runID).Scan(&total); err != nil {
		return nil, 0, err
	}
	rawColumn := "NULL::jsonb"
	if includeRaw {
		rawColumn = "raw"
	}
	rows, err := d.QueryContext(ctx, `SELECT id,run_id,target,source_target_id,asset_type,item_index,
source_asset_id,status,reason,asset_id,derived_ip_written,derived_ip_failed,`+rawColumn+`,raw_omitted,outcome,asset_changes
FROM sync_observations WHERE run_id=$1 ORDER BY id LIMIT $2 OFFSET $3`, runID, limit, offset)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	items := []*SyncObservation{}
	for rows.Next() {
		var item SyncObservation
		var changes, raw []byte
		if err := rows.Scan(&item.ID, &item.RunID, &item.Target, &item.SourceTargetID, &item.AssetType,
			&item.ItemIndex, &item.SourceAssetID, &item.Status, &item.Reason, &item.AssetID,
			&item.DerivedIPWritten, &item.DerivedIPFailed, &raw, &item.RawOmitted, &item.Outcome, &changes); err != nil {
			return nil, 0, err
		}
		item.Raw = json.RawMessage(raw)
		if err := json.Unmarshal(changes, &item.AssetChanges); err != nil {
			return nil, 0, err
		}
		items = append(items, &item)
	}
	return items, total, rows.Err()
}
