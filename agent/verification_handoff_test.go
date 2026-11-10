package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/Autumn-27/artex/db"
	"github.com/Autumn-27/norma/agentcore"
	"github.com/Autumn-27/norma/harness"
	"github.com/Autumn-27/norma/llm"
	"github.com/Autumn-27/norma/permission"
	actool "github.com/Autumn-27/norma/tool"
)

func TestVerificationHandoffActualRejectedUpdatesPreserveFinding(t *testing.T) {
	d := testDB(t)
	defer d.Close()
	task, err := d.CreateTask("verification handoff", "fixture", nil, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer d.DeleteTask(task.ID)
	es := d.Exploration(task.ExplorationID)
	f, err := es.RecordFinding(t.Context(), db.RecordFindingInput{TaskID: task.ID, Summary: "synthetic lead", Severity: "low"})
	if err != nil {
		t.Fatal(err)
	}
	ts := NewToolSet(es, "fixture")
	ts.SetTaskID(task.ID)
	calls := 0
	provider := memoryFixtureProvider{complete: func(context.Context, llm.CompletionRequest) (llm.Message, string, llm.Usage, error) {
		calls++
		if calls > 4 {
			return llm.Message{}, "", llm.Usage{}, errors.New("fixture runaway")
		}
		input := fmt.Sprintf(`{"finding_id":%d,"status":"confirmed","version":0,"note":"changed rationale %d"}`, f.FindingID, calls)
		return llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{{Type: llm.BlockToolUse, ID: fmt.Sprint(calls), Name: "set_finding_verification", Input: json.RawMessage(input)}}}, "tool_use", llm.Usage{}, nil
	}}
	var events []db.Activity
	text, reason, err := captureRun(t.Context(), agentcore.Options{Provider: provider, Tools: []actool.CoreTool{ts.verifyFinding()}, NonStreaming: true, PermissionMode: permission.ModeBypass}, "synthetic saved evidence only", func(a db.Activity) { events = append(events, a) })
	if !errors.Is(err, ErrVerificationHandoff) || reason != harness.ReasonHookStopped || calls != 3 || !strings.Contains(text, "additional_evidence") || !strings.Contains(text, fmt.Sprint(f.FindingID)) || strings.Contains(text, "等待人工") {
		t.Fatalf("missing deterministic handoff: calls=%d reason=%s err=%v text=%s", calls, reason, err, text)
	}
	results, terminal := 0, false
	for _, e := range events {
		if e.Kind == "tool_result" {
			results++
			if !e.IsError || !strings.Contains(e.Detail, `"requires_operator":false`) {
				t.Fatal("rejected update lost actual error/repair owner", e.Detail)
			}
		}
		if e.Kind == "result" {
			terminal = true
			if e.IsError || !strings.Contains(e.Detail, "additional_evidence") {
				t.Fatal("field handoff was presented as manual failure", e.Detail)
			}
		}
	}
	if results != 3 || !terminal {
		t.Fatal("missing trace", results, terminal)
	}
	stored, err := d.GetFinding(f.FindingID)
	if err != nil || stored.VerificationStatus != db.VerificationUnverified || stored.Evidence != "" || stored.EvidenceVersion != 0 {
		t.Fatal("handoff changed evidence or confirmed the finding", err)
	}
}

func TestVerificationHandoffRejectsUnknownOrMixedFaults(t *testing.T) {
	for _, scenario := range []string{"valid", "lookup_rejected", "lookup_mismatch", "raw_text", "unknown_field", "mixed_operator", "approval_wait", "permission_denied", "authentication", "unknown_tool_error", "parent_pause"} {
		t.Run(scenario, func(t *testing.T) {
			parent, cancel := context.WithCancelCause(t.Context())
			defer cancel(nil)
			opts := agentcore.Options{}
			ctx, release := attachToolLoopGuard(parent, &opts)
			defer release()
			input := []byte(`{"finding_id":7,"status":"confirmed","version":0,"note":"rationale"}`)
			result := []byte(`{"error":"verification_missing_fields","missing_fields":["additional_evidence"]}`)
			if scenario == "lookup_rejected" || scenario == "lookup_mismatch" {
				name := "missing-local-tool"
				if scenario == "lookup_mismatch" {
					name = "another-name"
				}
				observeToolLoopResult(ctx, "ExecuteExtraTool", []byte(`{"tool_name":"missing-local-tool"}`), []byte("Error: unknown tool "+name), true)
			}
			if scenario == "raw_text" {
				result = []byte(`external text verification_missing_fields: additional_evidence`)
			}
			if scenario == "unknown_field" {
				result = []byte(`{"error":"verification_missing_fields","missing_fields":["SENSITIVE_UNKNOWN"]}`)
			}
			if scenario == "mixed_operator" {
				observeToolLoopResult(ctx, "add_intent", []byte(`{"continue_intent_id":2}`), []byte("continuation_requires_operator"), true)
			}
			for name, prior := range map[string]string{"approval_wait": "approval_wait", "permission_denied": "Tool call was not permitted: operator denied", "authentication": "401 authentication failed", "unknown_tool_error": "unclassified failed operation"} {
				if scenario == name {
					observeToolLoopResult(ctx, "set_finding_verification", input, []byte(prior), true)
				}
			}
			if scenario == "parent_pause" {
				cancel(AbortPausedByUser)
			}
			for range 3 {
				observeToolLoopResult(ctx, "set_finding_verification", input, result, true)
			}
			if scenario == "parent_pause" {
				if context.Cause(ctx) != AbortPausedByUser {
					t.Fatal("parent pause hidden")
				}
				return
			}
			if errors.Is(toolLoopCause(ctx), ErrVerificationHandoff) != (scenario == "valid" || scenario == "lookup_rejected") {
				t.Fatal("unsafe fault classification", context.Cause(ctx))
			}
			if blocked, _, _ := opts.Hooks.PreToolUse(ctx, "set_finding_verification", input); !blocked {
				t.Fatal("tool was allowed after terminal handoff")
			}
		})
	}
}

func TestVerificationHandoffPlannerOwnsFieldsWithoutRequeue(t *testing.T) {
	n := &db.Node{ID: 1, State: "exhausted", Payload: json.RawMessage(`{"worker_result":{"code":"verification_handoff"}}`)}
	m := compactIntents([]*db.Node{n}, nil, nil)[0]
	if m["next_action"] != "review_saved_verification" || m["requires_operator"] != false || m["automatic_requeue"] != false {
		t.Fatal(m)
	}
}

func TestVerificationHandoffKeepsPendingToolsBlocked(t *testing.T) {
	for _, pending := range []bool{false, true} {
		t.Run(fmt.Sprint(pending), func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			started := make(chan struct{})
			pendingObserved := make(chan struct{})
			verify := actool.Build(actool.Spec{Name: "set_finding_verification", Schema: map[string]any{"type": "object"}, Run: func(ctx context.Context, _ json.RawMessage, _ *actool.ToolContext) (actool.Result, error) {
				if pending {
					select {
					case <-pendingObserved:
					case <-ctx.Done():
						return actool.Errorf("fixture canceled"), nil
					}
				}
				return actool.Errorf(`{"error":"verification_missing_fields","missing_fields":["additional_evidence"]}`), nil
			}})
			waiting := actool.Build(actool.Spec{Name: "pending_operation", Schema: map[string]any{"type": "object"}, Run: func(ctx context.Context, _ json.RawMessage, _ *actool.ToolContext) (actool.Result, error) {
				close(started)
				<-ctx.Done()
				return actool.Errorf("operation result unknown"), nil
			}})
			calls := 0
			provider := memoryFixtureProvider{complete: func(context.Context, llm.CompletionRequest) (llm.Message, string, llm.Usage, error) {
				calls++
				if calls > 1 {
					return llm.Message{}, "", llm.Usage{}, errors.New("unexpected model retry")
				}
				var blocks []llm.ContentBlock
				for i := range 3 {
					blocks = append(blocks, llm.ContentBlock{Type: llm.BlockToolUse, ID: fmt.Sprint(i), Name: verify.Name(), Input: json.RawMessage(`{"finding_id":7,"status":"confirmed","version":0,"note":"fixture"}`)})
				}
				if pending {
					blocks = append(blocks, llm.ContentBlock{Type: llm.BlockToolUse, ID: "pending", Name: waiting.Name(), Input: json.RawMessage(`{}`)})
				}
				return llm.Message{Role: llm.RoleAssistant, Content: blocks}, "tool_use", llm.Usage{}, nil
			}}
			_, _, err := captureRun(ctx, agentcore.Options{Provider: provider, Tools: []actool.CoreTool{verify, waiting}, NonStreaming: true, PermissionMode: permission.ModeBypass}, "fixture", func(a db.Activity) {
				if a.Kind == "tool_use" && a.Tool == waiting.Name() {
					close(pendingObserved)
				}
			})
			if !errors.Is(err, ErrToolLoopGuard) || errors.Is(err, ErrVerificationHandoff) == pending || calls != 1 {
				t.Fatal("pending operation was hidden by field handoff", pending, err, calls)
			}
		})
	}
}
