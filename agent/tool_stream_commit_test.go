package agent

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"iter"
	"sync/atomic"
	"testing"

	"github.com/Autumn-27/artex/db"
	"github.com/Autumn-27/norma/agentcore"
	"github.com/Autumn-27/norma/harness"
	"github.com/Autumn-27/norma/llm"
	"github.com/Autumn-27/norma/permission"
	"github.com/Autumn-27/norma/tool"
)

type toolCommitFixture struct {
	stream func(context.Context, llm.CompletionRequest) iter.Seq2[llm.StreamEvent, error]
}

func (p toolCommitFixture) Stream(ctx context.Context, r llm.CompletionRequest) iter.Seq2[llm.StreamEvent, error] {
	return p.stream(ctx, r)
}
func (p toolCommitFixture) Complete(context.Context, llm.CompletionRequest) (llm.Message, string, llm.Usage, error) {
	return llm.Message{}, "", llm.Usage{}, errors.New("stream fixture only")
}

func TestAutomaticRecoveryStreamNeverDispatchesFailedTurn(t *testing.T) {
	for _, scenario := range []string{"partial_arguments", "complete_tool_then_eof", "second_tool_partial", "truncated_without_error", "canceled"} {
		t.Run(scenario, func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			var executions atomic.Int64
			probe := tool.Build(tool.Spec{Name: "CommitProbe", Schema: map[string]any{"type": "object"}, Run: func(context.Context, json.RawMessage, *tool.ToolContext) (tool.Result, error) {
				executions.Add(1)
				return tool.Text("executed"), nil
			}})
			provider := toolCommitFixture{stream: func(context.Context, llm.CompletionRequest) iter.Seq2[llm.StreamEvent, error] {
				return func(yield func(llm.StreamEvent, error) bool) {
					events := []llm.StreamEvent{{Type: llm.SEThinkingDelta, Text: "planning"}, {Type: llm.SEToolUseStart, ToolID: "probe", ToolName: "CommitProbe"}, {Type: llm.SEToolInputJSON, Text: `{}`}}
					if scenario == "partial_arguments" {
						events[2].Text = `{"command":`
					}
					if scenario == "complete_tool_then_eof" {
						events = append(events, llm.StreamEvent{Type: llm.SETextDelta, Text: "after tool"})
					}
					if scenario == "second_tool_partial" {
						events = append(events, llm.StreamEvent{Type: llm.SEToolUseStart, ToolID: "probe2", ToolName: "CommitProbe"}, llm.StreamEvent{Type: llm.SEToolInputJSON, Text: `{"x":`})
					}
					for _, e := range events {
						if !yield(e, nil) {
							return
						}
					}
					if scenario == "canceled" {
						cancel()
						yield(llm.StreamEvent{}, context.Canceled)
					} else if scenario != "truncated_without_error" {
						yield(llm.StreamEvent{}, io.ErrUnexpectedEOF)
					}
				}
			}}
			uses := 0
			_, reason, err := captureRun(ctx, agentcore.Options{Provider: provider, Tools: []tool.CoreTool{probe}, PermissionMode: permission.ModeBypass, MaxTurns: 2}, "fixture", func(a db.Activity) {
				if a.Kind == "tool_use" {
					uses++
				}
			})
			if err == nil || reason == harness.ReasonCompleted || executions.Load() != 0 || uses != 0 {
				t.Fatalf("unsafe stream dispatched: executions=%d uses=%d reason=%s error=%v", executions.Load(), uses, reason, err)
			}
		})
	}
}

func TestAutomaticRecoveryStreamCommitsCompleteTurnExactlyOnce(t *testing.T) {
	var executions atomic.Int64
	calls := 0
	probe := tool.Build(tool.Spec{Name: "CommitProbe", Schema: map[string]any{"type": "object"}, Run: func(context.Context, json.RawMessage, *tool.ToolContext) (tool.Result, error) {
		executions.Add(1)
		return tool.Text("executed"), nil
	}})
	provider := toolCommitFixture{stream: func(context.Context, llm.CompletionRequest) iter.Seq2[llm.StreamEvent, error] {
		return func(yield func(llm.StreamEvent, error) bool) {
			calls++
			events := []llm.StreamEvent{{Type: llm.SETextDelta, Text: "done"}, {Type: llm.SEMessageDelta, StopReason: "end_turn"}}
			if calls == 1 {
				events = []llm.StreamEvent{{Type: llm.SEToolUseStart, ToolID: "probe", ToolName: "CommitProbe"}, {Type: llm.SEToolInputJSON, Text: `{}`}, {Type: llm.SEMessageDelta, StopReason: "tool_use"}}
			}
			for _, e := range events {
				if !yield(e, nil) {
					return
				}
			}
		}
	}}
	uses, results := 0, 0
	_, reason, err := captureRun(t.Context(), agentcore.Options{Provider: provider, Tools: []tool.CoreTool{probe}, PermissionMode: permission.ModeBypass, MaxTurns: 3}, "fixture", func(a db.Activity) {
		if a.Kind == "tool_use" {
			uses++
		}
		if a.Kind == "tool_result" {
			results++
		}
	})
	if err != nil || reason != harness.ReasonCompleted || executions.Load() != 1 || uses != 1 || results != 1 || calls != 2 {
		t.Fatalf("completion executions=%d uses=%d results=%d calls=%d reason=%s err=%v", executions.Load(), uses, results, calls, reason, err)
	}
}
