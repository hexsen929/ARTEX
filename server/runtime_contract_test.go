package server

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/Autumn-27/artex/agent"
	"github.com/Autumn-27/norma/harness"
	actool "github.com/Autumn-27/norma/tool"
)

func TestRuntimeContractPersistedDescriptionsAreUpdatedAtAssembly(t *testing.T) {
	f := newMemoryHTTPFixture(t)
	oldResolve := agent.ToolResolve
	t.Cleanup(func() { agent.ToolResolve = oldResolve })
	wireTools(f.pg, nil)
	old, err := f.pg.GetTool("add_intent")
	if err != nil || old == nil {
		t.Fatal(old, err)
	}
	t.Cleanup(func() {
		agents, _ := json.Marshal(old.Agents)
		f.pg.UpdateTool(old.Key, old.Description, old.Schema, agents, old.Enabled)
	})
	legacy := json.RawMessage(`{"type":"object","properties":{"summary":{"type":"string"},"intents":{"type":"array","description":"CUSTOM legacy summary/asset_ids/parent_ids/priority","items":{"type":"object","properties":{}}}}}`)
	if err = f.pg.UpdateTool("add_intent", "CUSTOM persisted description", legacy, json.RawMessage(`["planner"]`), true); err != nil {
		t.Fatal(err)
	}
	probe := actool.Build(actool.Spec{Name: "add_intent", Description: "code default", Schema: map[string]any{"type": "object"}, Run: func(context.Context, json.RawMessage, *actool.ToolContext) (actool.Result, error) {
		return actool.Text("fixture"), nil
	}})
	resolved := names(agent.ToolResolve(t.Context(), "planner", []actool.CoreTool{probe}))
	if resolved["add_intent"] == nil || !strings.Contains(resolved["add_intent"].Description(), "continuation_note") || !strings.Contains(resolved["add_intent"].Description(), "CUSTOM persisted description") {
		t.Fatal("real resolver still exposes legacy-only contract")
	}
	stored, _ := f.pg.GetTool("add_intent")
	var got, want any
	json.Unmarshal(stored.Schema, &got)
	json.Unmarshal(legacy, &want)
	if stored.Description != "CUSTOM persisted description" || !reflect.DeepEqual(got, want) {
		t.Fatal("runtime decoration rewrote stored customization")
	}
	props := resolved["add_intent"].InputSchema()["properties"].(map[string]any)
	if props["continuation_note"] == nil || props["check_identity"] == nil {
		t.Fatal("stale tool schema omitted current fields")
	}
	if err = f.pg.UpdateTool("add_intent", stored.Description, stored.Schema, json.RawMessage(`["planner"]`), false); err != nil {
		t.Fatal(err)
	}
	if got := names(agent.ToolResolve(t.Context(), "planner", []actool.CoreTool{probe})); got["add_intent"] != nil {
		t.Fatal("contract overlay resurrected disabled tool")
	}
}

func TestRuntimeContractLoopStopIsBlockedAndNotRetried(t *testing.T) {
	state, code, _ := workerOutcome(harness.ReasonHookStopped, agent.ErrToolLoopGuard)
	if state != "blocked" || code != "tool_loop_guard" || retryableWorkerModelError(harness.ReasonHookStopped, agent.ErrToolLoopGuard) {
		t.Fatal(state, code)
	}
}

func TestReadBudgetOutcomeIsAutomaticHandoff(t *testing.T) {
	state, code, message := workerOutcome(harness.ReasonMaxTurns, agent.ErrTrafficReadBudget)
	if state != "exhausted" || code != "traffic_read_budget" || !strings.Contains(message, "Planner") || strings.Contains(message, "等待人工") || retryableWorkerModelError(harness.ReasonMaxTurns, agent.ErrTrafficReadBudget) {
		t.Fatal(state, code, message)
	}
}
