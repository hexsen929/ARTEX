package server

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Autumn-27/artex/agent"
	"github.com/Autumn-27/artex/db"
	"github.com/Autumn-27/norma/llm"
)

// These fixtures use only saved synthetic records and an in-process provider.
// No tool, target, external model or running ARTEX instance is contacted.
func taskConversationReviewFixture(t *testing.T) (*Server, int64, *Task, *atomic.Int32) {
	t.Helper()
	s, findingID := newRetestServer(t)
	finding, err := s.m.pg.GetFinding(findingID)
	if err != nil || finding == nil || finding.TaskID == nil {
		t.Fatal("synthetic finding parent missing", err)
	}
	parent, ok := s.m.Task(strconv.FormatInt(*finding.TaskID, 10))
	if !ok {
		t.Fatal("synthetic runtime missing")
	}
	a, err := s.m.pg.GetAgentByKey("reporter")
	if err != nil {
		t.Fatal(err)
	}
	created := a == nil
	if created {
		a, err = s.m.pg.CreateAgent("reporter", "synthetic reviewer", "fixture only")
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
	calls := &atomic.Int32{}
	agent.ToolAugment = nil
	setRetestProvider(s, retestProvider{complete: func(context.Context, llm.CompletionRequest) (llm.Message, string, llm.Usage, error) {
		calls.Add(1)
		return llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{llm.TextBlock("Synthetic saved-record review finished; no target interaction")}}, "end_turn", llm.Usage{}, nil
	}})
	return s, findingID, parent, calls
}

func taskConversationReviewReservation(t *testing.T, s *Server, findingID int64, parent *Task) triggeredRun {
	t.Helper()
	id, err := strconv.ParseInt(parent.ID, 10, 64)
	if err != nil {
		t.Fatal(err)
	}
	reservations, err := s.m.pg.CreateFindingCaseReviews(t.Context(), map[int64][]int64{id: {findingID}})
	if err != nil || len(reservations) != 1 {
		t.Fatal("reserve synthetic review", err)
	}
	conversationID := reservations[0].ConversationID
	t.Cleanup(func() { _ = s.m.pg.DeleteConversation(conversationID) })
	return triggeredRun{conversationID: conversationID, agentKey: "reporter", title: "Synthetic review", message: "Explain saved synthetic records only", taskID: id}
}

type taskConversationReviewRow struct {
	state, reason, snapshot, revision string
}

func taskConversationReviewRead(t *testing.T, s *Server, id int64) taskConversationReviewRow {
	t.Helper()
	var row taskConversationReviewRow
	if err := s.m.pg.QueryRow(`SELECT state,error,to_jsonb(r)::text,xmin::text FROM finding_case_review_runs r WHERE conversation_id=$1`, id).
		Scan(&row.state, &row.reason, &row.snapshot, &row.revision); err != nil {
		t.Fatal(err)
	}
	return row
}

func TestTaskConversationRetestReadDoesNotTakeAdmissionLock(t *testing.T) {
	s, findingID := newRetestServer(t)
	s.concMu.Lock()
	defer s.concMu.Unlock()
	done := make(chan struct{})
	var response *httptest.ResponseRecorder
	go func() {
		response = retestRequest(s.listFindingRetests, http.MethodGet, findingID, "")
		close(done)
	}()
	t.Cleanup(func() { taskConversationAwait(t, done, "read-only retest response") })
	select {
	case <-done:
		if response.Code != http.StatusOK {
			t.Fatal(response.Code, response.Body.String())
		}
	case <-time.After(time.Second):
		t.Fatal("read-only retest listing waited for the execution admission lock")
	}
}

func TestTaskConversationRetestCreationWaitsForAdmissionLock(t *testing.T) {
	s, findingID := newRetestServer(t)
	var calls atomic.Int32
	setRetestProvider(s, retestProvider{complete: func(context.Context, llm.CompletionRequest) (llm.Message, string, llm.Usage, error) {
		calls.Add(1)
		return llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{llm.TextBlock("Synthetic fixture finished")}}, "end_turn", llm.Usage{}, nil
	}})
	s.concMu.Lock()
	var once sync.Once
	unlock := func() { once.Do(s.concMu.Unlock) }
	done, entered := make(chan struct{}), make(chan struct{})
	var response *httptest.ResponseRecorder
	go func() {
		close(entered)
		response = retestRequest(s.startFindingRetest, http.MethodPost, findingID, `{}`)
		close(done)
	}()
	t.Cleanup(func() {
		unlock()
		taskConversationAwait(t, done, "retest creation response")
	})
	taskConversationAwait(t, entered, "retest request entry")
	select {
	case <-done:
		t.Fatal("retest creation bypassed the held admission lock")
	case <-time.After(100 * time.Millisecond):
	}
	rows, err := s.m.pg.ListFindingRetests(findingID)
	if err != nil || len(rows) != 0 || calls.Load() != 0 {
		t.Fatalf("blocked creation had side effects: rows=%d calls=%d err=%v", len(rows), calls.Load(), err)
	}
	unlock()
	taskConversationAwait(t, done, "admitted retest response")
	if response.Code != http.StatusAccepted {
		t.Fatal(response.Code, response.Body.String())
	}
	waitRetestIdle(t, s)
	if calls.Load() != 1 {
		t.Fatalf("admitted fixture model calls=%d want 1", calls.Load())
	}
}

func TestTaskConversationRetestDeleteBarrierWinsAdmission(t *testing.T) {
	s, findingID, parent, calls := taskConversationReviewFixture(t)
	if !s.beginTaskDelete(parent.ID) {
		t.Fatal("synthetic delete barrier not installed")
	}
	response := retestRequest(s.startFindingRetest, http.MethodPost, findingID, `{}`)
	if response.Code != http.StatusConflict || calls.Load() != 0 || s.engine.inflightCount(parent.ID) != 0 {
		t.Fatalf("delete barrier lost admission: status=%d calls=%d body=%s", response.Code, calls.Load(), response.Body.String())
	}
}

func TestTaskConversationReviewClaimFailsOnlyItsOwnRejectedRound(t *testing.T) {
	for _, rejection := range []string{"parent_paused", "runtime_missing"} {
		t.Run(rejection, func(t *testing.T) {
			s, findingID, parent, calls := taskConversationReviewFixture(t)
			item := taskConversationReviewReservation(t, s, findingID, parent)
			if rejection == "parent_paused" {
				if _, err := s.m.pg.Exec(`UPDATE tasks SET paused=true WHERE id=$1`, parent.ID); err != nil {
					t.Fatal(err)
				}
			} else {
				delete(s.m.tasks, parent.ID)
			}
			s.runTriggeredRun(item)
			row := taskConversationReviewRead(t, s, item.conversationID)
			if row.state != "failed" || strings.TrimSpace(row.reason) == "" || calls.Load() != 0 || s.engine.inflightCount(parent.ID) != 0 {
				t.Fatalf("rejected queued review was not sealed: state=%s reason=%s calls=%d", row.state, row.reason, calls.Load())
			}
			if _, active := s.taskConversationRuns.Load(item.conversationID); active {
				t.Fatal("rejected review retained an active run")
			}
		})
	}
}

func TestTaskConversationReviewRepeatedRunningAndDoneTriggersAreNoops(t *testing.T) {
	for _, state := range []string{"running", "done"} {
		t.Run(state, func(t *testing.T) {
			s, findingID, parent, calls := taskConversationReviewFixture(t)
			item := taskConversationReviewReservation(t, s, findingID, parent)
			if _, err := s.m.pg.Exec(`UPDATE finding_case_review_runs SET state=$2,error='preserve synthetic owner',conclusion='{"owner":"existing"}' WHERE conversation_id=$1`, item.conversationID, state); err != nil {
				t.Fatal(err)
			}
			before := taskConversationReviewRead(t, s, item.conversationID)
			s.runTriggeredRun(item)
			s.runTriggeredRun(item)
			after := taskConversationReviewRead(t, s, item.conversationID)
			if before != after || calls.Load() != 0 || s.engine.inflightCount(parent.ID) != 0 {
				t.Fatalf("duplicate %s review executed or changed its row: before=%+v after=%+v calls=%d", state, before, after, calls.Load())
			}
			var activities int
			if err := s.m.pg.QueryRow(`SELECT count(*) FROM conversation_activities WHERE conversation_id=$1`, item.conversationID).Scan(&activities); err != nil || activities != 0 {
				t.Fatalf("duplicate trigger appended activities: count=%d err=%v", activities, err)
			}
		})
	}
}

func TestTaskConversationReviewRejectedOldClaimCannotSealNewRevision(t *testing.T) {
	s, findingID, parent, calls := taskConversationReviewFixture(t)
	item := taskConversationReviewReservation(t, s, findingID, parent)
	delete(s.m.tasks, parent.ID)
	s.concMu.Lock()
	var once sync.Once
	unlock := func() { once.Do(s.concMu.Unlock) }
	done := make(chan struct{})
	go func() { s.runTriggeredRun(item); close(done) }()
	t.Cleanup(func() {
		unlock()
		taskConversationAwait(t, done, "old review trigger exit")
	})
	deadline := time.Now().Add(3 * time.Second)
	for {
		row := taskConversationReviewRead(t, s, item.conversationID)
		var users int
		if err := s.m.pg.QueryRow(`SELECT count(*) FROM conversation_activities WHERE conversation_id=$1 AND kind='user'`, item.conversationID).Scan(&users); err != nil {
			t.Fatal(err)
		}
		if row.state == "running" {
			if users != 0 {
				t.Fatal("queued body was written before lifecycle admission")
			}
			break // Claim committed; body/execution still await concMu admission.
		}
		if time.Now().After(deadline) {
			t.Fatal("trigger did not claim its queued row before admission")
		}
		time.Sleep(5 * time.Millisecond)
	}
	if _, err := s.m.pg.Exec(`UPDATE finding_case_review_runs SET error='new revision must survive',conclusion='{"owner":"replacement"}' WHERE conversation_id=$1`, item.conversationID); err != nil {
		t.Fatal(err)
	}
	replacement := taskConversationReviewRead(t, s, item.conversationID)
	unlock()
	taskConversationAwait(t, done, "rejected old claim")
	if after := taskConversationReviewRead(t, s, item.conversationID); after != replacement || calls.Load() != 0 {
		t.Fatalf("old rejected claim changed replacement revision: before=%+v after=%+v calls=%d", replacement, after, calls.Load())
	}
}

func TestTaskConversationDedicatedReportRequiresMatchingRuntime(t *testing.T) {
	for _, state := range []string{"missing", "wrong_exploration", "valid"} {
		t.Run(state, func(t *testing.T) {
			s, findingID, parent, calls := taskConversationReviewFixture(t)
			c, binding, err := s.m.pg.EnsureFindingReportConversation(t.Context(), findingID)
			if err != nil || c == nil || binding == nil {
				t.Fatal("create authoritative dedicated report binding", err)
			}
			t.Cleanup(func() { _ = s.m.pg.DeleteConversation(c.ID) })
			if state == "missing" {
				delete(s.m.tasks, parent.ID)
			} else if state == "wrong_exploration" {
				parent.Store = s.m.pg.Exploration(parent.Store.ID() + 1000000)
			}
			s.concMu.Lock()
			run, err := s.prepareConversationRunLocked(c, s.convBusyKey(c.ID), nil)
			s.concMu.Unlock()
			if run != nil {
				t.Cleanup(func() { s.finishConversationRun(run) })
			}
			if state != "valid" {
				if err == nil || run != nil || calls.Load() != 0 || s.engine.inflightCount(parent.ID) != 0 {
					t.Fatal("invalid dedicated report runtime passed prepare", err)
				}
				if err := s.runConversation(c, "Synthetic binding must fail before model entry", s.convBusyKey(c.ID)); err == nil || calls.Load() != 0 {
					t.Fatal("invalid dedicated report reached the model")
				}
				return
			}
			if err != nil || run == nil || agent.RunInfoFrom(run.ctx).TaskID != binding.TaskID || agent.RunInfoFrom(run.ctx).ExplorationID != parent.Store.ID() {
				t.Fatal("real dedicated report binding was not usable", err)
			}
			s.finishConversationRun(run)
			// A normal dedicated report is not a case review, so absence of a
			// review row must not suppress its ordinary trigger execution.
			s.runTriggeredRun(triggeredRun{conversationID: c.ID, agentKey: "reporter", message: "Explain saved synthetic report", taskID: binding.TaskID})
			var reviews int
			if err := s.m.pg.QueryRow(`SELECT count(*) FROM finding_case_review_runs WHERE conversation_id=$1`, c.ID).Scan(&reviews); err != nil || reviews != 0 || calls.Load() != 1 {
				t.Fatalf("ordinary dedicated report was treated as a duplicate review: reviews=%d calls=%d err=%v", reviews, calls.Load(), err)
			}
		})
	}
}

func taskConversationReviewExtraOwner(t *testing.T, s *Server, label string) *Task {
	t.Helper()
	row, err := s.m.pg.CreateTask(label, "Synthetic lifecycle ownership only", nil, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	parent := &Task{ID: strconv.FormatInt(row.ID, 10), ExpID: row.ExplorationID, Store: s.m.pg.Exploration(row.ExplorationID), Status: "running"}
	s.m.tasks[parent.ID] = parent
	t.Cleanup(func() {
		_, _ = s.m.pg.Exec(`DELETE FROM tasks WHERE id=$1`, row.ID)
		_, _ = s.m.pg.Exec(`DELETE FROM explorations WHERE id=$1`, row.ExplorationID)
	})
	return parent
}

func TestTaskConversationTrustedCustomHintsAreLifecycleOnly(t *testing.T) {
	for _, role := range []string{"custom-owner-fixture", "auto", "pentest"} {
		t.Run(role, func(t *testing.T) {
			s, _, first, calls := taskConversationReviewFixture(t)
			second := taskConversationReviewExtraOwner(t, s, "second synthetic owner")
			other := taskConversationReviewExtraOwner(t, s, "unrelated synthetic owner")
			firstID, _ := strconv.ParseInt(first.ID, 10, 64)
			secondID, _ := strconv.ParseInt(second.ID, 10, 64)
			otherID, _ := strconv.ParseInt(other.ID, 10, 64)
			// Any attempt to choose a task proxy fails, even if the caller does
			// not subsequently use it. Lifecycle hints must never reach here.
			s.m.taskProxyInitErr = errors.New("synthetic task proxy must not be resolved for lifecycle-only owners")
			for _, tc := range []struct {
				name  string
				hints []int64
				want  []string
			}{
				{name: "no_hints", want: []string{}},
				{name: "single", hints: []int64{firstID, firstID}, want: []string{first.ID}},
				{name: "multiple", hints: []int64{firstID, secondID, firstID}, want: []string{first.ID, second.ID}},
			} {
				t.Run(tc.name, func(t *testing.T) {
					makeConversation := func(label string) *db.Conversation {
						c, err := s.m.pg.CreateConversation(role, label, nil)
						if err != nil {
							t.Fatal(err)
						}
						t.Cleanup(func() { _ = s.m.pg.DeleteConversation(c.ID) })
						return c
					}
					run := taskConversationPrepare(t, s, makeConversation("lifecycle-only owner fixture"), tc.hints)
					unrelated := taskConversationPrepare(t, s, makeConversation("unrelated owner fixture"), []int64{otherID})
					if !reflect.DeepEqual(run.owners, tc.want) || agent.RunInfoFrom(run.ctx) != (agent.RunInfo{}) || calls.Load() != 0 {
						t.Fatalf("lifecycle metadata changed execution identity: owners=%v info=%+v calls=%d", run.owners, agent.RunInfoFrom(run.ctx), calls.Load())
					}
					for _, owner := range tc.want {
						if s.engine.inflightCount(owner) != 1 {
							t.Fatal("trusted owner was not registered inflight", owner)
						}
					}
					for _, nativeRole := range []string{role, "worker", "planner", "mainagent"} {
						tool, managed, err := agent.TaskDomainTool(run.ctx, nativeRole, "list_facts")
						if err != nil || !managed || tool != nil {
							t.Fatal("lifecycle hints manufactured a native task-reader registry", nativeRole, err)
						}
					}
					cancelled := s.cancelTaskConversationRuns(first.ID, agent.AbortTaskDeleted)
					if len(tc.want) == 0 {
						if len(cancelled) != 0 || run.ctx.Err() != nil || s.engine.inflightCount(first.ID) != 0 || s.engine.inflightCount(second.ID) != 0 {
							t.Fatal("taskless custom chat gained hidden ownership")
						}
					} else if len(cancelled) != 1 || cancelled[0] != run || run.ctx.Err() == nil {
						t.Fatal("trusted owner did not cancel exactly its own run")
					}
					if unrelated.ctx.Err() != nil || s.engine.inflightCount(other.ID) != 1 {
						t.Fatal("owner cancellation affected an unrelated run")
					}
					s.finishConversationRun(run)
					s.finishConversationRun(unrelated)
					for _, owner := range []string{first.ID, second.ID, other.ID} {
						if s.engine.inflightCount(owner) != 0 {
							t.Fatal("finished lifecycle-only run retained ownership", owner)
						}
					}
				})
			}
		})
	}
}
