//go:build ignore

package server

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Autumn-27/artex/agent"
	"github.com/Autumn-27/artex/db"
	"github.com/Autumn-27/norma/llm"
)

func completionEngineFixture(t *testing.T, verdict bool, started chan struct{}, release <-chan struct{}) (*Engine, *Task, *atomic.Int64) {
	t.Helper()
	m, err := NewManager(t.TempDir(), "")
	if err != nil {
		t.Skipf("postgres unavailable: %v", err)
	}
	task, err := m.CreateTask("completion engine test", "verify necessary work", nil, 0, 0)
	if err != nil {
		m.Close()
		t.Fatal(err)
	}
	calls := new(atomic.Int64)
	provider := retestProvider{complete: func(ctx context.Context, _ llm.CompletionRequest) (llm.Message, string, llm.Usage, error) {
		if calls.Add(1) == 1 {
			if started != nil {
				close(started)
			}
			if release != nil {
				select {
				case <-ctx.Done():
					return llm.Message{}, "", llm.Usage{}, ctx.Err()
				case <-release:
				}
			}
			return llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{{Type: llm.BlockToolUse, ID: "completion-test", Name: "completion_verdict", Input: json.RawMessage(fmt.Sprintf(`{"complete":%t,"reason":"checked saved evidence"}`, verdict))}}}, "tool_use", llm.Usage{}, nil
		}
		return llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{llm.TextBlock("review complete")}}, "end_turn", llm.Usage{}, nil
	}}
	planner := agent.NewPlanner(provider, "test", t.TempDir(), nil, 0, 8)
	planner.SetNonStreaming(func() bool { return true })
	e := NewEngine(m)
	e.SetAuthoritativeAgentResolver(func(*Task) (*agent.Planner, *agent.Worker) { return planner, nil })
	t.Cleanup(func() {
		e.StopTask(task.ID)
		n, _ := strconv.ParseInt(task.ID, 10, 64)
		_ = m.pg.DeleteTask(n)
		m.Close()
	})
	return e, task, calls
}

func waitCompletionRun(t *testing.T, e *Engine, task *Task) *completionRun {
	t.Helper()
	v, ok := e.completing.Load(task.ID)
	if !ok {
		t.Fatal("completion run was not installed")
	}
	return v.(*completionRun)
}

func joinCompletionRun(t *testing.T, run *completionRun) {
	t.Helper()
	select {
	case <-run.done:
	case <-time.After(8 * time.Second):
		t.Fatal("completion did not settle")
	}
}

func TestCompletionEngineWaitsForCheckpointAndClosesClaims(t *testing.T) {
	e, task, calls := completionEngineFixture(t, true, nil, nil)
	id, err := task.Store.AddNode(db.KindIntent, map[string]any{"summary": "irrelevant check"}, 0, "running", "planner", nil)
	if err != nil {
		t.Fatal(err)
	}
	if !e.beginExecutionOperation(task.ID) {
		t.Fatal("execution not admitted")
	}
	_ = e.execContextFor(t.Context(), task.ID)
	if err = e.requestCompletion(t.Context(), task, "goals met", []db.CompletionDisposition{{IntentID: id, Kind: "irrelevant", Reason: "outside the requested goal"}}, false); err != nil {
		e.decInflight(task.ID)
		t.Fatal(err)
	}
	run := waitCompletionRun(t, e, task)
	if e.beginExecutionOperation(task.ID) {
		e.decInflight(task.ID)
		t.Fatal("new execution crossed completion barrier")
	}
	if calls.Load() != 0 {
		t.Fatal("review started before execution exited")
	}
	if _, err = task.Store.AddNode(db.KindFact, map[string]any{"summary": "last checkpoint"}, 0, "confirmed", "worker", nil); err != nil {
		e.decInflight(task.ID)
		t.Fatal(err)
	}
	if err = task.Store.SetIntentState(id, "exhausted"); err != nil {
		e.decInflight(task.ID)
		t.Fatal(err)
	}
	e.decInflight(task.ID)
	joinCompletionRun(t, run)
	if got := e.m.TaskStatus(task.ID); got != "done" {
		t.Fatalf("status=%s", got)
	}
	node, _ := task.Store.GetNode(id)
	if node.State != "stopped" {
		t.Fatalf("waived intent state=%s", node.State)
	}
	facts, _ := task.Store.ListByKind(db.KindFact, 100)
	found := false
	for _, f := range facts {
		var p map[string]any
		_ = json.Unmarshal(f.Payload, &p)
		found = found || p["summary"] == "last checkpoint"
	}
	if !found {
		t.Fatal("worker checkpoint lost")
	}
}

func TestCompletionEngineRejectedReviewRestoresFreshExecution(t *testing.T) {
	started, release := make(chan struct{}), make(chan struct{})
	e, task, _ := completionEngineFixture(t, false, started, release)
	_ = e.execContextFor(t.Context(), task.ID)
	old := e.execSettlement[task.ID]
	if err := e.requestCompletion(t.Context(), task, "candidate", nil, false); err != nil {
		t.Fatal(err)
	}
	run := waitCompletionRun(t, e, task)
	<-started
	close(release)
	joinCompletionRun(t, run)
	if e.m.TaskStatus(task.ID) == "done" {
		t.Fatal("rejected review committed")
	}
	if !old.Requested() {
		t.Fatal("worker controller did not receive stop request")
	}
	if !e.beginExecutionOperation(task.ID) {
		t.Fatal("failed review left execution blocked")
	}
	e.decInflight(task.ID)
	_ = e.execContextFor(t.Context(), task.ID)
	if fresh := e.execSettlement[task.ID]; fresh == old || fresh.Requested() {
		t.Fatal("retry reused a closed controller")
	}
	if _, ok := e.completionReplan.Load(task.ID); !ok {
		t.Fatal("review rejection was not fed back into replanning")
	}
}

func TestCompletionEngineConcurrentGoalAndPauseInvalidateReview(t *testing.T) {
	for _, change := range []string{"goal", "pause"} {
		t.Run(change, func(t *testing.T) {
			started, release := make(chan struct{}), make(chan struct{})
			e, task, _ := completionEngineFixture(t, true, started, release)
			if err := e.requestCompletion(t.Context(), task, "candidate", nil, false); err != nil {
				t.Fatal(err)
			}
			run := waitCompletionRun(t, e, task)
			<-started
			if change == "goal" {
				if _, err := task.Store.AddGoal(map[string]any{"summary": "new necessary goal"}, "user"); err != nil {
					t.Fatal(err)
				}
			} else {
				e.Pause(task.ID, agent.AbortPausedByUser)
				if err := e.m.ApplyTaskPause(task.ID); err != nil {
					t.Fatal(err)
				}
			}
			close(release)
			joinCompletionRun(t, run)
			if e.m.TaskStatus(task.ID) == "done" {
				t.Fatal("concurrent change overwritten")
			}
			if change == "pause" && (!e.IsPaused(task.ID) || !task.lifecycleSnapshot().Paused) {
				t.Fatal("user pause not preserved")
			}
		})
	}
}

func TestCompletionEngineQuarantineRetainsSlotAndPause(t *testing.T) {
	e, task, _ := completionEngineFixture(t, true, nil, nil)
	if !e.beginExecutionOperation(task.ID) {
		t.Fatal("admission failed")
	}
	if err := e.requestCompletion(t.Context(), task, "candidate", nil, false); err != nil {
		e.decInflight(task.ID)
		t.Fatal(err)
	}
	run := waitCompletionRun(t, e, task)
	e.quarantineCompletion(task, run)
	if !e.completionStalled(task.ID) || !task.lifecycleSnapshot().Paused {
		e.decInflight(task.ID)
		t.Fatal("stalled run not quarantined")
	}
	s := &Server{m: e.m, engine: e}
	if !s.occupiesConcurrencySlot(task) {
		e.decInflight(task.ID)
		t.Fatal("late execution released its concurrency slot")
	}
	if _, err := s.admitPausedTask(task); err == nil {
		e.decInflight(task.ID)
		t.Fatal("stalled task resumed")
	}
	e.decInflight(task.ID)
	joinCompletionRun(t, run)
	if e.m.TaskStatus(task.ID) == "done" || !e.IsPaused(task.ID) {
		t.Fatal("quarantine falsely completed or unpaused task")
	}
}
