package db

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
)

// The archive never contains jwt.key. This domain-separated fingerprint lets the
// server reject a different encryption key before restoring opaque ciphertext.
func ArchiveIntranetKeyFingerprint(masterKey []byte) string {
	if len(masterKey) == 0 {
		return ""
	}
	sum := sha256.Sum256(append([]byte("artex/archive-intranet/v1\x00"), masterKey...))
	return hex.EncodeToString(sum[:])
}

func (s *TaskArchiveSnapshot) HasIntranetSecrets() bool {
	return s != nil && (rawRowCount(s.Tables["sessions"]) > 0 || rawRowCount(s.Tables["credentials"]) > 0)
}

func restoreIntranetArchiveRows(tx *sql.Tx, taskID int64, tables map[string]json.RawMessage) error {
	archivedSessions := map[int64]bool{}
	sessionRows, err := decodeArchiveRows(tables["sessions"])
	if err != nil {
		return err
	}
	for _, row := range sessionRows {
		id, ok := jsonInt64(row["id"])
		if !ok || id <= 0 {
			return errors.New("invalid archived session id")
		}
		archivedSessions[id] = true
	}
	// Global dead/stopped resources may remain shared with other tasks. Never
	// overwrite them or resurrect a live network process from an archive.
	for _, table := range []string{"sessions", "tunnels"} {
		rows, err := decodeArchiveRows(tables[table])
		if err != nil {
			return err
		}
		for _, row := range rows {
			id, ok := jsonInt64(row["id"])
			if !ok || id <= 0 {
				return errors.New("invalid archived resource id")
			}
			var existing []byte
			err = tx.QueryRow(`SELECT row_to_json(r) FROM `+table+` r WHERE id=$1 FOR UPDATE`, id).Scan(&existing)
			if err == nil {
				var current map[string]any
				if json.Unmarshal(existing, &current) != nil {
					return errors.New("invalid current resource")
				}
				if table == "sessions" {
					// A collision is only reusable when it is the same encrypted
					// session, not an unrelated object allocated with the same id.
					currentTask, _ := jsonInt64(current["created_by_task"])
					archivedTask, _ := jsonInt64(row["created_by_task"])
					if current["kind"] != row["kind"] || current["url"] != row["url"] || current["secret"] != row["secret"] || currentTask != archivedTask {
						return fmt.Errorf("archived session %d conflicts with an existing resource", id)
					}
				} else {
					owner, _ := jsonInt64(current["task_id"])
					if owner != taskID {
						return fmt.Errorf("archived tunnel %d belongs to a different task", id)
					}
					if current["state"] != "stopped" {
						return fmt.Errorf("existing tunnel %d must be stopped before restoring", id)
					}
				}
				continue
			}
			if !errors.Is(err, sql.ErrNoRows) {
				return err
			}
			if table == "sessions" {
				row["status"] = SessionDead
			} else {
				owner, _ := jsonInt64(row["task_id"])
				if owner != taskID {
					return errors.New("archived tunnel task mismatch")
				}
				row["state"] = "stopped"
				row["remote_pid"] = 0
				row["error"] = "从归档恢复，需人工重新建立隧道"
			}
			raw, err := json.Marshal(row)
			if err != nil {
				return err
			}
			if _, err = tx.Exec(`INSERT INTO `+table+` SELECT * FROM json_populate_record(NULL::`+table+`,$1::json)`, string(raw)); err != nil {
				return err
			}
		}
	}
	for _, table := range []string{"credentials", "pending_scope", "task_session_bindings"} {
		rows, err := decodeArchiveRows(tables[table])
		if err != nil {
			return err
		}
		for _, row := range rows {
			owner, _ := jsonInt64(row["task_id"])
			if owner != taskID {
				return fmt.Errorf("archived %s task mismatch", table)
			}
			if table == "task_session_bindings" {
				sid, ok := jsonInt64(row["session_id"])
				if !ok || !archivedSessions[sid] {
					return errors.New("archived session binding has no matching resource snapshot")
				}
			}
		}
		if err := insertArchiveRows(tx, table, tables[table]); err != nil {
			return err
		}
	}
	// Verification runs remain immutable history; no restored conversation may
	// resume an old Agent or claim a fresh result against the current evidence.
	rows, err := decodeArchiveRows(tables["finding_checks"])
	if err != nil {
		return err
	}
	for _, row := range rows {
		if err := validateArchivedFindingOwner(tx, taskID, row); err != nil {
			return err
		}
		row["conversation_id"] = nil
		if row["status"] == "pending" || row["status"] == "running" {
			row["status"] = "stopped"
			row["error"] = "从归档恢复，验证未继续执行"
		}
	}
	raw, err := json.Marshal(rows)
	if err != nil {
		return err
	}
	if err = insertArchiveRows(tx, "finding_checks", raw); err != nil {
		return err
	}
	for _, table := range []string{"finding_verification_events", "finding_inline_merge_events"} {
		rows, err := decodeArchiveRows(tables[table])
		if err != nil {
			return err
		}
		for _, row := range rows {
			if err := validateArchivedFindingOwner(tx, taskID, row); err != nil {
				return err
			}
		}
		if err = insertArchiveRows(tx, table, tables[table]); err != nil {
			return err
		}
	}
	return nil
}

func validateArchivedFindingOwner(tx *sql.Tx, taskID int64, row map[string]any) error {
	fid, ok := jsonInt64(row["finding_id"])
	if !ok || fid <= 0 {
		return errors.New("invalid archived finding audit reference")
	}
	var owns bool
	if err := tx.QueryRow(`SELECT EXISTS(SELECT 1 FROM findings WHERE id=$1 AND task_id=$2)`, fid, taskID).Scan(&owns); err != nil {
		return err
	}
	if !owns {
		return errors.New("archived verification/audit belongs to another task")
	}
	return nil
}
