package db

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// A retest conversation is movable only when the complete binding is owned by
// this task. A shared/mis-bound conversation remains hot, with its history intact.
func archiveSideOwnerPredicate(alias string) string {
	return `(` + alias + `.task_id IS NULL OR ` + alias + `.task_id=$1)
 AND (` + alias + `.exploration_id IS NULL OR ` + alias + `.exploration_id=(SELECT exploration_id FROM tasks WHERE id=$1))
 AND (` + alias + `.intent_id IS NULL OR EXISTS(SELECT 1 FROM exploration_nodes n
      JOIN tasks t ON t.exploration_id=n.exploration_id WHERE n.id=` + alias + `.intent_id AND t.id=$1))
 AND NOT EXISTS(SELECT 1 FROM finding_retests r JOIN findings f ON f.id=r.finding_id
                WHERE r.conversation_id=` + alias + `.conversation_id AND f.task_id IS DISTINCT FROM $1)
 AND NOT EXISTS(SELECT 1 FROM finding_checks r JOIN findings f ON f.id=r.finding_id
                WHERE r.conversation_id=` + alias + `.conversation_id AND f.task_id IS DISTINCT FROM $1)
 AND NOT EXISTS(SELECT 1 FROM findings f WHERE f.report_conversation_id=` + alias + `.conversation_id
                AND f.task_id IS DISTINCT FROM $1)`
}

func archiveRetestConversationIDsQuery() string {
	return `SELECT c.id FROM conversations c WHERE c.agent_key='retester'
 AND EXISTS(SELECT 1 FROM finding_retests r JOIN findings f ON f.id=r.finding_id
            WHERE r.conversation_id=c.id AND f.task_id=$1)
 AND NOT EXISTS(SELECT 1 FROM finding_retests r JOIN findings f ON f.id=r.finding_id
                WHERE r.conversation_id=c.id AND f.task_id IS DISTINCT FROM $1)
 AND NOT EXISTS(SELECT 1 FROM finding_checks r WHERE r.conversation_id=c.id)
 AND NOT EXISTS(SELECT 1 FROM findings f WHERE f.report_conversation_id=c.id)
 AND NOT EXISTS(SELECT 1 FROM finding_case_review_runs r WHERE r.conversation_id=c.id)
 AND NOT EXISTS(SELECT 1 FROM intercept_pending p WHERE p.conversation_id=c.id
                AND COALESCE(p.task_id,'') NOT IN ('',($1::bigint)::text))
 AND NOT EXISTS(SELECT 1 FROM side_question_sessions s LEFT JOIN intercept_pending p ON p.id=s.approval_id
                WHERE (s.conversation_id=c.id OR p.conversation_id=c.id)
                  AND NOT (` + archiveSideOwnerPredicate("s") + `))
 AND NOT EXISTS(SELECT 1 FROM side_question_sessions s JOIN intercept_pending p ON p.id=s.approval_id
                WHERE s.conversation_id=c.id AND
                 ((COALESCE(p.task_id,'')<>($1::bigint)::text AND p.conversation_id IS DISTINCT FROM c.id)
                  OR COALESCE(p.task_id,'') NOT IN ('',($1::bigint)::text)
                  OR EXISTS(SELECT 1 FROM side_question_sessions sibling WHERE sibling.approval_id=p.id
                            AND NOT (` + archiveSideOwnerPredicate("sibling") + `))))`
}

func archiveTaskApprovalIDsQuery() string {
	return `SELECT p.id FROM intercept_pending p WHERE
 (COALESCE(p.task_id,'')=($1::bigint)::text
 OR (p.conversation_id IN (` + archiveRetestConversationIDsQuery() + `)
     AND COALESCE(p.task_id,'') IN ('',($1::bigint)::text)))
 AND NOT EXISTS(SELECT 1 FROM side_question_sessions s WHERE s.approval_id=p.id
                AND NOT (` + archiveSideOwnerPredicate("s") + `))`
}

func archiveTaskSideSessionPredicate(alias string) string {
	return `(` + archiveSideOwnerPredicate(alias) + `) AND
 (` + alias + `.approval_id IS NULL OR ` + alias + `.approval_id IN (` + archiveTaskApprovalIDsQuery() + `)) AND
 (` + alias + `.task_id=$1 OR ` + alias + `.conversation_id IN (` + archiveRetestConversationIDsQuery() + `)
 OR ` + alias + `.approval_id IN (` + archiveTaskApprovalIDsQuery() + `))`
}

func requireRetestArchiveTables(snapshot *TaskArchiveSnapshot) error {
	for _, table := range []string{"finding_retests", "conversations", "conversation_activities", "intercept_pending", "side_question_sessions", "side_question_requests"} {
		raw, present := snapshot.Tables[table]
		if snapshot.FormatVersion < 6 && !present {
			continue
		}
		trimmed := bytes.TrimSpace(raw)
		rows, err := decodeArchiveRows(raw)
		count, counted := snapshot.DataCounts[table]
		if !present || len(trimmed) == 0 || trimmed[0] != '[' || !json.Valid(raw) || err != nil ||
			(snapshot.FormatVersion >= 6 && !counted) || (counted && count != int64(len(rows))) {
			return fmt.Errorf("%w: missing or inconsistent %s history", ErrTaskArchiveFormatMismatch, table)
		}
	}
	return nil
}

func archiveRowID(row map[string]any, field string) (int64, error) {
	id, ok := jsonInt64(row[field])
	if !ok || id <= 0 {
		return 0, fmt.Errorf("invalid archived %s", field)
	}
	return id, nil
}

func validateArchivedRetestFinding(tx *sql.Tx, taskID, expID int64, row map[string]any) error {
	if err := validateArchivedFindingOwner(tx, taskID, row); err != nil {
		return err
	}
	fid, _ := jsonInt64(row["finding_id"])
	var nodeID, ownerExp sql.NullInt64
	if err := tx.QueryRow(`SELECT f.node_id,n.exploration_id FROM findings f
 LEFT JOIN exploration_nodes n ON n.id=f.node_id WHERE f.id=$1 AND f.task_id=$2`, fid, taskID).Scan(&nodeID, &ownerExp); err != nil {
		return err
	}
	if nodeID.Valid && (!ownerExp.Valid || ownerExp.Int64 != expID) {
		return errors.New("archived retest finding node belongs to another task")
	}
	snapshot, ok := row["snapshot"].(map[string]any)
	if !ok {
		return errors.New("invalid archived retest snapshot")
	}
	finding, ok := snapshot["finding"].(map[string]any)
	if !ok {
		return errors.New("archived retest snapshot has no finding")
	}
	snapshotID, _ := jsonInt64(finding["id"])
	snapshotTask, _ := jsonInt64(finding["task_id"])
	snapshotNode, _ := jsonInt64(finding["node_id"])
	if snapshotID != fid || snapshotTask != taskID || snapshotNode != nodeID.Int64 {
		return errors.New("archived retest snapshot ownership mismatch")
	}
	return nil
}

func lockArchiveRows(tx *sql.Tx, query string, args ...any) error {
	rows, err := tx.Query(query, args...)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var ignored any
		if err := rows.Scan(&ignored); err != nil {
			return err
		}
	}
	return rows.Err()
}

// Checks the exact rows before destructive compaction. A late append/result
// causes a fresh snapshot, never deletion of history absent from the package.
func validateFrozenArchiveRows(tx *sql.Tx, table string, raw json.RawMessage, predicate string, args ...any) error {
	switch table {
	case "finding_retests", "conversations", "conversation_activities", "intercept_pending", "side_question_sessions", "side_question_requests":
	default:
		return errors.New("invalid frozen archive table")
	}
	rows, err := decodeArchiveRows(raw)
	if err != nil {
		return err
	}
	var count int64
	if err := tx.QueryRow(`SELECT count(*) FROM `+table+` current WHERE `+predicate, args...).Scan(&count); err != nil {
		return err
	}
	if count != int64(len(rows)) {
		return errors.New("retest history changed after archive snapshot")
	}
	seen := map[string]bool{}
	for _, row := range rows {
		var key string
		if table == "side_question_sessions" || table == "side_question_requests" {
			field := "id"
			if table == "side_question_sessions" {
				field = "session_key"
			}
			key, _ = row[field].(string)
		} else {
			id, err := archiveRowID(row, "id")
			if err != nil {
				return err
			}
			key = fmt.Sprint(id)
		}
		if key == "" || seen[key] {
			return errors.New("invalid or duplicate archived history id")
		}
		seen[key] = true
		encoded, err := json.Marshal(row)
		if err != nil {
			return err
		}
		var same bool
		if len(args) != 1 {
			return errors.New("invalid frozen archive scope")
		}
		if err := tx.QueryRow(`SELECT EXISTS(SELECT 1 FROM `+table+` current WHERE (`+predicate+`)
 AND current IS NOT DISTINCT FROM json_populate_record(NULL::`+table+`,$2::json))`, args[0], string(encoded)).Scan(&same); err != nil {
			return err
		}
		if !same {
			return errors.New("retest history changed after archive snapshot")
		}
	}
	return nil
}

type retestArchiveScope struct {
	ConversationIDs []int64
	ApprovalIDs     []int64
	SideSessionKeys []string
}

func validateRetestArchiveCompaction(tx *sql.Tx, taskID, expID int64, snapshot *TaskArchiveSnapshot) (*retestArchiveScope, error) {
	if err := requireRetestArchiveTables(snapshot); err != nil {
		return nil, err
	}
	// Finding -> conversation -> retest/activity matches retest creation/finish
	// and conversation-deletion triggers without acquiring an unrelated task.
	if err := lockArchiveRows(tx, `SELECT id FROM findings WHERE task_id=$1 ORDER BY id FOR UPDATE`, taskID); err != nil {
		return nil, err
	}
	conversations, err := decodeArchiveRows(snapshot.Tables["conversations"])
	if err != nil {
		return nil, err
	}
	ids := []int64{}
	for _, row := range conversations {
		id, err := archiveRowID(row, "id")
		if err != nil || row["agent_key"] != FindingRetestAgentKey {
			return nil, errors.New("invalid archived retest conversation")
		}
		ids = append(ids, id)
	}
	if err := lockArchiveRows(tx, `SELECT c.id FROM conversations c WHERE c.id IN (`+archiveRetestConversationIDsQuery()+`) ORDER BY c.id FOR UPDATE`, taskID); err != nil {
		return nil, err
	}
	if err := validateFrozenArchiveRows(tx, "conversations", snapshot.Tables["conversations"], `current.id IN (`+archiveRetestConversationIDsQuery()+`)`, taskID); err != nil {
		return nil, err
	}
	if err := lockArchiveRows(tx, `SELECT r.id FROM finding_retests r JOIN findings f ON f.id=r.finding_id WHERE f.task_id=$1 ORDER BY r.id FOR UPDATE OF r`, taskID); err != nil {
		return nil, err
	}
	if err := lockArchiveRows(tx, `SELECT id FROM conversation_activities WHERE conversation_id=ANY($1::bigint[]) ORDER BY id FOR UPDATE`, ids); err != nil {
		return nil, err
	}
	if err := validateFrozenArchiveRows(tx, "finding_retests", snapshot.Tables["finding_retests"], `EXISTS(SELECT 1 FROM findings f WHERE f.id=current.finding_id AND f.task_id=$1)`, taskID); err != nil {
		return nil, err
	}
	if err := validateFrozenArchiveRows(tx, "conversation_activities", snapshot.Tables["conversation_activities"], `current.conversation_id=ANY($1::bigint[])`, ids); err != nil {
		return nil, err
	}
	if err := lockArchiveRows(tx, `SELECT p.id FROM intercept_pending p WHERE p.id IN (`+archiveTaskApprovalIDsQuery()+`) ORDER BY p.id FOR UPDATE`, taskID); err != nil {
		return nil, err
	}
	if err := lockArchiveRows(tx, `SELECT s.session_key FROM side_question_sessions s WHERE `+archiveTaskSideSessionPredicate("s")+` ORDER BY s.session_key FOR UPDATE`, taskID); err != nil {
		return nil, err
	}
	if err := lockArchiveRows(tx, `SELECT r.id FROM side_question_requests r JOIN side_question_sessions s ON s.session_key=r.session_key WHERE `+archiveTaskSideSessionPredicate("s")+` ORDER BY r.ordinal FOR UPDATE OF r`, taskID); err != nil {
		return nil, err
	}
	if err := validateFrozenArchiveRows(tx, "intercept_pending", snapshot.Tables["intercept_pending"], `current.id IN (`+archiveTaskApprovalIDsQuery()+`)`, taskID); err != nil {
		return nil, err
	}
	if err := validateFrozenArchiveRows(tx, "side_question_sessions", snapshot.Tables["side_question_sessions"], archiveTaskSideSessionPredicate("current"), taskID); err != nil {
		return nil, err
	}
	if err := validateFrozenArchiveRows(tx, "side_question_requests", snapshot.Tables["side_question_requests"], `EXISTS(SELECT 1 FROM side_question_sessions s WHERE s.session_key=current.session_key AND (`+archiveTaskSideSessionPredicate("s")+`))`, taskID); err != nil {
		return nil, err
	}
	rows, err := decodeArchiveRows(snapshot.Tables["finding_retests"])
	if err != nil {
		return nil, err
	}
	for _, row := range rows {
		if err := validateArchivedRetestFinding(tx, taskID, expID, row); err != nil {
			return nil, err
		}
	}
	result := &retestArchiveScope{ConversationIDs: ids}
	approvalRows, err := decodeArchiveRows(snapshot.Tables["intercept_pending"])
	if err != nil {
		return nil, err
	}
	for _, row := range approvalRows {
		id, err := archiveRowID(row, "id")
		if err != nil {
			return nil, err
		}
		result.ApprovalIDs = append(result.ApprovalIDs, id)
	}
	sideRows, err := decodeArchiveRows(snapshot.Tables["side_question_sessions"])
	if err != nil {
		return nil, err
	}
	for _, row := range sideRows {
		key, ok := row["session_key"].(string)
		if !ok || key == "" {
			return nil, errors.New("invalid archived side session key")
		}
		result.SideSessionKeys = append(result.SideSessionKeys, key)
	}
	return result, nil
}

func restoreRetestArchiveRows(tx *sql.Tx, taskID, expID int64, tables map[string]json.RawMessage, strict bool) ([]string, error) {
	rows, err := decodeArchiveRows(tables["finding_retests"])
	if err != nil {
		return nil, err
	}
	warnings := []string{}
	references := map[int64]bool{}
	for _, row := range rows {
		if err := validateArchivedRetestFinding(tx, taskID, expID, row); err != nil {
			return nil, err
		}
		if cid, ok := jsonInt64(row["conversation_id"]); ok && cid > 0 {
			references[cid] = true
		}
	}
	conversations, err := decodeArchiveRows(tables["conversations"])
	if err != nil {
		return nil, err
	}
	owned := map[int64]bool{}
	for _, row := range conversations {
		cid, err := archiveRowID(row, "id")
		if err != nil || owned[cid] || !references[cid] || row["agent_key"] != FindingRetestAgentKey {
			return nil, errors.New("archived conversation has no exclusive retest owner")
		}
		owned[cid] = true
		if profile, ok := jsonInt64(row["llm_profile_id"]); ok && profile > 0 && !rowExists(tx, "llm_profiles", profile) {
			row["llm_profile_id"] = nil
			warnings = append(warnings, "复测历史原模型配置已删除，未重建该配置")
		}
	}
	if strict {
		if err := validateArchivedRetestDependencies(tx, taskID, expID, tables); err != nil {
			return nil, err
		}
	}
	// Never reattach an approval or side conversation to a shared/global chat
	// that was deliberately not moved with this archive (including old formats).
	for _, table := range []string{"intercept_pending", "side_question_sessions"} {
		dependencies, err := decodeArchiveRows(tables[table])
		if err != nil {
			return nil, err
		}
		changed := false
		for _, row := range dependencies {
			if cid, ok := jsonInt64(row["conversation_id"]); ok && cid > 0 && !owned[cid] {
				row["conversation_id"] = nil
				changed = true
			}
		}
		if changed {
			tables[table], err = json.Marshal(dependencies)
			if err != nil {
				return nil, err
			}
			warnings = append(warnings, "未独占归档的会话引用已移除，未重新建立跨任务关联")
		}
	}
	raw, err := json.Marshal(conversations)
	if err != nil {
		return nil, err
	}
	if err := insertArchiveRows(tx, "conversations", raw); err != nil {
		return nil, err
	}
	activities, err := decodeArchiveRows(tables["conversation_activities"])
	if err != nil {
		return nil, err
	}
	for _, row := range activities {
		cid, _ := jsonInt64(row["conversation_id"])
		if !owned[cid] {
			return nil, errors.New("archived activity belongs to another conversation")
		}
	}
	if err := insertArchiveRows(tx, "conversation_activities", tables["conversation_activities"]); err != nil {
		return nil, err
	}
	for _, row := range rows {
		cid, _ := jsonInt64(row["conversation_id"])
		if cid > 0 && !owned[cid] {
			row["conversation_id"] = nil
			warnings = append(warnings, "复测的共享或未归档会话仍独立保留，未恢复不可信会话关联")
		}
		if row["status"] == "pending" || row["status"] == "running" {
			row["status"] = "stopped"
			row["error"] = "从归档恢复，复测未继续执行"
			row["finished_at"] = time.Now().UTC()
		}
	}
	raw, err = json.Marshal(rows)
	if err != nil {
		return nil, err
	}
	if err := insertArchiveRows(tx, "finding_retests", raw); err != nil {
		return nil, err
	}
	return warnings, nil
}

func stopArchivedSideRequests(raw json.RawMessage) (json.RawMessage, error) {
	rows, err := decodeArchiveRows(raw)
	if err != nil {
		return nil, err
	}
	for _, row := range rows {
		if row["status"] == "running" {
			row["status"] = "interrupted"
			row["error"] = "从归档恢复，原旁路提问未继续执行"
		}
	}
	return json.Marshal(rows)
}

// New packages keep dependency ownership explicit instead of importing another
// task's side history merely because its approval points at this conversation.
func validateArchivedRetestDependencies(tx *sql.Tx, taskID, expID int64, tables map[string]json.RawMessage) error {
	approvals, err := decodeArchiveRows(tables["intercept_pending"])
	if err != nil {
		return err
	}
	approvalIDs := map[int64]bool{}
	for _, row := range approvals {
		id, err := archiveRowID(row, "id")
		if err != nil || approvalIDs[id] {
			return errors.New("invalid archived approval id")
		}
		approvalIDs[id] = true
		if row["task_id"] != nil && row["task_id"] != "" && row["task_id"] != fmt.Sprint(taskID) {
			return errors.New("archived approval belongs to another task")
		}
	}
	sessions, err := decodeArchiveRows(tables["side_question_sessions"])
	if err != nil {
		return err
	}
	keys := map[string]bool{}
	for _, row := range sessions {
		key, ok := row["session_key"].(string)
		if !ok || key == "" || keys[key] {
			return errors.New("invalid archived side session")
		}
		keys[key] = true
		for _, field := range []string{"task_id", "exploration_id"} {
			if row[field] == nil {
				continue
			}
			id, err := archiveRowID(row, field)
			if err != nil {
				return err
			}
			want := taskID
			if field == "exploration_id" {
				want = expID
			}
			if id != want {
				return errors.New("archived side history belongs to another task")
			}
		}
		if row["intent_id"] != nil {
			id, err := archiveRowID(row, "intent_id")
			if err != nil {
				return err
			}
			var own bool
			if err := tx.QueryRow(`SELECT EXISTS(SELECT 1 FROM exploration_nodes WHERE id=$1 AND exploration_id=$2)`, id, expID).Scan(&own); err != nil {
				return err
			}
			if !own {
				return errors.New("archived side intent belongs to another task")
			}
		}
		if row["approval_id"] != nil {
			id, err := archiveRowID(row, "approval_id")
			if err != nil || !approvalIDs[id] {
				return errors.New("archived side approval has no owned parent snapshot")
			}
		}
	}
	requests, err := decodeArchiveRows(tables["side_question_requests"])
	if err != nil {
		return err
	}
	for _, row := range requests {
		key, _ := row["session_key"].(string)
		if !keys[key] {
			return errors.New("archived side request belongs to another session")
		}
	}
	return nil
}
