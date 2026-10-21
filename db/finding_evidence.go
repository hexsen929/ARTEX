package db

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// UpdateFindingEvidence preserves the previous evidence and replaces its source
// references in the same transaction. expectedTaskID restricts task agents to
// their own findings; inherited findings remain read-only.
func (d *DB) UpdateFindingEvidence(ctx context.Context, findingID, version, expectedTaskID int64, evidence, actor, reason string, factIDs []int64) (next int64, err error) {
	if findingID <= 0 || version < 0 || strings.TrimSpace(evidence) == "" || strings.TrimSpace(actor) == "" || strings.TrimSpace(reason) == "" || len(factIDs) > 20 {
		return 0, errors.New("补证需要有效记录、版本、完整 evidence、来源与原因；事实引用最多 20 条")
	}
	refs, err := json.Marshal(factIDs)
	if err != nil {
		return 0, err
	}
	if factIDs == nil {
		refs = []byte("[]")
	}
	err = d.WithEvidenceTx(ctx, func(tx *sql.Tx) error {
		if err := LockFindingEvidenceTx(tx, findingID, &version); err != nil {
			return err
		}
		var owner, nodeID, expID sql.NullInt64
		if err := tx.QueryRowContext(ctx, `SELECT f.task_id,f.node_id,n.exploration_id FROM findings f LEFT JOIN exploration_nodes n ON n.id=f.node_id WHERE f.id=$1`, findingID).Scan(&owner, &nodeID, &expID); err != nil {
			return err
		}
		if expectedTaskID > 0 && owner.Int64 != expectedTaskID {
			return errors.New("只能补充当前任务自己的发现，继承记录只读")
		}
		seen := map[int64]bool{}
		for _, id := range factIDs {
			if id <= 0 || seen[id] || !nodeID.Valid || !expID.Valid {
				return errors.New("事实引用必须唯一且关联到有效发现节点")
			}
			seen[id] = true
			var allowed bool
			if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM exploration_nodes n WHERE n.id=$1 AND n.kind='fact' AND n.state='confirmed' AND (n.exploration_id=$2 OR n.exploration_id IN (SELECT source.exploration_id FROM task_relations r JOIN tasks source ON source.id=r.source_task_id AND source.deleted_at IS NULL WHERE r.task_id=$3)))`, id, expID.Int64, owner.Int64).Scan(&allowed); err != nil {
				return err
			}
			if !allowed {
				return fmt.Errorf("事实 %d 不属于本任务或可读的直接来源任务", id)
			}
		}
		if _, err := tx.ExecContext(ctx, `UPDATE findings SET evidence_audit=evidence_audit || jsonb_build_array(jsonb_build_object('at',now(),'actor',$2::text,'reason',$3::text,'previous_version',evidence_version,'previous_evidence',evidence,'previous_node_payload',(SELECT payload FROM exploration_nodes WHERE id=findings.node_id))),evidence=$4,evidence_version=evidence_version+1 WHERE id=$1`, findingID, actor, reason, evidence); err != nil {
			return err
		}
		if nodeID.Valid && expID.Valid {
			if _, err := tx.ExecContext(ctx, `UPDATE exploration_nodes SET payload=payload || jsonb_build_object('evidence',$2::text,'source_fact_ids',$3::jsonb),content_version=content_version+1 WHERE id=$1 AND kind='finding'`, nodeID.Int64, evidence, string(refs)); err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, `DELETE FROM exploration_edges WHERE exploration_id=$1 AND dst_id=$2 AND rel='supports'`, expID.Int64, nodeID.Int64); err != nil {
				return err
			}
			for _, id := range factIDs {
				if _, err := tx.ExecContext(ctx, `INSERT INTO exploration_edges(exploration_id,src_id,rel,dst_id) VALUES($1,$2,'supports',$3) ON CONFLICT DO NOTHING`, expID.Int64, id, nodeID.Int64); err != nil {
					return err
				}
			}
		}
		return tx.QueryRowContext(ctx, `SELECT evidence_version FROM findings WHERE id=$1`, findingID).Scan(&next)
	})
	return
}

// Supporting facts are read from saved rows, never from a model's claim text.
// Source-task removal or a changed fact therefore invalidates a saved verdict.
const supportingFactsSQL = `(SELECT COALESCE(jsonb_agg(jsonb_build_object('id',sf.id,'exploration_id',sf.exploration_id,'state',sf.state,'payload',sf.payload,'content_version',sf.content_version) ORDER BY sf.id),'[]'::jsonb) FROM exploration_edges se JOIN exploration_nodes sf ON sf.id=se.src_id WHERE se.dst_id=ev.id AND se.rel='supports' AND sf.kind='fact' AND sf.state='confirmed' AND (sf.exploration_id=owner.exploration_id OR sf.exploration_id IN (SELECT source.exploration_id FROM task_relations r JOIN tasks source ON source.id=r.source_task_id AND source.deleted_at IS NULL WHERE r.task_id=owner.id)))`
