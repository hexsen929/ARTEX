package db

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// The caller resolves source-task membership. Lock evidence and goal again while
// committing so a stale inference or rejected finding cannot complete the task.
func (s *ExplorationStore) CommitGoalProof(goal, evidence int64, reason string) error {
	if strings.TrimSpace(reason) == "" {
		return fmt.Errorf("目标达成必须说明证据与验收条件的对应关系")
	}
	if node, err := s.GetNodeWithSources(evidence); err != nil || node == nil {
		return fmt.Errorf("证据不属于本任务或直接关联任务")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var goalState string
	if err := tx.QueryRowContext(ctx, `SELECT state FROM exploration_nodes WHERE id=$1 AND exploration_id=$2 AND kind='goal' FOR UPDATE`, goal, s.expID).Scan(&goalState); err != nil {
		return err
	}
	if goalState != "open" && goalState != "met" {
		return fmt.Errorf("目标已不处于可证明状态")
	}
	var kind, state string
	var raw []byte
	if err := tx.QueryRowContext(ctx, `SELECT kind,state,payload FROM exploration_nodes WHERE id=$1`, evidence).Scan(&kind, &state, &raw); err != nil {
		return err
	}
	var payload map[string]any
	_ = json.Unmarshal(raw, &payload)
	switch kind {
	case KindFact:
		if err := tx.QueryRowContext(ctx, `SELECT state,payload FROM exploration_nodes WHERE id=$1 FOR SHARE`, evidence).Scan(&state, &raw); err != nil {
			return err
		}
		_ = json.Unmarshal(raw, &payload)
		if state != "confirmed" || payload["confidence"] == "inferred" {
			return fmt.Errorf("推断、起点或已排除的事实不能作为目标达成证据，请先复核")
		}
	case KindFinding:
		var verification string
		if err := tx.QueryRowContext(ctx, `SELECT verification_status FROM findings WHERE node_id=$1 FOR SHARE`, evidence).Scan(&verification); err != nil {
			return err
		}
		if verification != VerificationConfirmed {
			return fmt.Errorf("发现尚未确认，不能证明目标达成")
		}
	default:
		return fmt.Errorf("目标证据必须是已核实的事实或发现")
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO exploration_edges(exploration_id,src_id,rel,dst_id) VALUES($1,$2,$3,$4) ON CONFLICT DO NOTHING`, s.expID, evidence, RelProves, goal); err != nil {
		return err
	}
	proof, _ := json.Marshal(map[string]any{"evidence_id": evidence, "reason": strings.TrimSpace(reason)})
	if _, err := tx.ExecContext(ctx, `UPDATE exploration_nodes SET state='met',payload=jsonb_set(payload,'{proof}',$1::jsonb),content_version=content_version+1 WHERE id=$2 AND exploration_id=$3`, string(proof), goal, s.expID); err != nil {
		return err
	}
	return tx.Commit()
}
