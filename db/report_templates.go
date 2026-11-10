package db

import (
	"context"
	"encoding/json"

	"github.com/Autumn-27/artex/knowledge"
)

type ReportTemplateRow struct {
	ID      int64
	Version int64
	Body    json.RawMessage
}

func (d *DB) ReportTemplates(ctx context.Context) ([]ReportTemplateRow, error) {
	rows, err := d.QueryContext(ctx, `SELECT id,version,body FROM report_templates ORDER BY id LIMIT 32`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []ReportTemplateRow{}
	for rows.Next() {
		var r ReportTemplateRow
		if err := rows.Scan(&r.ID, &r.Version, &r.Body); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}
func (d *DB) SaveReportTemplate(ctx context.Context, r ReportTemplateRow) (ReportTemplateRow, error) {
	if len(r.Body) > 400000 || !json.Valid(r.Body) {
		return r, knowledge.ErrInput
	}
	tx, err := d.BeginTx(ctx, nil)
	if err != nil {
		return r, err
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(7337741026)`); err != nil {
		return r, err
	}
	if r.ID == 0 {
		var count int
		if err = tx.QueryRowContext(ctx, `SELECT count(*) FROM report_templates`).Scan(&count); err != nil {
			return r, err
		}
		if count >= 32 {
			return r, knowledge.ErrQuota
		}
		err = tx.QueryRowContext(ctx, `INSERT INTO report_templates(body) VALUES($1::jsonb) RETURNING id,version`, string(r.Body)).Scan(&r.ID, &r.Version)
	} else {
		err = tx.QueryRowContext(ctx, `UPDATE report_templates SET body=$2::jsonb,version=version+1 WHERE id=$1 AND version=$3 RETURNING version`, r.ID, string(r.Body), r.Version).Scan(&r.Version)
	}
	if err != nil {
		return r, err
	}
	return r, tx.Commit()
}
func (d *DB) DeleteReportTemplate(ctx context.Context, id, version int64) error {
	res, err := d.ExecContext(ctx, `DELETE FROM report_templates WHERE id=$1 AND version=$2`, id, version)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return knowledge.ErrConflict
	}
	return nil
}
