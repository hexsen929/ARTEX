package agent

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/Autumn-27/artex/db"
)

func TestExecutionPlannerDispatchIsAtomicAndMetadataSurvives(t *testing.T) {
	d := testDB(t)
	t.Cleanup(func() { _ = d.Close() })
	exp, err := d.CreateExploration("planning fixture", "fixture")
	if err != nil {
		t.Fatal(err)
	}
	defer d.Exec(`DELETE FROM explorations WHERE id=$1`, exp)
	s := d.Exploration(exp)
	tools := NewToolSet(s, "planner")
	id, err := tools.addOneIntent(intentItem{Summary: "one fixture question", Acceptance: "observable fixture", Prerequisites: "fixture ready", StopCondition: "fixture blocker", ChainTags: []string{"web"}})
	if err != nil {
		t.Fatal(err)
	}
	again, err := tools.addOneIntent(intentItem{Summary: "one fixture question"})
	if err != nil || again != id {
		t.Fatal("active work was duplicated", id, again, err)
	}
	if _, err := s.ClaimIntent(id, "fixture"); err != nil {
		t.Fatal(err)
	}
	if err := s.SetIntentOutcome(id, "blocked", "approval_wait", "not executed"); err != nil {
		t.Fatal(err)
	}
	node, err := s.GetNode(id)
	if err != nil {
		t.Fatal(err)
	}
	entry := compactIntents([]*db.Node{node}, nil, nil)[0]
	for _, key := range []string{"worker_result", "acceptance", "prerequisites", "stop_condition", "chain_tags", "blocked_reason"} {
		if entry[key] == nil {
			t.Fatal("planner lost field", key)
		}
	}
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			_, _ = s.AddPlannerIntent(map[string]any{"summary": fmt.Sprintf("fixture-%d", n)}, 5, nil, nil, MaxOpenIntents)
		}(i)
	}
	wg.Wait()
	if count, err := s.CountOpenIntents(); err != nil || count != MaxOpenIntents {
		t.Fatal("frontier limit raced", count, err)
	}
	// A long-running intent must survive a newer history window.
	for i := 0; i < 305; i++ {
		other, err := s.AddIntent(map[string]any{"summary": "history fixture"}, 5, nil, "test")
		if err != nil {
			t.Fatal(err)
		}
		if err := s.SetIntentState(other, "done"); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.SetIntentState(id, "running"); err != nil {
		t.Fatal(err)
	}
	overview := tools.graphOverviewData()
	running := overview["running_intents"].([]map[string]any)
	if len(running) != 1 || running[0]["id"] != id {
		t.Fatal("old running intent disappeared", running)
	}
}

func TestExecutionPlanningSchemaMigrationPreservesCustomizations(t *testing.T) {
	d := testDB(t)
	t.Cleanup(func() { _ = d.Close() })
	tool := NewToolSet(nil, "planner").addIntent()
	defaults, _ := json.Marshal(tool.InputSchema())
	old, err := d.GetTool("add_intent")
	if err != nil {
		t.Fatal(err)
	}
	if old == nil {
		if err := d.SeedTool("add_intent", tool.Description(), defaults, json.RawMessage(`["planner"]`)); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _, _ = d.Exec(`DELETE FROM tools WHERE key='add_intent'`) })
		old, err = d.GetTool("add_intent")
		if err != nil || old == nil {
			t.Fatal(old, err)
		}
	}
	t.Cleanup(func() { _, _ = d.Exec(`UPDATE tools SET schema=$1 WHERE key='add_intent'`, string(old.Schema)) })
	legacy := `{"type":"object","properties":{"summary":{"type":"string","description":"CUSTOM_DESCRIPTION"},"intents":{"type":"array","items":{"type":"object"}}}}`
	if _, err := d.Exec(`UPDATE tools SET schema=$1 WHERE key='add_intent'`, legacy); err != nil {
		t.Fatal(err)
	}
	if err := d.MergePlanningToolSchema(defaults); err != nil {
		t.Fatal(err)
	}
	saved, err := d.GetTool("add_intent")
	if err != nil {
		t.Fatal(err)
	}
	var schema map[string]any
	_ = json.Unmarshal(saved.Schema, &schema)
	props := schema["properties"].(map[string]any)
	if props["summary"].(map[string]any)["description"] != "CUSTOM_DESCRIPTION" {
		t.Fatal("custom description reset")
	}
	items := props["intents"].(map[string]any)["items"].(map[string]any)["properties"].(map[string]any)
	for _, key := range []string{"acceptance", "prerequisites", "stop_condition", "chain_tags"} {
		if props[key] == nil || items[key] == nil {
			t.Fatal("missing model-facing field", key)
		}
	}
	if saved.Enabled != old.Enabled || !slices.Equal(saved.Agents, old.Agents) {
		t.Fatal("schema migration reset role policy")
	}
}

func TestExecutionStagnationSignalsPreserveMeaningfulDifferences(t *testing.T) {
	d := NewStuckDetector(testStuckConfig())
	for _, code := range []int{200, 401, 403, 404, 500, 201} {
		if event := d.Observe("Bash", fmt.Sprintf("HTTP/1.1 %d\n%s", code, strings.Repeat("same prefix", 40)), false); event != nil {
			t.Fatal("status differences erased", event)
		}
	}
	d = NewStuckDetector(testStuckConfig())
	for _, tail := range []string{"first evidence", "different evidence", "third evidence", "fourth evidence"} {
		if event := d.Observe("Read", strings.Repeat("generic heading ", 40)+tail, false); event != nil {
			t.Fatal("body differences after 200 chars erased", event)
		}
	}
	text := StuckIntervention(StuckEvent{Tool: "Bash", Kind: "same_signature", Count: 12, N: 1})
	if strings.Contains(text, "标记此方向榨干") {
		t.Fatal("stagnation claimed route disproved")
	}
}

func TestExecutionPlannerReadinessRepairsStoredLegacyBody(t *testing.T) {
	old := PromptOverride
	t.Cleanup(func() { PromptOverride = old })
	PromptOverride = func(key string) (string, bool) {
		if key == "planner" {
			return "CUSTOM_PREFIX\n" + plannerDefaultTmpl, true
		}
		return "", false
	}
	text := plannerSystem("fixture", "/data", "/data")
	if !strings.HasPrefix(text, "CUSTOM_PREFIX") || strings.Contains(text, "产出 0 意图=任务停摆") || strings.Contains(text, "→ 直接重派同方向") {
		t.Fatal("legacy unconditional dispatch survived")
	}
	if !strings.Contains(text, "【规划与执行校准】") || !strings.Contains(workerSystem("", "", "/data", "/data"), "【执行与交接校准】") {
		t.Fatal("current method was not assembled")
	}
}
