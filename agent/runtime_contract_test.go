package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
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
		opts.Hooks.PostToolUse(ctx, "set_finding_verification", input, []byte(`verification_missing_fields: additional_evidence`), true)
	}
	if !errors.Is(toolLoopCause(ctx), ErrToolLoopGuard) || t.Context().Err() != nil {
		t.Fatal("rewriting note bypassed repeated failure limit")
	}
	if blocked, _, _ := opts.Hooks.PreToolUse(ctx, "traffic_get", []byte(`{"id":"fixture"}`)); !blocked {
		t.Fatal("tool ran after terminal guard stop")
	}
}
