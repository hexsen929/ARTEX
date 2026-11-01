package db

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
)

// Only this host-written activity kind is an authority. Titles, human messages,
// model output and ordinary tool records are never parsed to infer ownership.
const findingReportBindingKind = "report_binding"
const findingReportBindingTool = "update_finding_report"

type FindingReportBinding struct {
	FindingID      int64 `json:"finding_id"`
	TaskID         int64 `json:"task_id"`
	ExplorationID  int64 `json:"-"`
	NodeID         int64 `json:"-"`
	ConversationID int64 `json:"conversation_id"`
}

type reportBindingQuerier interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}

func reportBindings(ctx context.Context, q reportBindingQuerier, conversationID int64) ([]FindingReportBinding, error) {
	rows, err := q.QueryContext(ctx, `SELECT detail FROM conversation_activities WHERE conversation_id=$1 AND kind=$2 AND tool=$3 ORDER BY id LIMIT 102`, conversationID, findingReportBindingKind, findingReportBindingTool)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []FindingReportBinding
	seen := map[int64]bool{}
	for rows.Next() {
		var detail string
		if err := rows.Scan(&detail); err != nil {
			return nil, err
		}
		var b FindingReportBinding
		if err := json.Unmarshal([]byte(detail), &b); err != nil || b.FindingID <= 0 || b.TaskID <= 0 || b.ConversationID != conversationID {
			return nil, errors.New("报告会话关联元数据无效，请从漏洞详情重新打开")
		}
		if !seen[b.FindingID] {
			out = append(out, b)
			seen[b.FindingID] = true
		}
		if len(out) > 100 {
			return nil, errors.New("报告会话关联不明确，请从漏洞详情重新打开")
		}
	}
	return out, rows.Err()
}

func appendReportBindingTx(ctx context.Context, tx *sql.Tx, b FindingReportBinding) error {
	raw, err := json.Marshal(b)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO conversation_activities(conversation_id,worker,kind,tool,summary,detail)
 SELECT $1,'reporter',$2,$3,'报告来源关联',$4 WHERE NOT EXISTS(
 SELECT 1 FROM conversation_activities WHERE conversation_id=$1 AND kind=$2 AND tool=$3 AND detail=$4)`, b.ConversationID, findingReportBindingKind, findingReportBindingTool, string(raw))
	return err
}

// recordFindingReportConversationTx is invoked only after evidence-version
// admission, inside the report transaction. Non-reporter platform conversations
// retain their existing ability to write a report without becoming reporter chats.
func recordFindingReportConversationTx(ctx context.Context, tx *sql.Tx, findingID, conversationID int64) error {
	var role string
	err := tx.QueryRowContext(ctx, `SELECT agent_key FROM conversations WHERE id=$1 FOR UPDATE`, conversationID).Scan(&role)
	if errors.Is(err, sql.ErrNoRows) {
		return errors.New("报告会话已删除")
	}
	if err != nil {
		return err
	}
	if role != "reporter" {
		return nil
	}
	b := FindingReportBinding{FindingID: findingID, ConversationID: conversationID}
	err = tx.QueryRowContext(ctx, `SELECT task_id FROM findings WHERE id=$1`, findingID).Scan(&b.TaskID)
	if err != nil {
		return err
	}
	if b.TaskID <= 0 {
		return errors.New("报告未关联真实来源任务")
	}
	if err := appendReportBindingTx(ctx, tx, b); err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `UPDATE findings SET report_conversation_id=$2 WHERE id=$1`, findingID, conversationID)
	return err
}

// SetFindingReportConversationID is kept for explicit host callers. It never
// attempts a title/substring-based historical backfill.
func (d *DB) SetFindingReportConversationID(findingID, conversationID int64) error {
	return d.WithEvidenceTx(context.Background(), func(tx *sql.Tx) error {
		if err := LockFindingEvidenceTx(tx, findingID, nil); err != nil {
			return err
		}
		return recordFindingReportConversationTx(context.Background(), tx, findingID, conversationID)
	})
}

// FindingReportBindingForConversation returns nil only for an ordinary unbound
// conversation. Once a host association exists, deletion, ambiguity or changed
// ownership is an error and must not downgrade the run to an unscoped chat.
func (d *DB) FindingReportBindingForConversation(ctx context.Context, conversationID int64) (*FindingReportBinding, error) {
	bindings, err := reportBindings(ctx, d, conversationID)
	if err != nil {
		return nil, err
	}
	if len(bindings) == 0 {
		return nil, nil
	}
	if len(bindings) != 1 {
		return nil, errors.New("历史报告会话包含多个漏洞，请从当前漏洞详情打开专属报告对话")
	}
	b := bindings[0]
	var taskID, expID int64
	err = d.QueryRowContext(ctx, `SELECT f.task_id,t.exploration_id,COALESCE(f.node_id,0) FROM findings f
 JOIN tasks t ON t.id=f.task_id JOIN conversations c ON c.id=f.report_conversation_id
 WHERE f.id=$1 AND f.task_id=$2 AND f.report_conversation_id=$3 AND c.agent_key='reporter'
 AND t.deleted_at IS NULL AND t.archived_at IS NULL
 AND NOT EXISTS(SELECT 1 FROM task_archives a WHERE a.task_id=t.id AND a.state IN ('archive_queued','archiving','restore_queued','restoring','delete_queued','deleting'))`, b.FindingID, b.TaskID, conversationID).Scan(&taskID, &expID, &b.NodeID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, errors.New("报告来源已删除、归档或关联已变更，请从漏洞详情重新打开")
	}
	if err != nil {
		return nil, err
	}
	b.TaskID, b.ExplorationID = taskID, expID
	return &b, nil
}

// EnsureFindingReportConversation atomically resolves or creates one dedicated
// reporter conversation. Opening the panel does not enqueue an Agent/model run.
func (d *DB) EnsureFindingReportConversation(ctx context.Context, findingID int64) (*Conversation, *FindingReportBinding, error) {
	tx, err := d.BeginTx(ctx, nil)
	if err != nil {
		return nil, nil, err
	}
	defer tx.Rollback()
	b := &FindingReportBinding{FindingID: findingID}
	var old sql.NullInt64
	err = tx.QueryRowContext(ctx, `SELECT f.task_id,t.exploration_id,f.report_conversation_id,COALESCE(f.node_id,0) FROM findings f
 JOIN tasks t ON t.id=f.task_id WHERE f.id=$1 AND t.deleted_at IS NULL AND t.archived_at IS NULL
 AND NOT EXISTS(SELECT 1 FROM task_archives a WHERE a.task_id=t.id AND a.state IN ('archive_queued','archiving','restore_queued','restoring','delete_queued','deleting'))
 FOR UPDATE OF f`, findingID).Scan(&b.TaskID, &b.ExplorationID, &old, &b.NodeID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil, ErrFindingNotFound
	}
	if err != nil {
		return nil, nil, err
	}
	if old.Valid {
		c, ce := scanConv(tx.QueryRowContext(ctx, `SELECT `+convCols+` FROM conversations WHERE id=$1 AND agent_key='reporter' FOR UPDATE`, old.Int64))
		if ce != nil && !errors.Is(ce, sql.ErrNoRows) {
			return nil, nil, ce
		}
		if ce == nil {
			bindings, be := reportBindings(ctx, tx, c.ID)
			if be != nil {
				return nil, nil, be
			}
			var other int
			if err = tx.QueryRowContext(ctx, `SELECT count(*) FROM findings WHERE report_conversation_id=$1 AND id<>$2`, c.ID, findingID).Scan(&other); err != nil {
				return nil, nil, err
			}
			if len(bindings) == 1 && bindings[0].FindingID == findingID && bindings[0].TaskID == b.TaskID && other == 0 {
				b.ConversationID = c.ID
				if err = tx.Commit(); err != nil {
					return nil, nil, err
				}
				return &c, b, nil
			}
		}
	}
	c, err := scanConv(tx.QueryRowContext(ctx, `INSERT INTO conversations(agent_key,title) VALUES('reporter',$1) RETURNING `+convCols, fmt.Sprintf("漏洞报告 · finding#%d", findingID)))
	if err != nil {
		return nil, nil, err
	}
	b.ConversationID = c.ID
	if err = appendReportBindingTx(ctx, tx, *b); err != nil {
		return nil, nil, err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE findings SET report_conversation_id=$2 WHERE id=$1`, findingID, c.ID); err != nil {
		return nil, nil, err
	}
	if err = tx.Commit(); err != nil {
		return nil, nil, err
	}
	return &c, b, nil
}

// ResolveFindingReportConversation is read-only and uses exact host relations.
// Historical names or tool text are deliberately not a source of ownership.
func (d *DB) ResolveFindingReportConversation(findingID int64) (*int64, error) {
	var id sql.NullInt64
	if err := d.QueryRow(`SELECT report_conversation_id FROM findings WHERE id=$1`, findingID).Scan(&id); err != nil {
		return nil, err
	}
	if !id.Valid {
		return nil, nil
	}
	b, err := d.FindingReportBindingForConversation(context.Background(), id.Int64)
	if err != nil {
		return nil, err
	}
	if b == nil || b.FindingID != findingID {
		return nil, nil
	}
	return &id.Int64, nil
}
