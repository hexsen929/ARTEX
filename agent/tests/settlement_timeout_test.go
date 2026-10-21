//go:build ignore

package agent

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/Autumn-27/artex/db"
	"github.com/Autumn-27/norma/agentcore"
	"github.com/Autumn-27/norma/harness"
	"github.com/Autumn-27/norma/llm"
	"github.com/Autumn-27/norma/permission"
	"github.com/Autumn-27/norma/tool"
)

func TestWorkerCheckpointBudgetResolution(t *testing.T) {
	w := &Worker{}
	if w.settlementTimeout() != DefaultWorkerSettlementTimeout {
		t.Fatal("default checkpoint budget")
	}
	budget := 75 * time.Second
	w.SetSettlementTimeoutResolver(func() time.Duration { return budget })
	if w.settlementTimeout() != budget {
		t.Fatal("override ignored")
	}
	budget = 90 * time.Second
	if w.settlementTimeout() != budget {
		t.Fatal("updated budget not resolved per run")
	}
	for _, invalid := range []time.Duration{0, -time.Second, MaxWorkerSettlementTimeout + time.Second} {
		budget = invalid
		if w.settlementTimeout() != DefaultWorkerSettlementTimeout {
			t.Fatal("invalid budget accepted")
		}
	}
	cfg := workerCheckpointSettlement(false, false, 75*time.Second)
	if cfg.MaxDuration != 75*time.Second || cfg.AllowedTools == nil {
		t.Fatal("checkpoint limit or execution policy lost")
	}
}

func TestCheckpointTimeoutIsNotClaimedAsSaved(t *testing.T) {
	for _, reason := range []harness.TerminalReason{harness.ReasonTimeout, harness.ReasonMaxTurns, harness.ReasonSettlementRequested} {
		summary, detail := terminalText(context.Background(), &harness.Terminal{Reason: reason, Err: context.DeadlineExceeded, Text: "partial checkpoint output"}, &runTrace{})
		if !strings.Contains(summary, "收尾阶段超时") || !strings.Contains(summary, "收尾未确认完成") || strings.Contains(summary, "已收尾写回事实") {
			t.Fatal(summary)
		}
		if !strings.Contains(detail, "context deadline exceeded") || !strings.Contains(detail, "partial checkpoint output") {
			t.Fatal("underlying error/partial output lost")
		}
	}
}

func TestCaptureRunPreservesCheckpointTimeoutWithPartialOutput(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		calls := 0
		p := completionReviewProvider{stream: func(ctx context.Context, req llm.CompletionRequest, yield func(llm.StreamEvent, error) bool) {
			calls++
			if calls == 1 {
				completionReviewEvents(yield, llm.StreamEvent{Type: llm.SEToolUseStart, ToolID: "probe", ToolName: "fixture_probe"}, llm.StreamEvent{Type: llm.SEToolInputJSON, Text: `{}`}, llm.StreamEvent{Type: llm.SEMessageDelta, StopReason: "tool_use"})
				return
			}
			if !yield(llm.StreamEvent{Type: llm.SETextDelta, Text: "partial checkpoint"}, nil) {
				return
			}
			<-ctx.Done()
			yield(llm.StreamEvent{}, ctx.Err())
		}}
		probe := tool.Build(tool.Spec{Name: "fixture_probe", Run: func(ctx context.Context, _ json.RawMessage, _ *tool.ToolContext) (tool.Result, error) {
			<-ctx.Done()
			return tool.Errorf("cancelled"), ctx.Err()
		}})
		var results []db.Activity
		_, reason, err := captureRun(t.Context(), agentcore.Options{Provider: p, Tools: []tool.CoreTool{probe}, MaxDuration: time.Second, Settlement: &harness.Settlement{Prompt: "save only", AllowedTools: []string{}, MaxDuration: 2 * time.Second}, PermissionMode: permission.ModeBypass}, "fixture", func(a db.Activity) {
			if a.Kind == "result" {
				results = append(results, a)
			}
		})
		if reason != harness.ReasonTimeout || !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("reason=%s err=%v", reason, err)
		}
		if len(results) != 1 || !results[0].IsError || !strings.Contains(results[0].Summary, "收尾阶段超时") {
			t.Fatalf("result=%+v", results)
		}
	})
}
