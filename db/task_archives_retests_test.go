package db

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

type retestArchiveFixture struct {
	d             *DB
	task          *Task
	findingID     int64
	conversation  *Conversation
	retest        *FindingRetest
	conversations []int64
	tasks         []int64
}

func newRetestArchiveFixture(t *testing.T) *retestArchiveFixture {
	t.Helper()
	d := portTestDB(t)
	task, err := d.CreateTask("retest archive fixture", "preserve local synthetic history", nil, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	f := &retestArchiveFixture{d: d, task: task, tasks: []int64{task.ID}}
	t.Cleanup(func() {
		for _, id := range f.conversations {
			_ = d.DeleteConversation(id)
		}
		for _, id := range f.tasks {
			_, _ = d.Exec(`DELETE FROM task_archives WHERE task_id=$1`, id)
			_ = d.DeleteTask(id)
		}
	})
	if err := d.SetPaused(task.ID, true); err != nil {
		t.Fatal(err)
	}
	finding, err := d.Exploration(task.ExplorationID).RecordFinding(t.Context(), RecordFindingInput{
		TaskID: task.ID, Summary: "synthetic archive finding", Severity: "low", Evidence: "synthetic retained observation",
	})
	if err != nil {
		t.Fatal(err)
	}
	f.findingID = finding.FindingID
	r, c, created, err := d.CreateFindingRetest(t.Context(), f.findingID, "synthetic retest notes")
	if err != nil || !created || c == nil || r == nil {
		t.Fatalf("create synthetic retest: %v", err)
	}
	f.retest, f.conversation = r, c
	f.conversations = append(f.conversations, c.ID)
	return f
}

func (f *retestArchiveFixture) snapshot(t *testing.T) (*TaskArchive, *TaskArchiveSnapshot) {
	t.Helper()
	a, err := f.d.QueueTaskArchive(f.task.ID)
	if err != nil {
		t.Fatal(err)
	}
	claimed, err := f.d.ClaimTaskArchiveJob(t.Context())
	if err != nil || claimed == nil || claimed.ID != a.ID || claimed.State != Archiving {
		t.Fatalf("claim archive fixture: %+v %v", claimed, err)
	}
	s, err := f.d.SnapshotTaskArchive(f.task.ID)
	if err != nil {
		t.Fatal(err)
	}
	return a, s
}

func (f *retestArchiveFixture) restore(t *testing.T, a *TaskArchive, s *TaskArchiveSnapshot) []string {
	t.Helper()
	if _, err := f.d.QueueTaskArchiveRestore(a.ID); err != nil {
		t.Fatal(err)
	}
	claimed, err := f.d.ClaimTaskArchiveJob(t.Context())
	if err != nil || claimed == nil || claimed.ID != a.ID || claimed.State != Restoring {
		t.Fatalf("claim restore fixture: %+v %v", claimed, err)
	}
	warnings, err := f.d.RestoreTaskArchive(a.ID, s, 0)
	if err != nil {
		t.Fatal(err)
	}
	return warnings
}

func requireArchiveRowCount(t *testing.T, d *DB, query string, want int64, args ...any) {
	t.Helper()
	var got int64
	if err := d.QueryRow(query, args...).Scan(&got); err != nil || got != want {
		t.Fatalf("synthetic archive row count=%d want=%d err=%v", got, want, err)
	}
}

func addRetestSideArchiveFixture(t *testing.T, f *retestArchiveFixture) (int64, string, string) {
	t.Helper()
	var approval int64
	if err := f.d.QueryRow(`INSERT INTO intercept_pending(conversation_id,tool_name,status)
 VALUES($1,'synthetic_read','pending') RETURNING id`, f.conversation.ID).Scan(&approval); err != nil {
		t.Fatal(err)
	}
	key, request := fmt.Sprintf("archive-side-check-%d", f.task.ID), fmt.Sprintf("archive-side-check-request-%d", f.task.ID)
	if _, err := f.d.Exec(`INSERT INTO side_question_sessions(session_key,conversation_id,approval_id,run_id,version,snapshot)
 VALUES($1,$2,$3,1,1,'{}')`, key, f.conversation.ID, approval); err != nil {
		t.Fatal(err)
	}
	if _, err := f.d.Exec(`INSERT INTO side_question_requests(id,session_key,generation,client_id,question,answer,status,model,snapshot_at)
 VALUES($1,$2,0,'fixture','synthetic question','synthetic partial answer','running','{}',now())`, request, key); err != nil {
		t.Fatal(err)
	}
	return approval, key, request
}

func TestTaskArchiveRetestConversationAndApprovalRoundTrip(t *testing.T) {
	f := newRetestArchiveFixture(t)
	if started, err := f.d.StartFindingRetest(t.Context(), f.retest.ID); err != nil || !started {
		t.Fatalf("start synthetic retest: %v", err)
	}
	if err := f.d.RecordFindingRetestResult(t.Context(), f.conversation.ID, "inconclusive", "synthetic conclusion", "synthetic evidence"); err != nil {
		t.Fatal(err)
	}
	if err := f.d.FinishFindingRetest(f.retest.ID, "completed", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := f.d.Exec(`INSERT INTO conversation_activities(conversation_id,worker,kind,summary,detail)
 VALUES($1,'retester','text','synthetic reply','complete retained reply')`, f.conversation.ID); err != nil {
		t.Fatal(err)
	}
	var approval int64
	if err := f.d.QueryRow(`INSERT INTO intercept_pending(conversation_id,tool_name,status)
 VALUES($1,'synthetic_read','pending') RETURNING id`, f.conversation.ID).Scan(&approval); err != nil {
		t.Fatal(err)
	}
	key, request := fmt.Sprintf("archive-retest-side-%d", f.task.ID), fmt.Sprintf("archive-retest-request-%d", f.task.ID)
	if _, err := f.d.Exec(`INSERT INTO side_question_sessions(session_key,conversation_id,approval_id,run_id,version,snapshot)
 VALUES($1,$2,$3,1,1,'{}')`, key, f.conversation.ID, approval); err != nil {
		t.Fatal(err)
	}
	if _, err := f.d.Exec(`INSERT INTO side_question_requests(id,session_key,generation,client_id,question,answer,status,model,snapshot_at)
 VALUES($1,$2,0,'fixture','synthetic question','synthetic partial answer','running','{}',now())`, request, key); err != nil {
		t.Fatal(err)
	}
	a, s := f.snapshot(t)
	for table, count := range map[string]int64{"finding_retests": 1, "conversations": 1, "conversation_activities": 2,
		"intercept_pending": 1, "side_question_sessions": 1, "side_question_requests": 1} {
		if s.DataCounts[table] != count {
			t.Fatalf("snapshot %s=%d want=%d", table, s.DataCounts[table], count)
		}
	}
	if err := f.d.CompleteTaskArchive(a.ID, s, "synthetic-package", "synthetic-digest", 100, 50); err != nil {
		t.Fatal(err)
	}
	requireArchiveRowCount(t, f.d, `SELECT count(*) FROM conversations WHERE id=$1`, 0, f.conversation.ID)
	requireArchiveRowCount(t, f.d, `SELECT count(*) FROM finding_retests WHERE id=$1`, 0, f.retest.ID)
	f.restore(t, a, s)
	r, err := f.d.FindingRetestForConversation(t.Context(), f.conversation.ID)
	if err != nil || r == nil || r.ID != f.retest.ID || r.Status != "completed" || r.Verdict != "inconclusive" || r.Summary != "synthetic conclusion" || r.Evidence != "synthetic evidence" {
		t.Fatalf("restored immutable retest: %+v %v", r, err)
	}
	requireArchiveRowCount(t, f.d, `SELECT count(*) FROM conversation_activities WHERE conversation_id=$1`, 2, f.conversation.ID)
	requireArchiveRowCount(t, f.d, `SELECT count(*) FROM intercept_pending WHERE id=$1 AND conversation_id=$2 AND status='timeout'`, 1, approval, f.conversation.ID)
	requireArchiveRowCount(t, f.d, `SELECT count(*) FROM side_question_sessions WHERE session_key=$1 AND conversation_id=$2 AND approval_id=$3`, 1, key, f.conversation.ID, approval)
	requireArchiveRowCount(t, f.d, `SELECT count(*) FROM side_question_requests WHERE id=$1 AND status='interrupted' AND answer='synthetic partial answer'`, 1, request)
}

func TestTaskArchivePendingRetestRestoresStoppedWithoutDispatch(t *testing.T) {
	f := newRetestArchiveFixture(t)
	a, s := f.snapshot(t)
	if err := f.d.CompleteTaskArchive(a.ID, s, "synthetic-package", "synthetic-digest", 100, 50); err != nil {
		t.Fatal(err)
	}
	f.restore(t, a, s)
	r, err := f.d.FindingRetestForConversation(t.Context(), f.conversation.ID)
	if err != nil || r == nil || r.Status != "stopped" || r.Verdict != "" || r.FinishedAt == nil {
		t.Fatalf("pending retest was revived or fabricated: %+v %v", r, err)
	}
	if started, err := f.d.StartFindingRetest(t.Context(), r.ID); err != nil || started {
		t.Fatalf("restored historical retest restarted: %v %v", started, err)
	}
	task, err := f.d.GetTask(f.task.ID)
	if err != nil || task == nil || !task.Paused {
		t.Fatal("archive restoration resumed its task")
	}
}

func TestTaskArchiveRetestSharedConversationRemainsHotAndDetached(t *testing.T) {
	f := newRetestArchiveFixture(t)
	other, err := f.d.CreateTask("other archive fixture", "unrelated history", nil, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	f.tasks = append(f.tasks, other.ID)
	finding, err := f.d.Exploration(other.ExplorationID).RecordFinding(t.Context(), RecordFindingInput{TaskID: other.ID, Summary: "unrelated finding", Severity: "low", Evidence: "unrelated fixture"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.d.Exec(`INSERT INTO finding_checks(finding_id,conversation_id,kind,snapshot) VALUES($1,$2,'verify','{}')`, finding.FindingID, f.conversation.ID); err != nil {
		t.Fatal(err)
	}
	a, s := f.snapshot(t)
	if s.DataCounts["conversations"] != 0 || s.DataCounts["conversation_activities"] != 0 || s.DataCounts["finding_retests"] != 1 {
		t.Fatal("shared conversation was claimed as exclusive task history")
	}
	if err := f.d.CompleteTaskArchive(a.ID, s, "synthetic-package", "synthetic-digest", 100, 50); err != nil {
		t.Fatal(err)
	}
	requireArchiveRowCount(t, f.d, `SELECT count(*) FROM conversations WHERE id=$1`, 1, f.conversation.ID)
	requireArchiveRowCount(t, f.d, `SELECT count(*) FROM finding_checks WHERE finding_id=$1 AND conversation_id=$2 AND status='pending'`, 1, finding.FindingID, f.conversation.ID)
	warnings := f.restore(t, a, s)
	history, err := f.d.ListFindingRetests(f.findingID)
	if err != nil || len(history) != 1 || history[0].ConversationID != nil || !strings.Contains(strings.Join(warnings, " "), "共享") {
		t.Fatalf("shared conversation was rebound: %+v warnings=%v err=%v", history, warnings, err)
	}
	requireArchiveRowCount(t, f.d, `SELECT count(*) FROM conversation_activities WHERE conversation_id=$1`, 1, f.conversation.ID)
}

func TestTaskArchiveRetestIndirectSharedSideHistoryRemainsHot(t *testing.T) {
	f := newRetestArchiveFixture(t)
	other, err := f.d.CreateTask("foreign side fixture", "preserve unrelated side history", nil, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	f.tasks = append(f.tasks, other.ID)
	var approval int64
	if err := f.d.QueryRow(`INSERT INTO intercept_pending(conversation_id,tool_name,status)
 VALUES($1,'synthetic_read','pending') RETURNING id`, f.conversation.ID).Scan(&approval); err != nil {
		t.Fatal(err)
	}
	key := fmt.Sprintf("foreign-side-via-approval-%d", other.ID)
	if _, err := f.d.Exec(`INSERT INTO side_question_sessions(session_key,task_id,exploration_id,approval_id,run_id,version,snapshot)
 VALUES($1,$2,$3,$4,1,1,'{}')`, key, other.ID, other.ExplorationID, approval); err != nil {
		t.Fatal(err)
	}
	a, s := f.snapshot(t)
	for _, table := range []string{"conversations", "conversation_activities", "intercept_pending", "side_question_sessions"} {
		if s.DataCounts[table] != 0 {
			t.Fatalf("foreign indirect history included in %s", table)
		}
	}
	if err := f.d.CompleteTaskArchive(a.ID, s, "synthetic-package", "synthetic-digest", 100, 50); err != nil {
		t.Fatal(err)
	}
	requireArchiveRowCount(t, f.d, `SELECT count(*) FROM conversations WHERE id=$1`, 1, f.conversation.ID)
	requireArchiveRowCount(t, f.d, `SELECT count(*) FROM intercept_pending WHERE id=$1 AND status='pending'`, 1, approval)
	requireArchiveRowCount(t, f.d, `SELECT count(*) FROM side_question_sessions WHERE session_key=$1 AND task_id=$2`, 1, key, other.ID)
	f.restore(t, a, s)
	history, err := f.d.ListFindingRetests(f.findingID)
	if err != nil || len(history) != 1 || history[0].ConversationID != nil {
		t.Fatal("foreign indirect history was rebound during restore")
	}
	requireArchiveRowCount(t, f.d, `SELECT count(*) FROM side_question_sessions WHERE session_key=$1 AND task_id=$2`, 1, key, other.ID)
}

func TestTaskArchiveRetestSideWithForeignApprovalKeepsConversationHot(t *testing.T) {
	f := newRetestArchiveFixture(t)
	other, err := f.d.CreateTask("foreign approval fixture", "preserve approval-owned history", nil, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	f.tasks = append(f.tasks, other.ID)
	var approval int64
	if err := f.d.QueryRow(`INSERT INTO intercept_pending(task_id,tool_name,status)
 VALUES($1,'synthetic_read','pending') RETURNING id`, fmt.Sprint(other.ID)).Scan(&approval); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = f.d.Exec(`DELETE FROM intercept_pending WHERE id=$1`, approval) })
	key := fmt.Sprintf("direct-side-foreign-approval-%d", other.ID)
	if _, err := f.d.Exec(`INSERT INTO side_question_sessions(session_key,conversation_id,approval_id,run_id,version,snapshot)
 VALUES($1,$2,$3,1,1,'{}')`, key, f.conversation.ID, approval); err != nil {
		t.Fatal(err)
	}
	a, s := f.snapshot(t)
	if s.DataCounts["conversations"] != 0 || s.DataCounts["side_question_sessions"] != 0 || s.DataCounts["intercept_pending"] != 0 {
		t.Fatal("foreign approval side history was moved with a retest conversation")
	}
	if err := f.d.CompleteTaskArchive(a.ID, s, "synthetic-package", "synthetic-digest", 100, 50); err != nil {
		t.Fatal(err)
	}
	requireArchiveRowCount(t, f.d, `SELECT count(*) FROM conversations WHERE id=$1`, 1, f.conversation.ID)
	requireArchiveRowCount(t, f.d, `SELECT count(*) FROM intercept_pending WHERE id=$1 AND task_id=$2`, 1, approval, fmt.Sprint(other.ID))
	requireArchiveRowCount(t, f.d, `SELECT count(*) FROM side_question_sessions WHERE session_key=$1 AND approval_id=$2`, 1, key, approval)
}

func TestTaskArchiveRetestIndirectForeignConversationHistoryRemainsHot(t *testing.T) {
	f := newRetestArchiveFixture(t)
	other, err := f.d.CreateTask("foreign conversation fixture", "preserve linked conversation side history", nil, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	f.tasks = append(f.tasks, other.ID)
	finding, err := f.d.Exploration(other.ExplorationID).RecordFinding(t.Context(), RecordFindingInput{TaskID: other.ID, Summary: "foreign retest finding", Severity: "low", Evidence: "synthetic other observation"})
	if err != nil {
		t.Fatal(err)
	}
	_, foreign, created, err := f.d.CreateFindingRetest(t.Context(), finding.FindingID, "synthetic foreign conversation")
	if err != nil || !created || foreign == nil {
		t.Fatalf("create foreign conversation: %v", err)
	}
	f.conversations = append(f.conversations, foreign.ID)
	var approval int64
	if err := f.d.QueryRow(`INSERT INTO intercept_pending(conversation_id,tool_name,status)
 VALUES($1,'synthetic_read','pending') RETURNING id`, f.conversation.ID).Scan(&approval); err != nil {
		t.Fatal(err)
	}
	key := fmt.Sprintf("foreign-conversation-side-%d", other.ID)
	if _, err := f.d.Exec(`INSERT INTO side_question_sessions(session_key,conversation_id,approval_id,run_id,version,snapshot)
 VALUES($1,$2,$3,1,1,'{}')`, key, foreign.ID, approval); err != nil {
		t.Fatal(err)
	}
	a, s := f.snapshot(t)
	if s.DataCounts["conversations"] != 0 || s.DataCounts["side_question_sessions"] != 0 || s.DataCounts["intercept_pending"] != 0 {
		t.Fatal("indirect foreign conversation was treated as owned task history")
	}
	if err := f.d.CompleteTaskArchive(a.ID, s, "synthetic-package", "synthetic-digest", 100, 50); err != nil {
		t.Fatal(err)
	}
	requireArchiveRowCount(t, f.d, `SELECT count(*) FROM conversations WHERE id=ANY($1::bigint[])`, 2, []int64{f.conversation.ID, foreign.ID})
	requireArchiveRowCount(t, f.d, `SELECT count(*) FROM side_question_sessions WHERE session_key=$1 AND conversation_id=$2`, 1, key, foreign.ID)
}

func TestTaskArchiveRetestLateApprovalAndSideChangesKeepHotHistory(t *testing.T) {
	for _, change := range []string{"approval", "request", "snapshot", "new_request"} {
		t.Run(change, func(t *testing.T) {
			f := newRetestArchiveFixture(t)
			var approval int64
			var key, request string
			if change != "approval" {
				approval, key, request = addRetestSideArchiveFixture(t, f)
			}
			a, s := f.snapshot(t)
			switch change {
			case "approval":
				if err := f.d.QueryRow(`INSERT INTO intercept_pending(conversation_id,tool_name,status)
 VALUES($1,'synthetic_read','pending') RETURNING id`, f.conversation.ID).Scan(&approval); err != nil {
					t.Fatal(err)
				}
			case "request":
				if _, err := f.d.Exec(`UPDATE side_question_requests SET answer='late answer' WHERE id=$1`, request); err != nil {
					t.Fatal(err)
				}
			case "snapshot":
				if _, err := f.d.Exec(`UPDATE side_question_sessions SET snapshot='{"late":true}' WHERE session_key=$1`, key); err != nil {
					t.Fatal(err)
				}
			case "new_request":
				if _, err := f.d.Exec(`INSERT INTO side_question_requests(id,session_key,generation,client_id,question,status,model,snapshot_at)
 VALUES($1,$2,0,'late-fixture','late question','completed','{}',now())`, request+"-late", key); err != nil {
					t.Fatal(err)
				}
			}
			if err := f.d.CompleteTaskArchive(a.ID, s, "synthetic-package", "synthetic-digest", 100, 50); err == nil {
				t.Fatal("late approval or side history was cascaded from a stale archive")
			}
			requireArchiveRowCount(t, f.d, `SELECT count(*) FROM conversations WHERE id=$1`, 1, f.conversation.ID)
			requireArchiveRowCount(t, f.d, `SELECT count(*) FROM intercept_pending WHERE id=$1`, 1, approval)
			if key != "" {
				want := int64(1)
				if change == "new_request" {
					want = 2
				}
				requireArchiveRowCount(t, f.d, `SELECT count(*) FROM side_question_requests WHERE session_key=$1`, want, key)
			}
		})
	}
}

func TestTaskArchiveRetestSnapshotChangesDoNotDeleteHotHistory(t *testing.T) {
	for _, change := range []string{"append", "result", "foreign_conversation"} {
		t.Run(change, func(t *testing.T) {
			f := newRetestArchiveFixture(t)
			a, s := f.snapshot(t)
			switch change {
			case "append":
				_, err := f.d.Exec(`INSERT INTO conversation_activities(conversation_id,worker,kind,summary) VALUES($1,'retester','text','late synthetic history')`, f.conversation.ID)
				if err != nil {
					t.Fatal(err)
				}
			case "result":
				if _, err := f.d.Exec(`UPDATE finding_retests SET notes='late synthetic update' WHERE id=$1`, f.retest.ID); err != nil {
					t.Fatal(err)
				}
			case "foreign_conversation":
				other, err := f.d.CreateConversation(FindingRetestAgentKey, "unrelated chat", nil)
				if err != nil {
					t.Fatal(err)
				}
				f.conversations = append(f.conversations, other.ID)
				var raw []byte
				if err := f.d.QueryRow(`SELECT json_agg(c) FROM conversations c WHERE id=$1`, other.ID).Scan(&raw); err != nil {
					t.Fatal(err)
				}
				s.Tables["conversations"] = raw
			}
			if err := f.d.CompleteTaskArchive(a.ID, s, "synthetic-package", "synthetic-digest", 100, 50); err == nil {
				t.Fatal("changed or foreign history was compacted from a stale snapshot")
			}
			requireArchiveRowCount(t, f.d, `SELECT count(*) FROM finding_retests WHERE id=$1`, 1, f.retest.ID)
			requireArchiveRowCount(t, f.d, `SELECT count(*) FROM conversations WHERE id=$1`, 1, f.conversation.ID)
		})
	}
}

func TestTaskArchiveRetestLegacyMissingTablesDoesNotFabricateHistory(t *testing.T) {
	f := newRetestArchiveFixture(t)
	a, s := f.snapshot(t)
	if err := f.d.CompleteTaskArchive(a.ID, s, "synthetic-package", "synthetic-digest", 100, 50); err != nil {
		t.Fatal(err)
	}
	s.FormatVersion = 5
	for _, table := range []string{"finding_retests", "conversations", "conversation_activities"} {
		delete(s.Tables, table)
		delete(s.DataCounts, table)
	}
	f.restore(t, a, s)
	requireArchiveRowCount(t, f.d, `SELECT count(*) FROM finding_retests WHERE finding_id=$1`, 0, f.findingID)
	requireArchiveRowCount(t, f.d, `SELECT count(*) FROM conversations WHERE id=$1`, 0, f.conversation.ID)
}

func TestTaskArchiveRetestRestoreRejectsForeignSourceAndActivities(t *testing.T) {
	for _, change := range []string{"task", "finding", "node", "activity", "role", "side_owner"} {
		t.Run(change, func(t *testing.T) {
			f := newRetestArchiveFixture(t)
			if change == "side_owner" {
				addRetestSideArchiveFixture(t, f)
			}
			a, s := f.snapshot(t)
			if err := f.d.CompleteTaskArchive(a.ID, s, "synthetic-package", "synthetic-digest", 100, 50); err != nil {
				t.Fatal(err)
			}
			table := "finding_retests"
			if change == "activity" {
				table = "conversation_activities"
			} else if change == "role" {
				table = "conversations"
			} else if change == "side_owner" {
				table = "side_question_sessions"
			}
			rows, err := decodeArchiveRows(s.Tables[table])
			if err != nil {
				t.Fatal(err)
			}
			switch change {
			case "task":
				rows[0]["snapshot"].(map[string]any)["finding"].(map[string]any)["task_id"] = f.task.ID + 100000
			case "finding":
				rows[0]["snapshot"].(map[string]any)["finding"].(map[string]any)["id"] = f.findingID + 100000
			case "node":
				rows[0]["snapshot"].(map[string]any)["finding"].(map[string]any)["node_id"] = int64(1000000000)
			case "activity":
				rows[0]["conversation_id"] = f.conversation.ID + 100000
			case "role":
				rows[0]["agent_key"] = "auto"
			case "side_owner":
				rows[0]["task_id"] = f.task.ID + 100000
			}
			s.Tables[table], err = json.Marshal(rows)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := f.d.QueueTaskArchiveRestore(a.ID); err != nil {
				t.Fatal(err)
			}
			if _, err := f.d.ClaimTaskArchiveJob(t.Context()); err != nil {
				t.Fatal(err)
			}
			if _, err := f.d.RestoreTaskArchive(a.ID, s, 0); err == nil {
				t.Fatal("foreign retest source/activity was restored")
			}
			requireArchiveRowCount(t, f.d, `SELECT count(*) FROM findings WHERE id=$1`, 0, f.findingID)
			requireArchiveRowCount(t, f.d, `SELECT count(*) FROM conversations WHERE id=$1`, 0, f.conversation.ID)
		})
	}
}

func TestTaskArchiveRetestFormatRequiresNewHistoryTables(t *testing.T) {
	if !IsTaskArchiveFormatSupported(5) || TaskArchiveFormatVersion != 6 {
		t.Fatal("archive compatibility boundary changed incorrectly")
	}
	for _, table := range []string{"finding_retests", "conversations", "conversation_activities", "intercept_pending", "side_question_sessions", "side_question_requests"} {
		s := &TaskArchiveSnapshot{FormatVersion: 6, Tables: map[string]json.RawMessage{}, DataCounts: map[string]int64{}}
		for _, name := range []string{"finding_retests", "conversations", "conversation_activities", "intercept_pending", "side_question_sessions", "side_question_requests"} {
			s.Tables[name], s.DataCounts[name] = json.RawMessage(`[]`), 0
		}
		delete(s.Tables, table)
		if err := requireRetestArchiveTables(s); err == nil {
			t.Fatalf("new-format history table %s was silently omitted", table)
		}
		for _, invalid := range []json.RawMessage{nil, json.RawMessage(`null`), json.RawMessage(`[] {}`)} {
			s.Tables[table] = invalid
			if err := requireRetestArchiveTables(s); err == nil {
				t.Fatalf("invalid new-format history table %s was accepted", table)
			}
		}
	}
}
