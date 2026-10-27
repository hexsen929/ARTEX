package db

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"unicode/utf8"
)

const (
	VerificationConfirmed  = "confirmed"
	VerificationUnverified = "unverified"
	VerificationBlocked    = "blocked"
	VerificationRejected   = "rejected"
	VerificationLegacy     = "legacy"
)

var ErrVerificationInput = errors.New("invalid finding verification")

func ValidVerificationStatus(s string) bool {
	switch s {
	case VerificationConfirmed, VerificationUnverified, VerificationBlocked, VerificationRejected, VerificationLegacy:
		return true
	}
	return false
}
func VerificationLabel(s string) string {
	switch s {
	case VerificationConfirmed:
		return "已确认"
	case VerificationBlocked:
		return "验证受阻"
	case VerificationRejected:
		return "已排除"
	case VerificationUnverified:
		return "待验证"
	default:
		return "历史未复核"
	}
}
func ValidateVerification(status, note, evidence string) error {
	if !ValidVerificationStatus(status) || utf8.RuneCountInString(note) > 4000 {
		return ErrVerificationInput
	}
	if status == VerificationConfirmed && (strings.TrimSpace(evidence) == "" || strings.TrimSpace(note) == "") {
		return errors.Join(ErrVerificationInput, errors.New("确认漏洞需提供证据和确认依据"))
	}
	if (status == VerificationBlocked || status == VerificationRejected) && strings.TrimSpace(note) == "" {
		return errors.Join(ErrVerificationInput, errors.New("请说明验证受阻或排除的依据"))
	}
	return nil
}

// SetFindingVerification changes verification independently of severity/triage,
// synchronizes the graph and invalidates generated reports in one transaction.
func (d *DB) SetFindingVerification(ctx context.Context, id int64, status, note string, version int64, additionalEvidence ...string) error {
	note = strings.TrimSpace(note)
	return d.WithEvidenceTx(ctx, func(tx *sql.Tx) error {
		return setFindingVerificationTx(ctx, tx, id, status, note, version, additionalEvidence...)
	})
}

// setFindingVerificationTx lets a completed check share the same atomic update,
// evidence-version protection, graph synchronization and audit path as the UI.
func setFindingVerificationTx(ctx context.Context, tx *sql.Tx, id int64, status, note string, version int64, additionalEvidence ...string) error {
	var nodeID sql.NullInt64
	var evidence, from, previousNote string
	var currentVersion int64
	err := tx.QueryRowContext(ctx, `SELECT node_id,evidence,verification_status,verification_note,evidence_version FROM findings WHERE id=$1 FOR UPDATE`, id).Scan(&nodeID, &evidence, &from, &previousNote, &currentVersion)
	if err != nil {
		return err
	}
	if currentVersion != version {
		return ErrEvidenceConflict
	}
	addition := ""
	if len(additionalEvidence) > 0 {
		addition = strings.TrimSpace(additionalEvidence[0])
	}
	if utf8.RuneCountInString(addition) > 32000 {
		return ErrVerificationInput
	}
	if addition != "" {
		evidence = strings.TrimSpace(evidence + "\n\n补充验证证据：\n" + addition)
	}
	if err := ValidateVerification(status, note, evidence); err != nil {
		return err
	}
	if from == status && previousNote == note && addition == "" {
		return nil
	}
	if _, err := tx.ExecContext(ctx, `UPDATE findings SET verification_status=$2,verification_note=$3,evidence=$4,evidence_version=evidence_version+1 WHERE id=$1`, id, status, note, evidence); err != nil {
		return err
	}
	// Changes to a member invalidate the unified case report as well.
	if _, err := tx.ExecContext(ctx, `UPDATE finding_cases SET version=version+1 WHERE id IN (SELECT case_id FROM finding_case_members WHERE finding_id=$1)`, id); err != nil {
		return err
	}
	if nodeID.Valid {
		data, _ := json.Marshal(map[string]string{"verification_status": status, "verification_note": note})
		if addition != "" {
			if _, err := tx.ExecContext(ctx, `UPDATE exploration_nodes SET payload=jsonb_set(payload,'{evidence}',jsonb_build_object('poc',$2::text,'by',COALESCE(payload#>>'{evidence,by}','verification'))) WHERE id=$1`, nodeID.Int64, evidence); err != nil {
				return err
			}
		}
		if _, err := tx.ExecContext(ctx, `UPDATE exploration_nodes SET payload=payload || $2::jsonb,content_version=content_version+1 WHERE id=$1`, nodeID.Int64, string(data)); err != nil {
			return err
		}
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO finding_verification_events(finding_id,from_status,to_status,note) VALUES($1,$2,$3,$4)`, id, from, status, note)
	return err
}

// Task-scoped tool path: inherited findings are read-only.
func (s *ExplorationStore) VerifyFinding(ctx context.Context, id int64, status, note string, version int64, addition string) (*DBFinding, error) {
	f, err := s.db.GetFinding(id)
	if err != nil {
		return nil, err
	}
	if f == nil || f.NodeID == nil {
		return nil, ErrFindingNotFound
	}
	var own bool
	if err := s.db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM exploration_nodes WHERE id=$1 AND exploration_id=$2 AND kind='finding')`, *f.NodeID, s.expID).Scan(&own); err != nil {
		return nil, err
	}
	if !own {
		return nil, ErrFindingNotFound
	}
	if err := s.db.SetFindingVerification(ctx, id, status, note, version, addition); err != nil {
		return nil, err
	}
	return s.db.GetFinding(id)
}
