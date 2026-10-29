package server

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"slices"
	"testing"

	"github.com/Autumn-27/artex/agent"
	"github.com/Autumn-27/artex/db"
	actool "github.com/Autumn-27/norma/tool"
)

func TestAssetMapperBoundaryCapabilitiesMatchRuntimeCeiling(t *testing.T) {
	c := bindingCapabilities("asset_mapper")
	if c.ToolBindingMode != "limited" || c.MCPBindingMode != "unsupported" || c.SkillBindingMode != "unsupported" || c.RuntimeConfigMode != "llm_only" {
		t.Fatalf("UI capability metadata advertises execution for asset mapper: %+v", c)
	}
	if len(c.AllowedToolKeys) != 2 {
		t.Fatalf("UI tool ceiling is not the two catalog reads: %+v", c.AllowedToolKeys)
	}
	for _, name := range []string{"list_assets", "list_companies", "Bash", "Read", "Write", "Edit", "WebFetch", "Browser", "Skill", "mcp__fixture__inspect", "shell_open", "upsert_asset", "external_fixture"} {
		want := name == "list_assets" || name == "list_companies"
		if agent.IsAssetMapperToolAllowed(name) != want || toolBindingSupported("asset_mapper", name) != want || slices.Contains(c.AllowedToolKeys, name) != want {
			t.Fatalf("UI and runtime disagree about mapper tool %s", name)
		}
	}
	raw, err := json.Marshal(c)
	if err != nil {
		t.Fatal(err)
	}
	var wire agentBindingCapabilities
	if err := json.Unmarshal(raw, &wire); err != nil || wire.ToolBindingMode != "limited" || len(wire.AllowedToolKeys) != 2 {
		t.Fatalf("capability wire projection lost the ceiling: %+v %v", wire, err)
	}
	// The new bounded role must not alter ordinary custom agents.
	if !toolBindingSupported("custom_mapper_fixture", "Bash") {
		t.Fatal("asset mapper restriction leaked into custom direct-binding roles")
	}
}

func TestAssetMapperBoundaryBindingsAndLegacyResourcesStayClosed(t *testing.T) {
	pg := openIntranetTestDB(t)
	oldAugment, oldResolve := agent.ToolAugment, agent.ToolResolve
	oldTraffic, oldIntelligence, oldRecord := agent.FindingTrafficBindingEnabled, agent.IntelligenceEnabled, agent.IntelligenceRecord
	t.Cleanup(func() {
		agent.ToolAugment, agent.ToolResolve = oldAugment, oldResolve
		agent.FindingTrafficBindingEnabled, agent.IntelligenceEnabled, agent.IntelligenceRecord = oldTraffic, oldIntelligence, oldRecord
	})
	wireTools(pg, nil) // new bounded role is initialized atomically with this catalog
	a, err := pg.GetAgentByKey("asset_mapper")
	if err != nil || a == nil {
		t.Fatal("asset mapper fixture unavailable", err)
	}
	oldSkills, err := pg.AgentSkillNames(a.ID)
	if err != nil {
		t.Fatal(err)
	}
	oldMCP, err := pg.AgentVisible(a.ID, "mcp")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = pg.SetAgentSkillVisibility(a.ID, oldSkills)
		_ = pg.SetAgentVisibilityKind(a.ID, "mcp", oldMCP)
		_, _ = pg.Exec(`UPDATE agents SET interactive_shell=$2 WHERE id=$1`, a.ID, a.InteractiveShell)
	})
	// Malformed legacy grants must not be consulted merely because they exist.
	mcp, err := pg.SaveMCP(&db.MCPServer{Name: "mapper-no-start-fixture", Transport: "stdio", Command: "/fixture/must-not-start", Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = pg.DeleteMCP(mcp) })
	if err := pg.SetAgentVisibilityKind(a.ID, "mcp", []int64{mcp}); err != nil {
		t.Fatal(err)
	}
	if err := pg.SetAgentSkillVisibility(a.ID, []string{"must-not-read-missing-fixture"}); err != nil {
		t.Fatal(err)
	}
	if _, err := pg.Exec(`UPDATE agents SET interactive_shell=true WHERE id=$1`, a.ID); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"Bash", "external_fixture", "upsert_asset", "report_finding"} {
		if err := validateNewToolBindings(pg, name, nil, []string{"asset_mapper"}); !errors.Is(err, errUnsupportedBinding) {
			t.Fatalf("new unsupported mapper binding %s was accepted: %v", name, err)
		}
	}
	if err := validateNewToolBindings(pg, "list_assets", nil, []string{"asset_mapper"}); err != nil {
		t.Fatal("supported read binding was rejected", err)
	}
	newSkills, newMCP := []string{"additional-mapper-fixture"}, []int64{mcp + 1}
	if err := validateResourceBindingPatch(pg, a, nil, &newSkills); !errors.Is(err, errUnsupportedBinding) {
		t.Fatal("new mapper Skill grant was accepted", err)
	}
	if err := validateResourceBindingPatch(pg, a, &newMCP, nil); !errors.Is(err, errUnsupportedBinding) {
		t.Fatal("new mapper MCP grant was accepted", err)
	}
	hostCalls := 0
	wireAgentAugment(pg, t.TempDir(), func() ([]actool.CoreTool, map[string][]string) {
		hostCalls++
		return []actool.CoreTool{actool.NewBash()}, map[string][]string{"Bash": {"asset_mapper"}}
	})
	read := func(name string) actool.CoreTool {
		return actool.Build(actool.Spec{Name: name, Schema: map[string]any{"type": "object"}, ReadOnly: func(json.RawMessage) bool { return true }})
	}
	wireTools(pg, map[string]actool.CoreTool{"list_assets": read("list_assets"), "list_companies": read("list_companies")})
	for _, key := range []string{"list_assets", "list_companies"} {
		row, err := pg.GetTool(key)
		if err != nil || row == nil {
			t.Fatal("read fixture catalog missing", err)
		}
		saved := *row
		t.Cleanup(func() {
			_ = pg.UpdateTool(saved.Key, saved.Description, saved.Schema, mustJSON(saved.Agents), saved.Enabled)
		})
		bindings := append(append([]string{}, row.Agents...), "asset_mapper")
		if err := pg.UpdateTool(key, row.Description, row.Schema, mustJSON(bindings), true); err != nil {
			t.Fatal(err)
		}
	}
	// Direct callback callers are bounded too, despite existing Skill/MCP rows.
	extra, info, closeFn := agent.ToolAugment(t.Context(), "asset_mapper")
	if closeFn != nil {
		defer closeFn()
	}
	if len(extra) != 0 || info.AssemblyError != nil || hostCalls != 0 || info.Unlock != nil || len(info.SkillDirs) != 0 {
		t.Fatal("mapper initialized legacy execution resources", info.AssemblyError)
	}
	tools, info, cleanup := agent.AugmentTools(t.Context(), "asset_mapper", []actool.CoreTool{})
	cleanup()
	if len(tools) != 2 || info.AssemblyError != nil || hostCalls != 0 {
		t.Fatal("mapper lost its read ceiling or initialized execution resources", info.AssemblyError)
	}
	for _, tool := range tools {
		if !agent.IsAssetMapperToolAllowed(tool.Name()) || !tool.IsReadOnly(nil) {
			t.Fatalf("mapper runtime exceeded its UI capability contract: %s", tool.Name())
		}
	}
	const unsafeKey = "mapper_unsupported_shell_fixture"
	if _, err := pg.Exec(`INSERT INTO tools(key,description,schema,agents,enabled,kind) VALUES($1,'fixture','{}','["asset_mapper"]',true,'shell')`, unsafeKey); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = pg.Exec(`DELETE FROM tools WHERE key=$1`, unsafeKey) })
	tools, info, cleanup = agent.AugmentTools(context.Background(), "asset_mapper", []actool.CoreTool{})
	cleanup()
	if tools == nil || len(tools) != 0 || info.AssemblyError == nil {
		t.Fatal("legacy unsupported declaration silently passed mapper runtime")
	}
}

func TestAssetMapperBoundaryDefaultsUseRealCatalogAndDoNotRegrant(t *testing.T) {
	pg := openIntranetTestDB(t)
	pg.SetMaxOpenConns(1)
	pg.SetMaxIdleConns(1)
	// Shadow only initialization tables in one isolated fixture connection. This
	// checks fresh/legacy paths without deleting other tests' built-in role rows.
	if _, err := pg.Exec(`CREATE TEMP TABLE agents (LIKE public.agents INCLUDING ALL);
CREATE TEMP TABLE agent_prompts (LIKE public.agent_prompts INCLUDING ALL);
CREATE TEMP TABLE tools (LIKE public.tools INCLUDING ALL);
CREATE TEMP TABLE settings (LIKE public.settings INCLUDING ALL)`); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = pg.Exec(`DROP TABLE IF EXISTS pg_temp.agent_prompts,pg_temp.agents,pg_temp.tools,pg_temp.settings`)
	})
	oldAugment, oldResolve := agent.ToolAugment, agent.ToolResolve
	oldTraffic, oldIntelligence, oldRecord := agent.FindingTrafficBindingEnabled, agent.IntelligenceEnabled, agent.IntelligenceRecord
	t.Cleanup(func() {
		agent.ToolAugment, agent.ToolResolve = oldAugment, oldResolve
		agent.FindingTrafficBindingEnabled, agent.IntelligenceEnabled, agent.IntelligenceRecord = oldTraffic, oldIntelligence, oldRecord
	})
	wireTools(pg, nil)
	a, err := pg.GetAgentByKey("asset_mapper")
	if err != nil || a == nil || !a.Builtin || !a.Enabled {
		t.Fatal("new mapper role was not initialized with the catalog", err)
	}
	prompt, err := pg.CurrentPrompt(a.ID)
	if err != nil || prompt != agent.BuiltinPromptSeeds()["asset_mapper"] {
		t.Fatal("new mapper did not receive the actual public default prompt", err)
	}
	for _, seed := range agent.BuiltinToolSeeds() {
		if !agent.IsAssetMapperToolAllowed(seed.Key) {
			continue
		}
		row, err := pg.GetTool(seed.Key)
		if err != nil || row == nil || row.Description != seed.Desc || !slices.Contains(row.Agents, "asset_mapper") {
			t.Fatal("new mapper read metadata is incomplete", err)
		}
		var schema map[string]any
		if err := json.Unmarshal(row.Schema, &schema); err != nil || !reflect.DeepEqual(schema, seed.Schema) {
			// JSON normalizes numeric defaults to float64, so compare marshaled
			// schema values through the same decoder rather than raw map types.
			raw, marshalErr := json.Marshal(seed.Schema)
			var expected map[string]any
			if marshalErr != nil || json.Unmarshal(raw, &expected) != nil || !reflect.DeepEqual(schema, expected) {
				t.Fatalf("mapper tool %s has a placeholder schema", seed.Key)
			}
		}
	}
	// Simulate an existing user-owned role with empty prompt/selection and
	// missing catalog rows. Generic catalog seeds must not restore mapper grants.
	if _, err := pg.Exec(`UPDATE agents SET builtin=false,enabled=false,name='user mapper',current_prompt_id=NULL WHERE key='asset_mapper'`); err != nil {
		t.Fatal(err)
	}
	if _, err := pg.Exec(`DELETE FROM agent_prompts WHERE agent_id=$1`, a.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := pg.Exec(`DELETE FROM tools WHERE key IN ('list_assets','list_companies')`); err != nil {
		t.Fatal(err)
	}
	wireTools(pg, nil)
	seedPrompts(pg)
	got, err := pg.GetAgentByKey("asset_mapper")
	if err != nil || got == nil || got.Builtin || got.Enabled || got.Name != "user mapper" {
		t.Fatal("existing custom/disabled mapper was overwritten", err)
	}
	if prompt, err := pg.CurrentPrompt(a.ID); err != nil || prompt != "" {
		t.Fatal("existing empty mapper prompt was silently filled", err)
	}
	for _, key := range []string{"list_assets", "list_companies"} {
		row, err := pg.GetTool(key)
		if err != nil || row == nil || slices.Contains(row.Agents, "asset_mapper") {
			t.Fatalf("generic catalog restored an existing mapper's empty selection: %s %v", key, err)
		}
	}
}
