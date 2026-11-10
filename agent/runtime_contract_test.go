package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/Autumn-27/artex/db"
	"github.com/Autumn-27/norma/agentcore"
	"github.com/Autumn-27/norma/harness"
	"github.com/Autumn-27/norma/llm"
	"github.com/Autumn-27/norma/permission"
	actool "github.com/Autumn-27/norma/tool"
)

func TestRuntimeContractPlannerFinalRequestAllowsWaiting(t *testing.T) {
	d := testDB(t)
	defer d.Close()
	task, err := d.CreateTask("planner contract fixture", "synthetic local task", nil, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer d.DeleteTask(task.ID)
	es := d.Exploration(task.ExplorationID)
	id, err := es.AddIntent(map[string]any{"summary": "fixture"}, 5, nil, "test")
	if err != nil {
		t.Fatal(err)
	}
	if ok, err := es.ClaimIntent(id, "fixture"); err != nil || !ok {
		t.Fatal("fixture intent was not running", ok, err)
	}
	if err = es.SetIntentOutcome(id, "blocked", "authentication", "fixture provider unavailable"); err != nil {
		t.Fatal(err)
	}
	oldAugment, oldResolve, oldRecall := ToolAugment, ToolResolve, MemoryRecall
	ToolAugment, ToolResolve, MemoryRecall = nil, nil, nil
	t.Cleanup(func() { ToolAugment, ToolResolve, MemoryRecall = oldAugment, oldResolve, oldRecall })
	var requests []llm.CompletionRequest
	provider := memoryFixtureProvider{complete: func(_ context.Context, req llm.CompletionRequest) (llm.Message, string, llm.Usage, error) {
		requests = append(requests, req)
		return llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{llm.TextBlock("等待人工处理，未派发。")}}, "end_turn", llm.Usage{}, nil
	}}
	planner := NewPlanner(provider, "fixture", t.TempDir(), nil, 32000, 2)
	planner.SetNonStreaming(func() bool { return true })
	if _, _, err = planner.Plan(t.Context(), task.ID, nil, es, "synthetic goal", nil, nil); err != nil {
		t.Fatal(err)
	}
	if len(requests) != 1 {
		t.Fatalf("unexpected model turns: %d", len(requests))
	}
	var userText string
	for _, m := range requests[0].Messages {
		if m.Role == llm.RoleUser {
			userText += m.Text()
		}
	}
	combined := strings.Join(requests[0].System, "\n") + userText
	for _, forbidden := range []string{"不得产出 0 意图", "产出 0 意图=任务停摆", "本轮就必须产出至少一个", "asset_ids 务必传"} {
		if strings.Contains(combined, forbidden) {
			t.Fatalf("legacy directive escaped into real request: %s", forbidden)
		}
	}
	if !strings.Contains(userText, "允许0意图") || !strings.Contains(userText, "blocked") {
		t.Fatal("final request lost waiting policy or actual blocked state")
	}
	final := plannerRoundInput(false, false, true, "fixture", "")
	if !strings.Contains(final, "不创建或续接意图") || strings.Contains(final, "只在授权范围") {
		t.Fatal("final settlement can still dispatch", final)
	}
}

func TestRuntimeContractMetadataPreservesCustomSchema(t *testing.T) {
	base := NewToolSet(nil, "planner").addIntent()
	raw, _ := json.Marshal(base.InputSchema())
	var schema map[string]any
	json.Unmarshal(raw, &schema)
	props := schema["properties"].(map[string]any)
	props["priority"].(map[string]any)["default"] = float64(7)
	props["intents"].(map[string]any)["description"] = "CUSTOM_NOTE: summary/asset_ids/parent_ids/priority"
	before, _ := json.Marshal(schema)
	updated := RuntimeToolContract(DecorateTool(base, "CUSTOM_DESCRIPTION", schema))
	if !strings.Contains(updated.Description(), intentCallContract) || !strings.Contains(updated.Description(), "CUSTOM_DESCRIPTION") {
		t.Fatal("authoritative contract or customization missing")
	}
	after, _ := json.Marshal(schema)
	if string(before) != string(after) {
		t.Fatal("persisted schema was mutated")
	}
	up := updated.InputSchema()["properties"].(map[string]any)
	if !reflect.DeepEqual(up["priority"], props["priority"]) || !strings.Contains(up["intents"].(map[string]any)["description"].(string), "continuation_note") {
		t.Fatal("defaults changed or batch contract remained stale")
	}
}

func TestRuntimeContractVerificationExplainsMissingEvidence(t *testing.T) {
	d := testDB(t)
	defer d.Close()
	task, err := d.CreateTask("verification contract", "fixture", nil, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer d.DeleteTask(task.ID)
	es := d.Exploration(task.ExplorationID)
	f, err := es.RecordFinding(t.Context(), db.RecordFindingInput{TaskID: task.ID, Summary: "fixture lead", Severity: "low"})
	if err != nil {
		t.Fatal(err)
	}
	ts := NewToolSet(es, "worker")
	ts.SetTaskID(task.ID)
	input := map[string]any{"finding_id": f.FindingID, "status": "confirmed", "note": strings.Repeat("basis ", 100), "version": 0}
	raw, _ := json.Marshal(input)
	result, err := ts.verifyFinding().Call(t.Context(), raw, nil)
	var failure struct {
		Error   string   `json:"error"`
		Missing []string `json:"missing_fields"`
		Retry   bool     `json:"retryable_without_changes"`
	}
	if err != nil || !result.IsError || json.Unmarshal([]byte(result.Flatten()), &failure) != nil || failure.Error != "verification_missing_fields" || !reflect.DeepEqual(failure.Missing, []string{"additional_evidence"}) || failure.Retry {
		t.Fatal("missing evidence was not actionable", result.Flatten(), err)
	}
	nodes, err := es.ListByKind(db.KindFinding, 10)
	if err != nil {
		t.Fatal(err)
	}
	if err = es.PopulateFindingTrafficIDs(nodes); err != nil {
		t.Fatal(err)
	}
	if compactFinding(nodes[0])["has_text_evidence"] != false {
		t.Fatal("list falsely advertised evidence")
	}
	stored, _ := d.GetFinding(f.FindingID)
	if stored.VerificationStatus != db.VerificationUnverified || stored.EvidenceVersion != 0 {
		t.Fatal("failed validation changed the finding")
	}
	input["additional_evidence"] = "recorded synthetic observation"
	raw, _ = json.Marshal(input)
	result, err = ts.verifyFinding().Call(t.Context(), raw, nil)
	if err != nil || result.IsError {
		t.Fatal("corrected real evidence rejected", result.Flatten(), err)
	}
	stored, _ = d.GetFinding(f.FindingID)
	if stored.VerificationStatus != db.VerificationConfirmed || !strings.Contains(stored.Evidence, "recorded synthetic observation") {
		t.Fatal("accepted evidence was not preserved")
	}
}

func TestRuntimeContractToolLoopStopsUniqueSuccessfulPages(t *testing.T) {
	for _, filtered := range []bool{false, true} {
		t.Run(fmt.Sprint(filtered), func(t *testing.T) {
			toolCalls, modelCalls := 0, 0
			probe := actool.Build(actool.Spec{Name: "traffic_search", Description: "synthetic read only index", Schema: map[string]any{"type": "object", "properties": map[string]any{}}, ReadOnly: func(json.RawMessage) bool { return true }, Run: func(context.Context, json.RawMessage, *actool.ToolContext) (actool.Result, error) {
				toolCalls++
				return actool.Text(fmt.Sprintf(`{"items":[{"id":"fixture-%d"}],"has_more":true}`, toolCalls)), nil
			}})
			provider := memoryFixtureProvider{complete: func(ctx context.Context, _ llm.CompletionRequest) (llm.Message, string, llm.Usage, error) {
				modelCalls++
				if modelCalls > 50 {
					return llm.Message{}, "", llm.Usage{}, errors.New("fixture exceeded limit")
				}
				args := map[string]any{"host": "fixture.invalid", "page": modelCalls - 1, "limit": 10}
				if filtered {
					args["contains"] = "fixture"
				}
				raw, _ := json.Marshal(args)
				return llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{{Type: llm.BlockToolUse, ID: fmt.Sprint(modelCalls), Name: "traffic_search", Input: raw}}}, "tool_use", llm.Usage{}, nil
			}}
			var events []db.Activity
			_, reason, err := captureRun(t.Context(), agentcore.Options{Provider: provider, Tools: []actool.CoreTool{probe}, NonStreaming: true, PermissionMode: permission.ModeBypass}, "read synthetic index", func(a db.Activity) { events = append(events, a) })
			want := broadTrafficCallLimit
			if filtered {
				want = trafficCallLimit
			}
			if !errors.Is(err, ErrToolLoopGuard) || reason != harness.ReasonHookStopped || toolCalls != want || modelCalls > want+1 || t.Context().Err() != nil {
				t.Fatalf("loop not bounded: calls=%d models=%d reason=%s err=%v", toolCalls, modelCalls, reason, err)
			}
			found := false
			for _, e := range events {
				if e.Kind == "result" && e.IsError && strings.Contains(e.Detail, "tool_loop_guard") {
					found = true
				}
			}
			if !found {
				t.Fatal("guard stop did not leave an explicit failed/unfinished result")
			}
		})
	}
}

func TestRuntimeContractToolLoopStopsRepeatedInvalidEdits(t *testing.T) {
	opts := agentcore.Options{}
	ctx, release := attachToolLoopGuard(t.Context(), &opts)
	defer release()
	for i := 0; i < 3; i++ {
		input := []byte(fmt.Sprintf(`{"finding_id":7,"note":"changed prose %d"}`, i))
		observeToolLoopResult(ctx, "set_finding_verification", input, []byte(`verification_missing_fields: additional_evidence`), true)
	}
	if !errors.Is(toolLoopCause(ctx), ErrToolLoopGuard) || t.Context().Err() != nil {
		t.Fatal("rewriting note bypassed repeated failure limit")
	}
	if blocked, _, _ := opts.Hooks.PreToolUse(ctx, "traffic_get", []byte(`{"id":"fixture"}`)); !blocked {
		t.Fatal("tool ran after terminal guard stop")
	}
}

func TestRuntimeContractSDKValidationFailureStopsBeforeHandler(t *testing.T) {
	calls, executed := 0, 0
	probe := actool.Build(actool.Spec{Name: "set_finding_verification", Description: "synthetic validation", Schema: map[string]any{"type": "object", "properties": map[string]any{"version": map[string]any{"type": "integer"}}, "required": []any{"version"}}, Run: func(context.Context, json.RawMessage, *actool.ToolContext) (actool.Result, error) {
		executed++
		return actool.Text("must not run"), nil
	}})
	if err := actool.ValidateInput(probe.InputSchema(), json.RawMessage(`{"finding_id":7,"note":"missing version"}`)); err == nil {
		t.Fatal("fixture did not activate SDK schema validation")
	}
	provider := memoryFixtureProvider{complete: func(context.Context, llm.CompletionRequest) (llm.Message, string, llm.Usage, error) {
		calls++
		if calls > 10 {
			return llm.Message{}, "", llm.Usage{}, errors.New("fixture limit exceeded")
		}
		return llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{{Type: llm.BlockToolUse, ID: fmt.Sprint(calls), Name: probe.Name(), Input: json.RawMessage(`{"finding_id":7,"note":"missing version"}`)}}}, "tool_use", llm.Usage{}, nil
	}}
	_, reason, err := captureRun(t.Context(), agentcore.Options{Provider: provider, Tools: []actool.CoreTool{probe}, NonStreaming: true, PermissionMode: permission.ModeBypass}, "fixture", nil)
	if !errors.Is(err, ErrToolLoopGuard) || reason != harness.ReasonHookStopped || calls != 3 || executed != 0 {
		t.Fatalf("pre-hook schema failures escaped: calls=%d ran=%d reason=%s err=%v", calls, executed, reason, err)
	}
}

func TestRuntimeContractAllChatEntrypointsEnforceReadBudget(t *testing.T) {
	oldAugment, oldResolve, oldRecall := ToolAugment, ToolResolve, MemoryRecall
	t.Cleanup(func() { ToolAugment, ToolResolve, MemoryRecall = oldAugment, oldResolve, oldRecall })
	ToolResolve, MemoryRecall = nil, nil
	for _, role := range []string{"auto", "mainagent"} {
		t.Run(role, func(t *testing.T) {
			calls, executed := 0, 0
			probe := actool.Build(actool.Spec{Name: "traffic_search", Description: "synthetic index", Schema: map[string]any{"type": "object"}, ReadOnly: func(json.RawMessage) bool { return true }, Run: func(context.Context, json.RawMessage, *actool.ToolContext) (actool.Result, error) {
				executed++
				return actool.Text(fmt.Sprint(executed)), nil
			}})
			ToolAugment = func(context.Context, string) ([]actool.CoreTool, DeferredInfo, func()) {
				return []actool.CoreTool{probe}, DeferredInfo{}, func() {}
			}
			provider := memoryFixtureProvider{complete: func(context.Context, llm.CompletionRequest) (llm.Message, string, llm.Usage, error) {
				calls++
				if calls > 20 {
					return llm.Message{}, "", llm.Usage{}, errors.New("fixture exceeded limit")
				}
				return llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{{Type: llm.BlockToolUse, ID: fmt.Sprint(calls), Name: probe.Name(), Input: json.RawMessage(fmt.Sprintf(`{"host":"fixture.invalid","page":%d}`, calls))}}}, "tool_use", llm.Usage{}, nil
			}}
			var err error
			if role == "auto" {
				chat := NewChatAgent(provider, "fixture", t.TempDir(), nil, 32000)
				chat.SetNonStreaming(func() bool { return true })
				_, err = chat.Chat(t.Context(), role, "contract-fixture", "read index", 0, 0, false, nil)
			} else {
				d := testDB(t)
				defer d.Close()
				task, e := d.CreateTask("main contract", "fixture", nil, 0, 0)
				if e != nil {
					t.Fatal(e)
				}
				defer d.DeleteTask(task.ID)
				main := NewMainAgent(provider, "fixture", t.TempDir(), nil, 32000, 0)
				main.SetNonStreaming(func() bool { return true })
				_, err = main.Chat(t.Context(), task.ID, 0, nil, d.Exploration(task.ExplorationID), "fixture", "read index", nil, nil, nil, nil, nil)
			}
			if !errors.Is(err, ErrToolLoopGuard) || executed != broadTrafficCallLimit || calls > broadTrafficCallLimit+1 || t.Context().Err() != nil {
				t.Fatalf("chat escaped read budget: executed=%d calls=%d err=%v", executed, calls, err)
			}
		})
	}
}

func TestRuntimeContractParallelAdmissionAndParentCancellation(t *testing.T) {
	opts := agentcore.Options{}
	ctx, release := attachToolLoopGuard(t.Context(), &opts)
	defer release()
	var admitted atomic.Int64
	var wg sync.WaitGroup
	for i := 0; i < 80; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if blocked, _, _ := opts.Hooks.PreToolUse(ctx, "traffic_search", []byte(`{"host":"fixture.invalid"}`)); !blocked {
				admitted.Add(1)
			}
		}()
	}
	wg.Wait()
	if admitted.Load() != broadTrafficCallLimit || !errors.Is(toolLoopCause(ctx), ErrToolLoopGuard) || t.Context().Err() != nil {
		t.Fatal("parallel budget race", admitted.Load(), context.Cause(ctx))
	}
	parent, cancel := context.WithCancelCause(t.Context())
	other := agentcore.Options{}
	child, done := attachToolLoopGuard(parent, &other)
	defer done()
	cancel(AbortPausedByUser)
	if blocked, _, _ := other.Hooks.PreToolUse(child, "traffic_search", nil); !blocked || context.Cause(child) != AbortPausedByUser || toolLoopCause(child) != nil {
		t.Fatal("parent pause was overwritten")
	}
}
