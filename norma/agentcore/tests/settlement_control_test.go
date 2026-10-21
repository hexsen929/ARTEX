//go:build ignore

package agentcore

import (
	"context"
	"iter"
	"testing"

	"github.com/Autumn-27/norma/harness"
	"github.com/Autumn-27/norma/llm"
	"github.com/Autumn-27/norma/permission"
	"github.com/Autumn-27/norma/tool"
)

func TestSessionSettlementIsRunScopedAndGuardsDeferredTool(t *testing.T) {
	turns := [][]llm.StreamEvent{
		toolTurn("blocked", tool.ExecuteExtraToolName, `{"tool_name":"echo","params":{"msg":"hi"}}`),
		textTurn("checkpoint complete"),
		toolTurn("normal", tool.ExecuteExtraToolName, `{"tool_name":"echo","params":{"msg":"hi"}}`),
		textTurn("ordinary chat"),
	}
	var calls int
	callModel := func(context.Context, llm.CompletionRequest) iter.Seq2[llm.StreamEvent, error] {
		events := turns[calls]
		calls++
		return func(yield func(llm.StreamEvent, error) bool) {
			for _, event := range events {
				if !yield(event, nil) {
					return
				}
			}
		}
	}
	session := NewSession(Options{
		Tools: []tool.CoreTool{deferredEcho()}, DeferredTools: []string{"echo"},
		PermissionMode: permission.ModeBypass,
		Settlement:     &harness.Settlement{Prompt: "checkpoint", AllowedTools: []string{tool.ExecuteExtraToolName}},
		Deps:           harness.QueryDeps{CallModel: callModel},
	})
	defer session.Close()
	controller := harness.NewSettlementController()
	controller.Request()
	var blocked bool
	var terminal *harness.Terminal
	for event := range session.Prompt(harness.WithSettlementController(context.Background(), controller), "finish") {
		if event.Kind == harness.KindToolResult {
			blocked = event.ToolResult.IsError
		}
		if event.Kind == harness.KindResult {
			terminal = event.Terminal
		}
	}
	if !blocked || terminal == nil || terminal.Reason != harness.ReasonSettlementRequested {
		t.Fatalf("deferred tool not blocked: blocked=%v terminal=%+v", blocked, terminal)
	}
	var succeeded bool
	for event := range session.Prompt(context.Background(), "continue ordinary chat") {
		if event.Kind == harness.KindToolResult {
			succeeded = !event.ToolResult.IsError
		}
		if event.Kind == harness.KindResult {
			terminal = event.Terminal
		}
	}
	if !succeeded || terminal == nil || terminal.Reason != harness.ReasonCompleted {
		t.Fatalf("run restriction leaked into new chat: succeeded=%v terminal=%+v", succeeded, terminal)
	}
}
