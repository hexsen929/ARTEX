package db

import (
	"errors"
	"fmt"
	"testing"

	"github.com/Autumn-27/artex/sidequestion"
	"github.com/jackc/pgx/v5/pgconn"
)

func TestSideSnapshotInvalidParentRejectedBeforeDatabaseAccess(t *testing.T) {
	// A nil connection is intentional: invalid identity must be classified
	// before beginning a transaction, not retried as a storage error.
	d := &DB{}
	for _, parent := range []sidequestion.Parent{
		{}, {ConversationID: 7, TaskID: 11, ExplorationID: 13},
		{TaskID: 11}, {TaskID: 11, ExplorationID: 13, IntentID: -1},
	} {
		snapshot := sidequestion.Snapshot{Parent: parent, RunID: 1, Version: 1}
		if err := d.SaveSideSnapshot(t.Context(), snapshot); !errors.Is(err, sidequestion.ErrInvalidParent) {
			t.Fatalf("save invalid parent classification: %+v %v", parent, err)
		}
		if _, _, err := d.StartSideRequest(t.Context(), snapshot, "fixture", "fixture"); !errors.Is(err, sidequestion.ErrInvalidParent) {
			t.Fatalf("start invalid parent classification: %+v %v", parent, err)
		}
	}
}

func TestSideParentColumnsKeepExistingPostgresConstraint(t *testing.T) {
	d, conversationSnapshot := sideFixture(t)
	task, err := d.CreateTask("side parent shape fixture", "fixture", nil, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.DeleteTask(task.ID) })
	intentID, err := d.Exploration(task.ExplorationID).AddNode("intent", map[string]any{"summary": "fixture"}, 0, "running", "planner", nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, parent := range []sidequestion.Parent{
		{ConversationID: conversationSnapshot.Parent.ConversationID},
		{TaskID: task.ID, ExplorationID: task.ExplorationID},
		{TaskID: task.ID, ExplorationID: task.ExplorationID, IntentID: intentID},
	} {
		snapshot := conversationSnapshot
		snapshot.Parent = parent
		if err := d.SaveSideSnapshot(t.Context(), snapshot); err != nil {
			t.Fatalf("valid parent rejected: %+v: %v", parent, err)
		}
		stored, err := d.SideSnapshot(t.Context(), parent.Key())
		if err != nil || stored == nil || stored.Parent != parent {
			t.Fatalf("valid parent did not roundtrip: %+v: %+v %v", parent, stored, err)
		}
	}
	// The fix is not a weakened database constraint: a direct mixed-parent
	// insert is still rejected by PostgreSQL, with valid referenced resources.
	_, err = d.Exec(`INSERT INTO side_question_sessions(session_key,conversation_id,task_id,exploration_id,run_id,version,snapshot)
VALUES($1,$2,$3,$4,1,1,'{}'::jsonb)`, fmt.Sprintf("invalid-parent-%d", task.ID), conversationSnapshot.Parent.ConversationID, task.ID, task.ExplorationID)
	var constraint *pgconn.PgError
	if !errors.As(err, &constraint) || constraint.Code != "23514" || constraint.ConstraintName != "side_question_sessions_check" {
		t.Fatalf("exclusive-parent SQL constraint changed: %v", err)
	}
}
