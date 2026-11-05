package db

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

type FactConfirmation struct {
	Evidence string `json:"verification_evidence"`
	Scope    string `json:"tested_scope"`
	Note     string `json:"verification_note"`
}

// Include state and identity so a confirmation describes the exact fact read by
// the caller, including after concurrent corrections or exclusions.
func FactSnapshotVersion(n *Node) string {
	if n == nil || n.Kind != KindFact {
		return ""
	}
	h := sha256.New()
	fmt.Fprintf(h, "%d\n%s\n", n.ID, n.State)
	h.Write(n.Payload)
	return hex.EncodeToString(h.Sum(nil))
}

// ConfirmFact saves the observed evidence, its tested conditions and the reason
// for the conclusion together. Text is an audit record, not a semantic oracle.
func (s *ExplorationStore) ConfirmFact(ctx context.Context, id int64, origin, expectedVersion string, proof FactConfirmation) error {
	proof.Evidence, proof.Scope, proof.Note = strings.TrimSpace(proof.Evidence), strings.TrimSpace(proof.Scope), strings.TrimSpace(proof.Note)
	if expectedVersion == "" || proof.Evidence == "" || proof.Scope == "" || proof.Note == "" {
		return fmt.Errorf("请先读取 node_detail，再提交 fact_version、verification_evidence、tested_scope、verification_note；只有事实编号不能证明已复核")
	}
	if len([]rune(proof.Evidence)) > 12000 || len([]rune(proof.Scope)) > 2000 || len([]rune(proof.Note)) > 4000 {
		return fmt.Errorf("复核证据最多12000字、适用范围最多2000字、判断依据最多4000字；完整输出保留在产物中并引用位置")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	n := &Node{ID: id, Kind: KindFact}
	if err := tx.QueryRowContext(ctx, `SELECT state,payload FROM exploration_nodes WHERE id=$1 AND exploration_id=$2 AND kind='fact' FOR UPDATE`, id, s.expID).Scan(&n.State, &n.Payload); err != nil {
		return err
	}
	if n.State != "confirmed" || expectedVersion != FactSnapshotVersion(n) {
		return fmt.Errorf("事实内容或状态已变化，请重新读取 node_detail 并核对；未保存确认")
	}
	if origin == "" {
		origin = "system"
	}
	audit, _ := json.Marshal(map[string]any{
		"confidence": "confirmed", "confirmed_by": origin, "confirmed_at": time.Now().UTC().Format(time.RFC3339),
		"confirmed_from_version": expectedVersion, "verification_evidence": proof.Evidence,
		"tested_scope": proof.Scope, "verification_note": proof.Note,
	})
	if _, err := tx.ExecContext(ctx, `UPDATE exploration_nodes SET payload=payload||$1::jsonb,content_version=content_version+1 WHERE id=$2 AND exploration_id=$3`, string(audit), id, s.expID); err != nil {
		return err
	}
	return tx.Commit()
}
