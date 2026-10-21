//go:build ignore

package agent

import (
	"context"
	"errors"
	"testing"
	"testing/synctest"
	"time"

	"github.com/Autumn-27/artex/db"
	"github.com/Autumn-27/norma/llm"
)

func TestReviewCompletionDefaultBudgetExceeds45Seconds(t *testing.T) {
	provider := completionReviewProvider{stream: func(ctx context.Context, _ llm.CompletionRequest, yield func(llm.StreamEvent, error) bool) {
		deadline, ok := ctx.Deadline()
		if !ok || time.Until(deadline) < 170*time.Second {
			t.Errorf("review still constrained to old budget: %v", time.Until(deadline))
		}
		yield(llm.StreamEvent{Type: llm.SETextDelta, Text: "no verdict"}, nil)
	}}
	NewPlanner(provider, "", "", nil, 0, 0).ReviewCompletion(context.Background(), 1, new(db.DB).Exploration(7), completionReviewFixture(), nil)
}

func TestReviewCompletionAcceptsVerdictAfter45Seconds(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		calls := 0
		provider := completionReviewProvider{stream: func(ctx context.Context, _ llm.CompletionRequest, yield func(llm.StreamEvent, error) bool) {
			calls++
			if calls == 1 {
				time.Sleep(46 * time.Second)
				completionReviewEvents(yield,
					llm.StreamEvent{Type: llm.SEToolUseStart, ToolID: "verdict", ToolName: "completion_verdict"},
					llm.StreamEvent{Type: llm.SEToolInputJSON, Text: `{"complete":true,"reason":"saved evidence checked"}`},
					llm.StreamEvent{Type: llm.SEMessageDelta, StopReason: "tool_use"})
				return
			}
			completionReviewEvents(yield, llm.StreamEvent{Type: llm.SETextDelta, Text: "finished"}, llm.StreamEvent{Type: llm.SEMessageDelta, StopReason: "end_turn"})
		}}
		accepted, _, err := NewPlanner(provider, "", "", nil, 0, 0).ReviewCompletion(t.Context(), 1, new(db.DB).Exploration(7), completionReviewFixture(), nil)
		if err != nil || !accepted || calls != 2 {
			t.Fatalf("slow valid review rejected: accepted=%v calls=%d err=%v", accepted, calls, err)
		}
	})
}

func TestReviewCompletionCustomBudgetAndParentCancellation(t *testing.T) {
	provider := completionReviewProvider{stream: func(ctx context.Context, _ llm.CompletionRequest, yield func(llm.StreamEvent, error) bool) {
		deadline, _ := ctx.Deadline()
		if time.Until(deadline) > time.Second {
			t.Error("custom or parent budget ignored")
		}
		<-ctx.Done()
		yield(llm.StreamEvent{}, ctx.Err())
	}}
	for _, parent := range []bool{false, true} {
		planner := NewPlanner(provider, "", "", nil, 0, 0)
		planner.SetReviewOptions(func() ReviewOptions { return ReviewOptions{Duration: 80 * time.Millisecond, MaxTurns: 2} })
		ctx := t.Context()
		if parent {
			var cancel context.CancelFunc
			ctx, cancel = context.WithTimeout(ctx, 20*time.Millisecond)
			defer cancel()
		}
		accepted, _, err := planner.ReviewCompletion(ctx, 1, new(db.DB).Exploration(7), completionReviewFixture(), nil)
		if accepted || !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("budget exhausted but review accepted=%v err=%v", accepted, err)
		}
	}
}

func TestReviewCompletionHonorsTurnBudget(t *testing.T) {
	calls := 0
	provider := completionReviewProvider{stream: func(ctx context.Context, _ llm.CompletionRequest, yield func(llm.StreamEvent, error) bool) {
		calls++
		completionReviewEvents(yield,
			llm.StreamEvent{Type: llm.SEToolUseStart, ToolID: "unsupported", ToolName: "not_allowed"},
			llm.StreamEvent{Type: llm.SEToolInputJSON, Text: `{}`},
			llm.StreamEvent{Type: llm.SEMessageDelta, StopReason: "tool_use"})
	}}
	planner := NewPlanner(provider, "", "", nil, 0, 0)
	planner.SetReviewOptions(func() ReviewOptions { return ReviewOptions{Duration: time.Second, MaxTurns: 1} })
	accepted, _, _ := planner.ReviewCompletion(t.Context(), 1, new(db.DB).Exploration(7), completionReviewFixture(), nil)
	if accepted || calls != 1 {
		t.Fatalf("turn limit ignored: calls=%d accepted=%v", calls, accepted)
	}
}
