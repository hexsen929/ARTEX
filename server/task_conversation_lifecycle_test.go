package server

import (
	"context"
	"encoding/json"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Autumn-27/artex/agent"
	"github.com/Autumn-27/artex/db"
	"github.com/Autumn-27/artex/intercept"
	"github.com/Autumn-27/norma/llm"
	actool "github.com/Autumn-27/norma/tool"
)

func taskConversationPendingRetest(t *testing.T, s *Server, findingID int64) (*db.FindingRetest, *db.Conversation, *Task) {
	t.Helper()
	finding, err := s.m.pg.GetFinding(findingID)
	if err != nil || finding == nil || finding.TaskID == nil {
		t.Fatal("synthetic retest parent missing")
	}
	parent, ok := s.m.Task(strconv.FormatInt(*finding.TaskID, 10))
	if !ok || parent == nil {
		t.Fatal("synthetic retest runtime missing")
	}
	r, c, created, err := s.m.pg.CreateFindingRetest(t.Context(), findingID, "Synthetic lifecycle observation; no target interaction")
	if err != nil || !created || r == nil || c == nil {
		t.Fatalf("create synthetic retest: created=%v err=%v", created, err)
	}
	return r, c, parent
}

func taskConversationPrepare(t *testing.T, s *Server, c *db.Conversation, owners []int64) *taskConversationRun {
	t.Helper()
	s.concMu.Lock()
	run, err := s.prepareConversationRunLocked(c, s.convBusyKey(c.ID), owners)
	s.concMu.Unlock()
	if err != nil {
		t.Fatalf("prepare synthetic conversation: %v", err)
	}
	t.Cleanup(func() { s.finishConversationRun(run) })
	return run
}

func taskConversationAwait(t *testing.T, done <-chan struct{}, label string) {
	t.Helper()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatalf("timed out waiting for %s", label)
	}
}

func taskConversationBlockedRetest(t *testing.T) (*Server, *Task, *taskConversationRun, <-chan struct{}, func()) {
	t.Helper()
	s, findingID := newRetestServer(t)
	_, c, parent := taskConversationPendingRetest(t, s, findingID)
	entered, cancelled, release := make(chan struct{}), make(chan struct{}), make(chan struct{})
	var enteredOnce, cancelledOnce, releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	setRetestProvider(s, retestProvider{complete: func(ctx context.Context, _ llm.CompletionRequest) (llm.Message, string, llm.Usage, error) {
		enteredOnce.Do(func() { close(entered) })
		<-ctx.Done()
		cancelledOnce.Do(func() { close(cancelled) })
		<-release // Cancellation is not proof that the provider has actually exited.
		return llm.Message{}, "", llm.Usage{}, ctx.Err()
	}})
	t.Cleanup(func() {
		unblock()
		s.stopTaskConversationRuns()
		waitRetestIdle(t, s)
	})
	if err := s.runConversation(c, "Synthetic lifecycle wait", s.convBusyKey(c.ID)); err != nil {
		t.Fatal(err)
	}
	taskConversationAwait(t, entered, "synthetic provider entry")
	value, ok := s.taskConversationRuns.Load(c.ID)
	if !ok {
		t.Fatal("run was not registered before provider entry")
	}
	return s, parent, value.(*taskConversationRun), cancelled, unblock
}

func TestTaskConversationRetestCarriesTaskContextAndPrivateProxy(t *testing.T) {
	s, findingID := newRetestServer(t)
	_, c, parent := taskConversationPendingRetest(t, s, findingID)
	id, _ := strconv.ParseInt(parent.ID, 10, 64)
	s.m.taskProxy = newTaskProxyManager(t.TempDir(), verifierContextRoutes{id: {socksRec(90, db.TunnelAlive, 22009)}}, nil, 22100, 22110)
	const probeName = "TaskConversationContextFixtureProbe"
	type observation struct {
		info         agent.RunInfo
		conversation int64
		env          []string
	}
	observed := make(chan observation, 1)
	agent.ToolAugment = func(context.Context, string) ([]actool.CoreTool, agent.DeferredInfo, func()) {
		probe := actool.Build(actool.Spec{Name: probeName, Schema: map[string]any{"type": "object"},
			ReadOnly: func(json.RawMessage) bool { return true },
			Run: func(ctx context.Context, _ json.RawMessage, tc *actool.ToolContext) (actool.Result, error) {
				observed <- observation{agent.RunInfoFrom(ctx), intercept.ConvIDFromContext(ctx), append([]string(nil), tc.Env...)}
				return actool.Text("Synthetic context observed; no I/O"), nil
			}})
		return []actool.CoreTool{probe}, agent.DeferredInfo{}, func() {}
	}
	var turns atomic.Int32
	setRetestProvider(s, retestProvider{complete: func(context.Context, llm.CompletionRequest) (llm.Message, string, llm.Usage, error) {
		if turns.Add(1) == 1 {
			return llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{{Type: llm.BlockToolUse, ID: "context-probe", Name: probeName, Input: json.RawMessage(`{}`)}}}, "tool_use", llm.Usage{}, nil
		}
		return llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{llm.TextBlock("Synthetic observation finished")}}, "end_turn", llm.Usage{}, nil
	}})
	s.chatAgent.SetProxy("http://stale-chat.invalid:8788", "stale-ca")
	if err := s.runConversation(c, "Observe synthetic context only", s.convBusyKey(c.ID)); err != nil {
		t.Fatal(err)
	}
	select {
	case got := <-observed:
		if got.info.TaskID != id || got.info.ExplorationID != parent.Store.ID() || got.conversation != c.ID {
			t.Fatalf("retest got missing/foreign context: %+v", got.info)
		}
		joined := strings.Join(got.env, "\n")
		if !strings.Contains(joined, "socks5://127.0.0.1:22009") || strings.Contains(joined, "stale-chat.invalid") {
			t.Fatal("retest did not inherit its private target-traffic route")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("context probe was not called")
	}
	waitRetestIdle(t, s)
	if s.engine.inflightCount(parent.ID) != 0 {
		t.Fatal("completed conversation retained its task inflight lease")
	}
}

func TestTaskConversationDeleteWaitsForActualProviderExit(t *testing.T) {
	s, parent, run, cancelled, unblock := taskConversationBlockedRetest(t)
	if !s.beginTaskDelete(parent.ID) {
		t.Fatal("delete barrier was not installed")
	}
	taskConversationAwait(t, cancelled, "provider cancellation")
	if s.engine.inflightCount(parent.ID) == 0 {
		t.Fatal("cancellation prematurely released task ownership")
	}
	drained := make(chan error, 1)
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	go func() { drained <- s.waitTaskQuiescent(ctx, parent.ID) }()
	select {
	case err := <-drained:
		t.Fatalf("delete drained before provider exit: %v", err)
	default:
	}
	unblock()
	taskConversationAwait(t, run.done, "registered run exit")
	select {
	case err := <-drained:
		if err != nil {
			t.Fatal(err)
		}
	case <-ctx.Done():
		t.Fatal("delete did not drain after provider exit")
	}
	if s.engine.inflightCount(parent.ID) != 0 {
		t.Fatal("delete retained finished conversation ownership")
	}
}

func TestTaskConversationCancellationDoesNotStopAnotherTask(t *testing.T) {
	s := &Server{chatBusy: map[string]bool{}, chatCancel: map[string]context.CancelCauseFunc{}}
	first, cancelFirst := context.WithCancelCause(t.Context())
	defer cancelFirst(context.Canceled)
	second, cancelSecond := context.WithCancelCause(t.Context())
	defer cancelSecond(context.Canceled)
	a := &taskConversationRun{ctx: first, cancel: cancelFirst, conversationID: 1, owners: []string{"101"}, done: make(chan struct{})}
	b := &taskConversationRun{ctx: second, cancel: cancelSecond, conversationID: 2, owners: []string{"202"}, done: make(chan struct{})}
	s.taskConversationRuns.Store(a.conversationID, a)
	s.taskConversationRuns.Store(b.conversationID, b)
	if runs := s.cancelTaskConversationRuns("101", agent.AbortTaskDeleted); len(runs) != 1 || runs[0] != a {
		t.Fatal("task cancellation selected unrelated runs")
	}
	if first.Err() == nil || second.Err() != nil {
		t.Fatal("task cancellation crossed task ownership")
	}
}

func TestTaskConversationOldCleanupCannotEraseReplacementOwner(t *testing.T) {
	s := &Server{chatBusy: map[string]bool{"same": true}, chatCancel: map[string]context.CancelCauseFunc{}}
	oldCtx, oldCancel := context.WithCancelCause(t.Context())
	newCtx, newCancel := context.WithCancelCause(t.Context())
	var oldReleased, newReleased atomic.Int32
	old := &taskConversationRun{ctx: oldCtx, cancel: oldCancel, conversationID: 4, busyKey: "same", owners: []string{"101"}, done: make(chan struct{}), release: func() { oldReleased.Add(1) }}
	current := &taskConversationRun{ctx: newCtx, cancel: newCancel, conversationID: 4, busyKey: "same", owners: []string{"202"}, done: make(chan struct{}), release: func() { newReleased.Add(1) }}
	s.taskConversationRuns.Store(int64(4), current)
	s.chatCancel["same"] = newCancel
	s.findingCheckTasks.Store(int64(4), "202")
	s.finishConversationRun(old)
	s.finishConversationRun(old)
	value, ok := s.taskConversationRuns.Load(int64(4))
	owner, owned := s.findingCheckTasks.Load(int64(4))
	if !ok || value != current || !owned || owner != "202" || !s.chatBusy["same"] || s.chatCancel["same"] == nil || newCtx.Err() != nil || oldReleased.Load() != 1 {
		t.Fatal("late old cleanup erased a replacement's lifecycle state")
	}
	s.finishConversationRun(current)
	if newReleased.Load() != 1 || s.chatBusy["same"] {
		t.Fatal("replacement did not release exactly once")
	}
}

func taskConversationAggregate(t *testing.T, s *Server, first *Task) (*taskConversationRun, *Task) {
	t.Helper()
	row, err := s.m.pg.CreateTask("second synthetic aggregate owner", "Fixture metadata only", nil, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	second := &Task{ID: strconv.FormatInt(row.ID, 10), ExpID: row.ExplorationID, Store: s.m.pg.Exploration(row.ExplorationID), Status: "running"}
	s.m.tasks[second.ID] = second
	c, err := s.m.pg.CreateConversation("reporter", "Synthetic aggregate lifecycle", nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = s.m.pg.Exec(`DELETE FROM conversations WHERE id=$1`, c.ID)
		_, _ = s.m.pg.Exec(`DELETE FROM tasks WHERE id=$1`, row.ID)
		_, _ = s.m.pg.Exec(`DELETE FROM explorations WHERE id=$1`, row.ExplorationID)
	})
	firstID, _ := strconv.ParseInt(first.ID, 10, 64)
	return taskConversationPrepare(t, s, c, []int64{firstID, row.ID, firstID}), second
}

func TestTaskConversationAggregateOwnersPauseAndDelete(t *testing.T) {
	for _, action := range []string{"pause", "delete"} {
		t.Run(action, func(t *testing.T) {
			s, findingID := newRetestServer(t)
			finding, err := s.m.pg.GetFinding(findingID)
			if err != nil || finding == nil || finding.TaskID == nil {
				t.Fatal("missing fixture owner")
			}
			first, _ := s.m.Task(strconv.FormatInt(*finding.TaskID, 10))
			run, second := taskConversationAggregate(t, s, first)
			if len(run.owners) != 2 || agent.RunInfoFrom(run.ctx).TaskID != 0 || s.engine.inflightCount(first.ID) != 1 || s.engine.inflightCount(second.ID) != 1 {
				t.Fatal("aggregate owners were lost or one task impersonated the batch")
			}
			if action == "delete" {
				if !s.beginTaskDelete(second.ID) {
					t.Fatal("second owner delete failed")
				}
			} else {
				paused := make(chan error, 1)
				go func() { _, err := s.applyTaskControl(second, "pause"); paused <- err }()
				taskConversationAwait(t, run.ctx.Done(), "aggregate owner pause cancellation")
				select {
				case err := <-paused:
					t.Fatalf("pause returned before run exit: %v", err)
				default:
				}
				s.finishConversationRun(run)
				select {
				case err := <-paused:
					if err != nil {
						t.Fatal(err)
					}
				case <-time.After(3 * time.Second):
					t.Fatal("pause did not finish after release")
				}
			}
			if run.ctx.Err() == nil {
				t.Fatal("aggregate was not cancelled by one of its registered owners")
			}
			s.finishConversationRun(run)
			if s.engine.inflightCount(first.ID) != 0 || s.engine.inflightCount(second.ID) != 0 {
				t.Fatal("aggregate inflight leases were not all released")
			}
		})
	}
}

func TestTaskConversationCloseWaitsForActualProviderExit(t *testing.T) {
	s, parent, run, cancelled, unblock := taskConversationBlockedRetest(t)
	closed := make(chan struct{})
	go func() { s.Close(); close(closed) }()
	taskConversationAwait(t, cancelled, "shutdown cancellation")
	if s.engine.inflightCount(parent.ID) == 0 {
		t.Fatal("Close discarded an active provider's inflight lease")
	}
	select {
	case <-closed:
		t.Fatal("Close returned before provider exit")
	default:
	}
	unblock()
	taskConversationAwait(t, run.done, "provider exit")
	taskConversationAwait(t, closed, "Close completion")
}

func TestTaskConversationMissingRuntimeFailsExecutionButHistoryIsExplanationOnly(t *testing.T) {
	for _, historical := range []bool{false, true} {
		t.Run(strconv.FormatBool(historical), func(t *testing.T) {
			s, findingID := newRetestServer(t)
			r, c, parent := taskConversationPendingRetest(t, s, findingID)
			if historical {
				if err := s.m.pg.FinishFindingRetest(r.ID, "stopped", "Synthetic sealed history"); err != nil {
					t.Fatal(err)
				}
			}
			before, err := s.m.pg.FindingRetestForConversation(t.Context(), c.ID)
			if err != nil {
				t.Fatal(err)
			}
			delete(s.m.tasks, parent.ID)
			var called, augmented atomic.Int32
			agent.ToolAugment = func(context.Context, string) ([]actool.CoreTool, agent.DeferredInfo, func()) {
				augmented.Add(1)
				return nil, agent.DeferredInfo{}, func() {}
			}
			setRetestProvider(s, retestProvider{complete: func(ctx context.Context, req llm.CompletionRequest) (llm.Message, string, llm.Usage, error) {
				called.Add(1)
				if !agent.ExplanationOnly(ctx) || len(req.Tools) != 0 {
					t.Error("historical explanation received execution capabilities")
				}
				return llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{llm.TextBlock("Synthetic saved history explanation only")}}, "end_turn", llm.Usage{}, nil
			}})
			err = s.runConversation(c, "Explain synthetic saved history", s.convBusyKey(c.ID))
			if !historical {
				if err == nil || called.Load() != 0 {
					t.Fatal("missing pending source was executed")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			waitRetestIdle(t, s)
			after, err := s.m.pg.FindingRetestForConversation(t.Context(), c.ID)
			if err != nil {
				t.Fatal(err)
			}
			if called.Load() != 1 || augmented.Load() != 0 || !reflect.DeepEqual(before, after) {
				t.Fatal("history explanation replayed execution or modified sealed history")
			}
		})
	}
}
