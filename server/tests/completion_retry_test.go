//go:build ignore

package server

import (
	"context"
	"errors"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Autumn-27/artex/agent"
	"github.com/Autumn-27/artex/db"
	"github.com/Autumn-27/norma/llm"
)

func TestCompletionEngineBudgetRetryStopsAndPersists(t *testing.T) {
	e, task, _ := completionEngineFixture(t, true, nil, nil)
	calls := new(atomic.Int64)
	provider := retestProvider{complete: func(ctx context.Context, _ llm.CompletionRequest) (llm.Message, string, llm.Usage, error) {
		calls.Add(1)
		<-ctx.Done()
		return llm.Message{}, "", llm.Usage{}, ctx.Err()
	}}
	planner := agent.NewPlanner(provider, "test", "", nil, 0, 8)
	planner.SetNonStreaming(func() bool { return true })
	planner.SetReviewOptions(func() agent.ReviewOptions { return agent.ReviewOptions{Duration: 150 * time.Millisecond, MaxTurns: 2} })
	e.SetAuthoritativeAgentResolver(func(*Task) (*agent.Planner, *agent.Worker) { return planner, nil })
	id, _ := strconv.ParseInt(task.ID, 10, 64)
	if err := e.requestCompletion(t.Context(), task, "first candidate", nil, false); err != nil {
		t.Fatal(err)
	}
	for count := 1; count <= 3; count++ {
		run := waitCompletionRun(t, e, task)
		joinCompletionRun(t, run)
		c, err := e.m.pg.TaskCompletion(t.Context(), id)
		if err != nil || c.FailureCount != count || c.FailureKind != "budget" || c.Phase != "interrupted" || e.m.TaskStatus(task.ID) == "done" {
			t.Fatalf("budget failure not persisted: %+v %v", c, err)
		}
		if calls.Load() != int64(count) {
			t.Fatalf("unexpected model calls: %d", calls.Load())
		}
		if _, ok := e.completionReplan.Load(task.ID); ok {
			t.Fatal("provider timeout restarted ordinary planner")
		}
		if err = e.requestCompletion(t.Context(), task, "planner resubmission", nil, false); err == nil {
			t.Fatal("resubmission bypassed backoff")
		} else {
			var limited *db.CompletionRetryError
			if !errors.As(err, &limited) {
				t.Fatalf("wrong policy error: %v", err)
			}
		}
		e.retryCompletion(t.Context(), task)
		if e.isCompleting(task.ID) {
			t.Fatal("retry fired before due")
		}
		if count < 3 {
			if _, err = e.m.pg.Exec(`UPDATE task_completions SET next_retry_at=now()-interval '1 second' WHERE task_id=$1`, id); err != nil {
				t.Fatal(err)
			}
			e.retryCompletion(t.Context(), task)
		} else if c.NextRetryAt != nil || c.RecoveryAction != "manual_retry" {
			t.Fatalf("automatic review did not stop: %+v", c)
		}
	}
}

func TestCompletionEngineHistoryFailureDoesNotAutoRetry(t *testing.T) {
	e, task, _ := completionEngineFixture(t, true, nil, nil)
	provider := retestProvider{complete: func(context.Context, llm.CompletionRequest) (llm.Message, string, llm.Usage, error) {
		return llm.Message{}, "", llm.Usage{}, &llm.HistoryError{Reason: "missing saved reasoning"}
	}}
	planner := agent.NewPlanner(provider, "test", "", nil, 0, 8)
	planner.SetNonStreaming(func() bool { return true })
	e.SetAuthoritativeAgentResolver(func(*Task) (*agent.Planner, *agent.Worker) { return planner, nil })
	if err := e.requestCompletion(t.Context(), task, "candidate", nil, false); err != nil {
		t.Fatal(err)
	}
	joinCompletionRun(t, waitCompletionRun(t, e, task))
	id, _ := strconv.ParseInt(task.ID, 10, 64)
	c, err := e.m.pg.TaskCompletion(t.Context(), id)
	if err != nil || c.FailureKind != "history" || c.NextRetryAt != nil || c.RecoveryAction != "repair_history" {
		t.Fatalf("history error retried: %+v %v", c, err)
	}
	e.retryCompletion(t.Context(), task)
	if e.isCompleting(task.ID) {
		t.Fatal("deterministic protocol error retried")
	}
}
