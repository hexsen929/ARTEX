//go:build ignore

package harness

import (
	"context"
	"encoding/json"
	"errors"
	"iter"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Autumn-27/norma/llm"
	"github.com/Autumn-27/norma/permission"
	"github.com/Autumn-27/norma/tool"
)

func settlementTestTool(name string, run func(context.Context)) tool.CoreTool {
	return tool.Build(tool.Spec{
		Name: name, Schema: map[string]any{"type": "object"},
		Concurrent: func(json.RawMessage) bool { return true },
		Permissions: func(context.Context, json.RawMessage, permission.Context) permission.Decision {
			return permission.Allowed()
		},
		Run: func(ctx context.Context, _ json.RawMessage, _ *tool.ToolContext) (tool.Result, error) {
			run(ctx)
			return tool.Text(name + " saved"), nil
		},
	})
}

func TestSettlementRejectsHiddenAndUnlistedToolsAtExecution(t *testing.T) {
	for _, tc := range []struct {
		name       string
		settlement Settlement
	}{
		{"disabled", Settlement{DisabledTools: []string{"probe"}}},
		{"allowlist", Settlement{AllowedTools: []string{"save"}}},
		{"empty_allowlist", Settlement{AllowedTools: []string{}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var probes atomic.Int32
			m := &scriptedModel{turns: [][]llm.StreamEvent{
				toolTurn("before", "probe", `{}`),
				toolTurn("after", "probe", `{}`), // model ignores the hidden schema
				textTurn("checkpoint complete"),
			}}
			s := tc.settlement
			s.Prompt = "checkpoint only"
			term := drain(t, QueryInput{
				Tools:    tool.NewRegistry(settlementTestTool("probe", func(context.Context) { probes.Add(1) })),
				MaxTurns: 1, Settlement: &s,
			}, QueryDeps{CallModel: m.call})
			if probes.Load() != 1 {
				t.Fatalf("probe callbacks = %d; hidden tool ran during settlement", probes.Load())
			}
			if term.Reason != ReasonMaxTurns {
				t.Fatalf("reason = %q", term.Reason)
			}
			var rejected bool
			for _, msg := range term.Messages {
				for _, block := range msg.Content {
					if block.ToolUseID == "after" && block.IsError {
						rejected = true
					}
				}
			}
			if !rejected {
				t.Fatal("missing rejected tool result")
			}
		})
	}
}

func TestSettlementRequestStopsQueuedConcurrentToolsAndSavesResult(t *testing.T) {
	c := NewSettlementController()
	var probes, saves atomic.Int32
	var liveParent atomic.Bool
	probe := settlementTestTool("probe", func(ctx context.Context) {
		probes.Add(1)
		c.Request()
		liveParent.Store(ctx.Err() == nil)
	})
	save := settlementTestTool("save", func(context.Context) { saves.Add(1) })
	m := &scriptedModel{turns: [][]llm.StreamEvent{
		twoToolTurn("probe"), // second goroutine must wait for the one-slot semaphore
		toolTurn("checkpoint", "save", `{}`),
		textTurn("checkpoint complete"),
	}}
	term := drainWithCtx(t, WithSettlementController(context.Background(), c), QueryInput{
		Tools: tool.NewRegistry(probe, save), MaxConcurrency: 1,
		Settlement: &Settlement{Prompt: "budget", RequestPrompt: "host checkpoint", AllowedTools: []string{"save"}},
	}, QueryDeps{CallModel: m.call})
	if probes.Load() != 1 || saves.Load() != 1 || !liveParent.Load() {
		t.Fatalf("probes=%d saves=%d parent live=%v", probes.Load(), saves.Load(), liveParent.Load())
	}
	if term.Reason != ReasonSettlementRequested || term.Err != nil || term.Text != "checkpoint complete" {
		t.Fatalf("terminal = %+v", term)
	}
	var saved, requested bool
	for _, msg := range term.Messages {
		if strings.Contains(msg.Text(), "host checkpoint") {
			requested = true
		}
		for _, block := range msg.Content {
			if block.Type == llm.BlockToolResult && !block.IsError && strings.Contains(block.Content[0].Text, "probe saved") {
				saved = true
			}
		}
	}
	if !saved || !requested {
		t.Fatal("request discarded the active tool result or the checkpoint prompt")
	}
}

type settlementRequestHook struct {
	controller *SettlementController
	once       sync.Once
}

func (h *settlementRequestHook) PreToolUse(context.Context, string, []byte) (bool, string, []byte) {
	h.once.Do(h.controller.Request)
	return false, "", nil
}
func (*settlementRequestHook) PostToolUse(context.Context, string, []byte, []byte, bool) {}
func (*settlementRequestHook) Stop(context.Context, []llm.Message) (bool, []string, string) {
	return false, nil, ""
}

func TestSettlementRequestDuringPreHookRechecksBeforeCallback(t *testing.T) {
	c := NewSettlementController()
	var probes atomic.Int32
	m := &scriptedModel{turns: [][]llm.StreamEvent{toolTurn("probe", "probe", `{}`), textTurn("saved")}}
	term := drainWithCtx(t, WithSettlementController(context.Background(), c), QueryInput{
		Tools:      tool.NewRegistry(settlementTestTool("probe", func(context.Context) { probes.Add(1) })),
		Hooks:      &settlementRequestHook{controller: c},
		Settlement: &Settlement{Prompt: "checkpoint", AllowedTools: []string{}},
	}, QueryDeps{CallModel: m.call})
	if probes.Load() != 0 || term.Reason != ReasonSettlementRequested {
		t.Fatalf("probes=%d terminal=%+v", probes.Load(), term)
	}
}

func TestSettlementRequestAtTextBoundaryPreservesTextAndClosesScope(t *testing.T) {
	c := NewSettlementController()
	var calls int
	var captured context.Context
	call := func(ctx context.Context, req llm.CompletionRequest) iter.Seq2[llm.StreamEvent, error] {
		captured = ctx
		calls++
		text := "existing result"
		if calls == 1 {
			c.Request()
		} else {
			text = "checkpoint complete"
			if len(req.Tools) != 1 || req.Tools[0].Name != "save" {
				t.Errorf("schemas = %+v", req.Tools)
			}
		}
		return func(yield func(llm.StreamEvent, error) bool) {
			for _, event := range textTurn(text) {
				if !yield(event, nil) {
					return
				}
			}
		}
	}
	term := drainWithCtx(t, WithSettlementController(context.Background(), c), QueryInput{
		Tools:      tool.NewRegistry(settlementTestTool("save", func(context.Context) {}), settlementTestTool("probe", func(context.Context) {})),
		Settlement: &Settlement{Prompt: "checkpoint", AllowedTools: []string{"save"}},
	}, QueryDeps{CallModel: call})
	if calls != 2 || term.Reason != ReasonSettlementRequested {
		t.Fatalf("calls=%d reason=%s", calls, term.Reason)
	}
	var found bool
	for _, msg := range term.Messages {
		if msg.Text() == "existing result" {
			found = true
		}
	}
	if !found {
		t.Fatal("lost the final text from the ordinary phase")
	}
	if err := tool.CheckExecution(captured, "save"); err == nil {
		t.Fatal("completed scope still admits tools")
	}
}

func TestSettlementRequestWithoutAllowlistFailsClosed(t *testing.T) {
	c := NewSettlementController()
	c.Request()
	var probes atomic.Int32
	m := &scriptedModel{turns: [][]llm.StreamEvent{toolTurn("probe", "probe", `{}`), textTurn("saved")}}
	drainWithCtx(t, WithSettlementController(context.Background(), c), QueryInput{
		Tools:      tool.NewRegistry(settlementTestTool("probe", func(context.Context) { probes.Add(1) })),
		Settlement: &Settlement{Prompt: "checkpoint"},
	}, QueryDeps{CallModel: m.call})
	if probes.Load() != 0 {
		t.Fatal("external settlement without an allowlist admitted a tool")
	}
}

func TestSettlementRequestWithoutPromptStopsBeforeModel(t *testing.T) {
	c := NewSettlementController()
	c.Request()
	term := drainWithCtx(t, WithSettlementController(context.Background(), c), QueryInput{}, QueryDeps{})
	if term.Reason != ReasonSettlementRequested || term.Err != nil {
		t.Fatalf("terminal=%+v", term)
	}
}

func TestSettlementDeadlineBoundsModelCall(t *testing.T) {
	c := NewSettlementController()
	c.Request()
	parent, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	call := func(ctx context.Context, _ llm.CompletionRequest) iter.Seq2[llm.StreamEvent, error] {
		return func(yield func(llm.StreamEvent, error) bool) {
			<-ctx.Done()
			yield(llm.StreamEvent{}, ctx.Err())
		}
	}
	term := drainWithCtx(t, WithSettlementController(parent, c), QueryInput{
		Settlement: &Settlement{Prompt: "checkpoint", AllowedTools: []string{}, MaxDuration: 20 * time.Millisecond},
	}, QueryDeps{CallModel: call})
	if term.Reason != ReasonSettlementRequested || !errors.Is(term.Err, context.DeadlineExceeded) || parent.Err() != nil {
		t.Fatalf("terminal=%+v parent=%v", term, parent.Err())
	}
}

func TestSettlementTurnLimitIncludesRecoveryCalls(t *testing.T) {
	c := NewSettlementController()
	c.Request()
	m := &scriptedModel{turns: [][]llm.StreamEvent{{
		{Type: llm.SETextDelta, Text: "truncated"}, {Type: llm.SEMessageDelta, StopReason: "max_tokens"},
	}}}
	term := drainWithCtx(t, WithSettlementController(context.Background(), c), QueryInput{
		Settlement: &Settlement{Prompt: "checkpoint", AllowedTools: []string{}, MaxTurns: 1},
	}, QueryDeps{CallModel: m.call})
	if m.calls != 1 || term.Reason != ReasonSettlementRequested {
		t.Fatalf("calls=%d terminal=%+v", m.calls, term)
	}
}

func TestParentCancellationWinsOverSettlementRequest(t *testing.T) {
	c := NewSettlementController()
	c.Request()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	term := drainWithCtx(t, WithSettlementController(ctx, c), QueryInput{
		Settlement: &Settlement{Prompt: "checkpoint", AllowedTools: []string{}},
	}, QueryDeps{})
	if term.Reason != ReasonAbortedStreaming || !errors.Is(term.Err, context.Canceled) {
		t.Fatalf("terminal=%+v", term)
	}
}

func TestSettlementWaitForToolsJoinsActualCallbacksAfterSyntheticResult(t *testing.T) {
	controller := NewSettlementController()
	ctx, cancel := context.WithCancel(WithSettlementController(context.Background(), controller))
	defer cancel()
	entered, release := make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	t.Cleanup(func() { releaseOnce.Do(func() { close(release) }) })
	var probes atomic.Int32
	late := settlementTestTool("late", func(context.Context) { probes.Add(1) })
	blocking := settlementTestTool("blocking", func(ctx context.Context) {
		close(entered)
		<-release // simulate a host callback that does not honor cancellation
		_, _ = tool.Invoke(ctx, late, []byte(`{}`), nil)
	})
	m := &scriptedModel{turns: [][]llm.StreamEvent{twoToolTurn("blocking")}}
	done := make(chan *Terminal, 1)
	go func() {
		for event := range Query(ctx, QueryInput{Tools: tool.NewRegistry(blocking), MaxConcurrency: 1}, QueryDeps{CallModel: m.call}) {
			if event.Kind == KindResult {
				done <- event.Terminal
			}
		}
	}()
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("tool did not start")
	}
	cancel()
	select {
	case term := <-done:
		if term.Reason != ReasonAbortedTools {
			t.Fatalf("terminal=%+v", term)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("query did not publish its synthetic result")
	}
	short, shortCancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer shortCancel()
	if err := controller.WaitForTools(short); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("synthetic result was mistaken for a joined tool: %v", err)
	}
	releaseOnce.Do(func() { close(release) })
	waitCtx, waitCancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer waitCancel()
	if err := controller.WaitForTools(waitCtx); err != nil {
		t.Fatalf("join actual tool: %v", err)
	}
	if probes.Load() != 0 {
		t.Fatal("late callback started another tool after terminal")
	}
}
