package server

import (
	"context"
	"fmt"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Autumn-27/artex/agent"
	"github.com/Autumn-27/artex/db"
)

func taskConversationTriggerAgent(t *testing.T, s *Server, key string) {
	t.Helper()
	a, err := s.m.pg.GetAgentByKey(key)
	if err != nil {
		t.Fatal(err)
	}
	created := a == nil
	if created {
		a, err = s.m.pg.CreateAgent(key, "synthetic trigger fixture", "saved-record lifecycle only")
		if err != nil {
			t.Fatal(err)
		}
	}
	oldEnabled, oldProfile := a.Enabled, a.LLMProfileID
	if _, err := s.m.pg.Exec(`UPDATE agents SET enabled=true,llm_profile_id=NULL WHERE id=$1`, a.ID); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if created {
			_, _ = s.m.pg.Exec(`DELETE FROM agents WHERE id=$1`, a.ID)
		} else {
			_, _ = s.m.pg.Exec(`UPDATE agents SET enabled=$2,llm_profile_id=$3 WHERE id=$1`, a.ID, oldEnabled, oldProfile)
		}
	})
}

func taskConversationTriggerCounts(t *testing.T, s *Server, key, title string) (int, int) {
	t.Helper()
	var conversations, activities int
	if err := s.m.pg.QueryRow(`SELECT count(*) FROM conversations WHERE agent_key=$1 AND title=$2`, key, title).Scan(&conversations); err != nil {
		t.Fatal(err)
	}
	if err := s.m.pg.QueryRow(`SELECT count(*) FROM conversation_activities a JOIN conversations c ON c.id=a.conversation_id WHERE c.agent_key=$1 AND c.title=$2`, key, title).Scan(&activities); err != nil {
		t.Fatal(err)
	}
	return conversations, activities
}

func TestTaskConversationGenericTriggerRejectsBeforeCreatingConversationOrBody(t *testing.T) {
	for _, rejection := range []string{"deleted_owner", "runtime_missing", "closing"} {
		t.Run(rejection, func(t *testing.T) {
			s, _, parent, calls := taskConversationReviewFixture(t)
			key := "synthetic-trigger-" + parent.ID
			taskConversationTriggerAgent(t, s, key)
			title := "Rejected synthetic trigger " + parent.ID
			t.Cleanup(func() { _, _ = s.m.pg.Exec(`DELETE FROM conversations WHERE agent_key=$1 AND title=$2`, key, title) })
			id, _ := strconv.ParseInt(parent.ID, 10, 64)
			if rejection == "deleted_owner" {
				if !s.beginTaskDelete(parent.ID) {
					t.Fatal("synthetic task deletion barrier was not installed")
				}
			} else if rejection == "runtime_missing" {
				delete(s.m.tasks, parent.ID)
			} else {
				s.stopTaskConversationRuns()
			}
			beforeConversations, beforeActivities := taskConversationTriggerCounts(t, s, key, title)
			s.runTriggeredRun(triggeredRun{agentKey: key, title: title, message: "Synthetic rejected body must not be persisted", taskID: id})
			conversations, activities := taskConversationTriggerCounts(t, s, key, title)
			if conversations != beforeConversations || activities != beforeActivities || calls.Load() != 0 || s.engine.inflightCount(parent.ID) != 0 {
				t.Fatalf("rejected trigger created work: conversations=%d activities=%d calls=%d", conversations-beforeConversations, activities-beforeActivities, calls.Load())
			}
			registered := 0
			s.taskConversationRuns.Range(func(_, _ any) bool { registered++; return true })
			if registered != 0 {
				t.Fatal("rejected trigger retained a prepared run")
			}
		})
	}
}

func TestTaskConversationGenericTriggerRegistersInflightBeforeFirstBodyInsert(t *testing.T) {
	for _, role := range []string{"custom", "auto"} {
		t.Run(role, func(t *testing.T) {
			s, _, parent, calls := taskConversationReviewFixture(t)
			key := role
			if role == "custom" {
				key = "synthetic-admitted-" + parent.ID
			}
			taskConversationTriggerAgent(t, s, key)
			title := "Admitted synthetic trigger " + parent.ID
			t.Cleanup(func() { _, _ = s.m.pg.Exec(`DELETE FROM conversations WHERE agent_key=$1 AND title=$2`, key, title) })
			// This transaction is a synthetic SQL gate, not a runtime service.
			// SHARE permits the observer SELECTs but holds the very first INSERT
			// until its already-registered ownership has been inspected.
			gate, err := s.m.pg.Begin()
			if err != nil {
				t.Fatal(err)
			}
			defer gate.Rollback()
			if _, err := gate.Exec(`LOCK TABLE conversation_activities IN SHARE MODE`); err != nil {
				t.Fatal(err)
			}
			var gatePID int
			if err := gate.QueryRow(`SELECT pg_backend_pid()`).Scan(&gatePID); err != nil {
				t.Fatal(err)
			}
			id, _ := strconv.ParseInt(parent.ID, 10, 64)
			done := make(chan struct{})
			go func() {
				s.runTriggeredRun(triggeredRun{agentKey: key, title: title, message: "Synthetic admitted body; no target interaction", taskID: id})
				close(done)
			}()
			t.Cleanup(func() {
				_ = gate.Rollback()
				taskConversationAwait(t, done, "SQL-gated trigger exit")
			})
			deadline := time.Now().Add(3 * time.Second)
			for {
				var blocked bool
				if err := s.m.pg.QueryRow(`SELECT EXISTS(SELECT 1 FROM pg_locks l
				 WHERE l.locktype='relation' AND l.relation='conversation_activities'::regclass
				 AND l.mode='RowExclusiveLock' AND NOT l.granted AND $1=ANY(pg_blocking_pids(l.pid)))`, gatePID).Scan(&blocked); err != nil {
					t.Fatal(err)
				}
				if blocked {
					break
				}
				select {
				case <-done:
					t.Fatal("trigger exited without reaching its first body INSERT")
				default:
				}
				if time.Now().After(deadline) {
					t.Fatal("synthetic first body INSERT did not reach the SQL gate")
				}
				time.Sleep(5 * time.Millisecond)
			}
			var conversationID int64
			if err := s.m.pg.QueryRow(`SELECT id FROM conversations WHERE agent_key=$1 AND title=$2`, key, title).Scan(&conversationID); err != nil {
				t.Fatal(err)
			}
			value, registered := s.taskConversationRuns.Load(conversationID)
			if !registered || s.engine.inflightCount(parent.ID) != 1 || calls.Load() != 0 {
				t.Fatalf("first body INSERT preceded run ownership: registered=%v inflight=%d calls=%d", registered, s.engine.inflightCount(parent.ID), calls.Load())
			}
			run := value.(*taskConversationRun)
			if len(run.owners) != 1 || run.owners[0] != parent.ID || agent.RunInfoFrom(run.ctx).TaskID != 0 {
				t.Fatal("generic trigger ownership was missing or became native task execution")
			}
			if _, activities := taskConversationTriggerCounts(t, s, key, title); activities != 0 {
				t.Fatal("body was persisted before the held SQL gate")
			}
			if err := gate.Rollback(); err != nil {
				t.Fatal(err)
			}
			taskConversationAwait(t, done, "admitted trigger completion")
			if calls.Load() != 1 || s.engine.inflightCount(parent.ID) != 0 {
				t.Fatalf("admitted synthetic trigger did not finish cleanly: calls=%d inflight=%d", calls.Load(), s.engine.inflightCount(parent.ID))
			}
		})
	}
}

func taskConversationTriggerPendingFinding(t *testing.T, s *Server, findingID int64, table string) (*db.Conversation, func(context.Context) (bool, error)) {
	t.Helper()
	var c *db.Conversation
	var start func(context.Context) (bool, error)
	if table == "finding_retests" {
		retest, conversation, created, err := s.m.pg.CreateFindingRetest(t.Context(), findingID, "Synthetic cancellation-race fixture")
		if err != nil || !created || retest == nil || conversation == nil {
			t.Fatal("create pending synthetic retest", err)
		}
		c = conversation
		start = func(ctx context.Context) (bool, error) { return s.m.pg.StartFindingRetest(ctx, retest.ID) }
	} else {
		check, conversation, created, err := s.m.pg.CreateFindingCheck(t.Context(), findingID, db.FindingCheckKindVerify)
		if err != nil || !created || check == nil || conversation == nil {
			t.Fatal("create pending synthetic check", err)
		}
		c = conversation
		start = func(ctx context.Context) (bool, error) { return s.m.pg.StartFindingCheck(ctx, check.ID) }
	}
	t.Cleanup(func() { _ = s.m.pg.DeleteConversation(c.ID) })
	return c, start
}

func taskConversationTriggerFindingRow(t *testing.T, s *Server, table string, conversationID int64) (string, string, string, bool) {
	t.Helper()
	var status, snapshot, revision string
	var finished bool
	if err := s.m.pg.QueryRow(`SELECT status,to_jsonb(r)::text,xmin::text,finished_at IS NOT NULL FROM `+table+` r WHERE conversation_id=$1`, conversationID).
		Scan(&status, &snapshot, &revision, &finished); err != nil {
		t.Fatal(err)
	}
	return status, snapshot, revision, finished
}

func TestTaskConversationFindingCancellationAtValidateAndStartIsStopped(t *testing.T) {
	for _, table := range []string{"finding_retests", "finding_checks"} {
		for _, landing := range []string{"validate", "start"} {
			t.Run(table+"/"+landing, func(t *testing.T) {
				s, findingID, parent, calls := taskConversationReviewFixture(t)
				c, _ := taskConversationTriggerPendingFinding(t, s, findingID, table)
				parentID, _ := strconv.ParseInt(parent.ID, 10, 64)
				otherFindingID, err := s.m.pg.AddFinding(parentID, 0, "synthetic stop-race", "unrelated running fixture", "low", "saved fixture only", "synthetic evidence", "fixture", nil)
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _, _ = s.m.pg.DeleteFinding(otherFindingID) })
				other, startOther := taskConversationTriggerPendingFinding(t, s, otherFindingID, table)
				if started, err := startOther(t.Context()); err != nil || !started {
					t.Fatal("start unrelated synthetic finding run", err)
				}
				beforeStatus, beforeSnapshot, beforeRevision, beforeFinished := taskConversationTriggerFindingRow(t, s, table, other.ID)
				if beforeStatus != "running" || beforeFinished {
					t.Fatal("unrelated fixture is not an active running record")
				}
				ctx, cancel := context.WithCancelCause(t.Context())
				defer cancel(agent.AbortChatTurnFinished)
				entered, release, done := make(chan struct{}), make(chan struct{}), make(chan struct{})
				var releaseOnce sync.Once
				unblock := func() { releaseOnce.Do(func() { close(release) }) }
				var validations, starts atomic.Int32
				validate := func() error {
					validations.Add(1)
					if landing == "validate" {
						close(entered)
						<-release
						return ctx.Err()
					}
					return nil
				}
				start := func() (bool, error) {
					starts.Add(1)
					close(entered)
					<-release
					return false, ctx.Err()
				}
				admitted := true
				go func() {
					admitted = s.beginFindingConversation(ctx, c, table, validate, start)
					close(done)
				}()
				t.Cleanup(func() {
					cancel(agent.AbortChatStoppedByUser)
					unblock()
					taskConversationAwait(t, done, "cancelled finding admission exit")
				})
				taskConversationAwait(t, entered, fmt.Sprintf("%s cancellation landing", landing))
				cancel(agent.AbortChatStoppedByUser)
				unblock()
				taskConversationAwait(t, done, "cancelled finding admission")
				status, _, _, finished := taskConversationTriggerFindingRow(t, s, table, c.ID)
				wantStarts := int32(0)
				if landing == "start" {
					wantStarts = 1
				}
				if admitted || status != "stopped" || !finished || validations.Load() != 1 || starts.Load() != wantStarts || calls.Load() != 0 {
					t.Fatalf("cancelled admission was not stopped: admitted=%v status=%s finished=%v validates=%d starts=%d calls=%d", admitted, status, finished, validations.Load(), starts.Load(), calls.Load())
				}
				// Even explicitly encountering an already-running record with the
				// cancelled context must not overwrite another invocation's owner.
				if s.beginFindingConversation(ctx, other, table, func() error { return ctx.Err() }, func() (bool, error) { return false, ctx.Err() }) {
					t.Fatal("cancelled context admitted an unrelated running record")
				}
				afterStatus, afterSnapshot, afterRevision, afterFinished := taskConversationTriggerFindingRow(t, s, table, other.ID)
				if beforeStatus != afterStatus || beforeSnapshot != afterSnapshot || beforeRevision != afterRevision || beforeFinished != afterFinished || calls.Load() != 0 {
					t.Fatal("cancellation sealing changed an unrelated running record")
				}
			})
		}
	}
}

func TestTaskConversationPreparedFailureCleanupKeepsInflightUntilCompletion(t *testing.T) {
	s, findingID := newRetestServer(t)
	finding, err := s.m.pg.GetFinding(findingID)
	if err != nil || finding == nil || finding.TaskID == nil {
		t.Fatal("missing synthetic parent")
	}
	parent, _ := s.m.Task(strconv.FormatInt(*finding.TaskID, 10))
	c, err := s.m.pg.CreateConversation("auto", "Synthetic prepared failure cleanup", nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.m.pg.DeleteConversation(c.ID) })
	run := taskConversationPrepare(t, s, c, []int64{*finding.TaskID})
	run.cancel(agent.AbortTaskDeleted)
	completed := false
	first := db.Activity{Worker: "auto", Kind: "user", Summary: "Synthetic rejected first body", Detail: "Synthetic rejected first body"}
	err = s.runPreparedConversation(run, c, "Synthetic rejected turn", &first, func(runErr error) {
		completed = true
		if runErr == nil || s.engine.inflightCount(parent.ID) != 1 {
			t.Error("completion escaped its inflight lifetime")
		}
		select {
		case <-run.done:
			t.Error("run done signalled before completion callback")
		default:
		}
		var bodies int
		if queryErr := s.m.pg.QueryRow(`SELECT count(*) FROM conversation_activities WHERE conversation_id=$1`, c.ID).Scan(&bodies); queryErr != nil || bodies != 0 {
			t.Error("cancelled first body was persisted")
		}
	})
	if err == nil || !completed || s.engine.inflightCount(parent.ID) != 0 {
		t.Fatal("prepared failure did not complete and release exactly once")
	}
}
