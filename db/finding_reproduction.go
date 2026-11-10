package db

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"unicode/utf8"
)

// Reproduction records observed actions independently of prose reports. Traffic
// IDs must already belong to this finding's evidence snapshots; never inferred.
type Reproduction struct {
	EntryPoint    string             `json:"entry_point"`
	Prerequisites []string           `json:"prerequisites"`
	Steps         []ReproductionStep `json:"steps"`
	Provenance    string             `json:"provenance"`
}
type ReproductionStep struct {
	Title      string   `json:"title"`
	Request    string   `json:"request"`
	Response   string   `json:"response"`
	Expected   string   `json:"expected"`
	Proves     string   `json:"proves"`
	TrafficIDs []string `json:"traffic_ids"`
}

var ErrReproduction = errors.New("复现步骤无效、过大或包含未绑定的流量 ID")

func (r *Reproduction) Validate() error {
	if r == nil {
		return nil
	}
	text := func(s string, n int) bool { return len(s) <= n && utf8.ValidString(s) && !strings.ContainsRune(s, 0) }
	if !text(r.EntryPoint, 2000) || !text(r.Provenance, 4000) || len(r.Prerequisites) > 16 || len(r.Steps) < 1 || len(r.Steps) > 32 {
		return ErrReproduction
	}
	for _, p := range r.Prerequisites {
		if !text(p, 2000) {
			return ErrReproduction
		}
	}
	for _, s := range r.Steps {
		if strings.TrimSpace(s.Title) == "" || !text(s.Title, 500) || !text(s.Request, 16000) || !text(s.Response, 16000) || !text(s.Expected, 2000) || !text(s.Proves, 2000) || len(s.TrafficIDs) > 8 {
			return ErrReproduction
		}
		for _, id := range s.TrafficIDs {
			if strings.TrimSpace(id) == "" || !text(id, 256) {
				return ErrReproduction
			}
		}
	}
	raw, err := json.Marshal(r)
	if err != nil || len(raw) > 128<<10 {
		return ErrReproduction
	}
	return nil
}

func saveReproductionTx(ctx context.Context, tx *sql.Tx, id int64, r *Reproduction) error {
	if r == nil {
		return nil
	}
	if err := r.Validate(); err != nil {
		return err
	}
	seen := map[string]bool{}
	for _, step := range r.Steps {
		for _, ref := range step.TrafficIDs {
			if seen[ref] {
				continue
			}
			seen[ref] = true
			var exists bool
			if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM finding_traffic_bindings b JOIN traffic_evidence_snapshots s ON s.id=b.snapshot_id WHERE b.finding_id=$1 AND s.source_traffic_id=$2)`, id, ref).Scan(&exists); err != nil {
				return err
			}
			if !exists {
				return ErrReproduction
			}
		}
	}
	raw, _ := json.Marshal(r)
	var nodeID sql.NullInt64
	err := tx.QueryRowContext(ctx, `UPDATE findings SET reproduction=$2::jsonb,evidence_version=evidence_version+1 WHERE id=$1 RETURNING node_id`, id, string(raw)).Scan(&nodeID)
	if err != nil {
		return err
	}
	if nodeID.Valid {
		_, err = tx.ExecContext(ctx, `UPDATE exploration_nodes SET payload=jsonb_set(payload,'{reproduction}',$2::jsonb),updated_at=now() WHERE id=$1 AND kind='finding'`, nodeID.Int64, string(raw))
	}
	return err
}

func (d *DB) SaveFindingReproduction(ctx context.Context, id, version int64, r *Reproduction) error {
	if r == nil || r.Validate() != nil {
		return ErrReproduction
	}
	tx, err := d.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var taskID sql.NullInt64
	if err = tx.QueryRowContext(ctx, `SELECT task_id FROM findings WHERE id=$1`, id).Scan(&taskID); err != nil {
		return err
	}
	if taskID.Valid {
		if err = LockTaskEvidenceTx(tx, taskID.Int64); err != nil {
			return err
		}
	}
	var current int64
	if err = tx.QueryRowContext(ctx, `SELECT evidence_version FROM findings WHERE id=$1 FOR UPDATE`, id).Scan(&current); err != nil {
		return err
	}
	if current != version {
		return ErrEvidenceConflict
	}
	if err = saveReproductionTx(ctx, tx, id, r); err != nil {
		return err
	}
	return tx.Commit()
}
