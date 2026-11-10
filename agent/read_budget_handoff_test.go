package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"iter"
	"strings"
	"testing"

	"github.com/Autumn-27/artex/db"
	"github.com/Autumn-27/norma/agentcore"
	"github.com/Autumn-27/norma/harness"
	"github.com/Autumn-27/norma/llm"
	"github.com/Autumn-27/norma/permission"
	actool "github.com/Autumn-27/norma/tool"
)

func TestReadBudgetBroadLimitLeavesKnownRecordsAvailable(t *testing.T) {
	opts := agentcore.Options{}
	ctx, release := attachToolLoopGuard(t.Context(), &opts)
	defer release()
	for range broadTrafficCallLimit {
		if blocked, _, _ := opts.Hooks.PreToolUse(ctx, "traffic_search", []byte(`{"host":"fixture.invalid"}`)); blocked {
			t.Fatal("early limit")
		}
	}
	blocked, note, _ := opts.Hooks.PreToolUse(ctx, "traffic_search", []byte(`{"host":"fixture.invalid","page":12}`))
	if !blocked || !strings.Contains(note, "traffic_query_limit") || readBudgetReached(ctx) || ctx.Err() != nil {
		t.Fatal("broad limit became operator stop", note, ctx.Err())
	}
	for _, call := range []struct{ name, input string }{{"traffic_get", `{"id":"saved-fixture"}`}, {"traffic_search", `{"host":"fixture.invalid","contains":"known-record"}`}} {
		if blocked, _, _ := opts.Hooks.PreToolUse(ctx, call.name, []byte(call.input)); blocked {
			t.Fatal("existing record/filtered read blocked", call.name)
		}
	}
	for range 2 {
		opts.Hooks.PreToolUse(ctx, "traffic_search", []byte(`{"host":"another.invalid","page":0}`))
	}
	if !readBudgetReached(ctx) || ctx.Err() != nil {
		t.Fatal("persistent wide reads did not enter live handoff")
	}
}

func TestReadBudgetAutomaticHandoffHasNoToolsAndOneModelCall(t *testing.T) {
	for _, streaming := range []bool{false, true} {
		t.Run(fmt.Sprint(streaming), func(t *testing.T) {
			modelCalls, readCalls, summaryCalls := 0, 0, 0
			probe := actool.Build(actool.Spec{Name: "traffic_search", Description: "saved index", Schema: map[string]any{"type": "object"}, ReadOnly: func(json.RawMessage) bool { return true }, Run: func(context.Context, json.RawMessage, *actool.ToolContext) (actool.Result, error) {
				readCalls++
				return actool.Text(fmt.Sprintf("saved-record-%d", readCalls)), nil
			}})
			provider := memoryFixtureProvider{complete: func(ctx context.Context, req llm.CompletionRequest) (llm.Message, string, llm.Usage, error) {
				modelCalls++
				if len(req.Tools) == 0 {
					summaryCalls++
					if req.MaxTokens > 1200 || req.Thinking != "disabled" || !strings.Contains(strings.Join(req.System, "\n"), readHandoffInstruction) {
						t.Fatal("unbounded/missing summary contract")
					}
					history := ""
					for _, m := range req.Messages {
						for _, b := range m.Content {
							history += b.Text
							for _, c := range b.Content {
								history += c.Text
							}
						}
					}
					if !strings.Contains(history, "saved-record-12") {
						t.Fatal("handoff omitted existing results")
					}
					return llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{llm.TextBlock("已读12条记录，未查完；交接保存记录与停止位置。")}}, "end_turn", llm.Usage{InputTokens: 7, OutputTokens: 11}, nil
				}
				if modelCalls > 20 {
					return llm.Message{}, "", llm.Usage{}, errors.New("fixture runaway")
				}
				return llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{{Type: llm.BlockToolUse, ID: fmt.Sprint(modelCalls), Name: "traffic_search", Input: json.RawMessage(fmt.Sprintf(`{"host":"fixture.invalid","page":%d}`, modelCalls-1))}}}, "tool_use", llm.Usage{}, nil
			}}
			provider.stream = func(ctx context.Context, req llm.CompletionRequest) iter.Seq2[llm.StreamEvent, error] {
				return func(yield func(llm.StreamEvent, error) bool) {
					message, stop, usage, err := provider.Complete(ctx, req)
					if err != nil {
						yield(llm.StreamEvent{}, err)
						return
					}
					if !yield(llm.StreamEvent{Type: llm.SEMessageStart, Usage: usage}, nil) {
						return
					}
					for _, b := range message.Content {
						if b.Type == llm.BlockToolUse {
							if !yield(llm.StreamEvent{Type: llm.SEToolUseStart, ToolID: b.ID, ToolName: b.Name}, nil) {
								return
							}
							if !yield(llm.StreamEvent{Type: llm.SEToolInputJSON, Text: string(b.Input)}, nil) {
								return
							}
						} else if !yield(llm.StreamEvent{Type: llm.SETextDelta, Text: b.Text}, nil) {
							return
						}
					}
					if !yield(llm.StreamEvent{Type: llm.SEMessageDelta, StopReason: stop}, nil) {
						return
					}
					yield(llm.StreamEvent{Type: llm.SEMessageStop}, nil)
				}
			}
			var activities []db.Activity
			text, reason, err := captureRun(t.Context(), agentcore.Options{Provider: provider, Tools: []actool.CoreTool{probe}, NonStreaming: !streaming, PermissionMode: permission.ModeBypass}, "read saved fixture", func(a db.Activity) { activities = append(activities, a) })
			if !errors.Is(err, ErrTrafficReadBudget) || reason != harness.ReasonMaxTurns || readCalls != 12 || summaryCalls != 1 || modelCalls != 16 || !strings.Contains(text, "交接保存记录") || t.Context().Err() != nil {
				t.Fatalf("bad handoff text=%s reason=%s reads=%d models=%d summaries=%d err=%v", text, reason, readCalls, modelCalls, summaryCalls, err)
			}
			for _, a := range activities {
				if a.Kind == "result" && (a.IsError || strings.Contains(a.Detail, "请人工核对")) {
					t.Fatal("normal read limit was presented as operator failure")
				}
			}
		})
	}
}

func TestReadBudgetSummaryCannotRunToolsOrHideParentPause(t *testing.T) {
	for _, scenario := range []string{"provider_error", "tool_response", "parent_pause"} {
		t.Run(scenario, func(t *testing.T) {
			parent, cancel := context.WithCancelCause(t.Context())
			defer cancel(nil)
			calls := 0
			provider := memoryFixtureProvider{complete: func(context.Context, llm.CompletionRequest) (llm.Message, string, llm.Usage, error) {
				calls++
				if scenario == "parent_pause" {
					cancel(AbortPausedByUser)
					return llm.Message{}, "", llm.Usage{}, context.Canceled
				}
				if scenario == "provider_error" {
					return llm.Message{}, "", llm.Usage{}, errors.New("SENSITIVE_PROVIDER_TEXT")
				}
				return llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{{Type: llm.BlockToolUse, ID: "never-execute", Name: "Bash", Input: json.RawMessage(`{}`)}}}, "tool_use", llm.Usage{}, nil
			}}
			opts := agentcore.Options{Provider: provider}
			ctx, release := attachToolLoopGuard(parent, &opts)
			defer release()
			g := ctx.Value(toolLoopGuardKey{}).(*toolLoopGuard)
			g.mu.Lock()
			g.readBudget = true
			g.mu.Unlock()
			msg, stop, _, err := opts.Deps.CallModelSync(ctx, llm.CompletionRequest{})
			if scenario == "parent_pause" {
				if err == nil || context.Cause(ctx) != AbortPausedByUser {
					t.Fatal("lost parent pause")
				}
				return
			}
			if err != nil || len(msg.ToolUses()) > 0 || stop != "end_turn" || msg.Text() != readHandoffFallback || strings.Contains(msg.Text(), "SENSITIVE") {
				t.Fatal(msg, stop, err)
			}
			opts.Deps.CallModelSync(ctx, llm.CompletionRequest{})
			if calls != 1 {
				t.Fatal("handoff repeated model call", calls)
			}
		})
	}
}

func TestReadBudgetPlannerSummaryDoesNotRequestOperatorOrRequeue(t *testing.T) {
	n := &db.Node{ID: 1, State: "exhausted", Payload: json.RawMessage(`{"worker_result":{"code":"traffic_read_budget"}}`)}
	item := compactIntents([]*db.Node{n}, nil, nil)[0]
	if item["requires_operator"] != false || item["automatic_requeue"] != false || item["next_action"] != "review_saved_results" {
		t.Fatal(item)
	}
}
