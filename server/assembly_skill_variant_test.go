package server

import (
	"context"
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/Autumn-27/artex/agent"
	"github.com/Autumn-27/artex/db"
	actool "github.com/Autumn-27/norma/tool"
)

func variantSkillTool(tools []actool.CoreTool) actool.CoreTool {
	for _, tool := range tools {
		if tool.Name() == "Skill" {
			return tool
		}
	}
	return nil
}

func TestIntranetSkillVisibilityUsesVariantWithoutBroadeningMCP(t *testing.T) {
	dsn, _, err := db.DSN()
	if err != nil {
		t.Skip("no test database configured")
	}
	pg, err := db.Open(dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = pg.Close() })
	worker, err := pg.GetAgentByKey("worker")
	if err != nil || worker == nil {
		t.Fatalf("worker unavailable: %v", err)
	}
	variant, err := pg.GetAgentByKey("worker.intranet")
	if err != nil || variant == nil {
		t.Fatalf("intranet variant unavailable: %v", err)
	}
	oldWorkerSkills, err := pg.AgentSkillNames(worker.ID)
	if err != nil {
		t.Fatal(err)
	}
	oldVariantSkills, err := pg.AgentSkillNames(variant.ID)
	if err != nil {
		t.Fatal(err)
	}
	oldWorkerMCP, err := pg.AgentVisible(worker.ID, "mcp")
	if err != nil {
		t.Fatal(err)
	}
	oldVariantMCP, err := pg.AgentVisible(variant.ID, "mcp")
	if err != nil {
		t.Fatal(err)
	}
	var createdMCP []int64
	t.Cleanup(func() {
		_ = pg.SetAgentSkillVisibility(worker.ID, oldWorkerSkills)
		_ = pg.SetAgentSkillVisibility(variant.ID, oldVariantSkills)
		_ = pg.SetAgentVisibilityKind(worker.ID, "mcp", oldWorkerMCP)
		_ = pg.SetAgentVisibilityKind(variant.ID, "mcp", oldVariantMCP)
		for _, id := range createdMCP {
			_ = pg.DeleteMCP(id)
		}
	})
	makeMCP := func(name string) (int64, string) {
		t.Helper()
		server := &db.MCPServer{Name: name, Transport: "stdio", Command: "/fixture/must-not-start", Enabled: true}
		id, err := pg.SaveMCP(server)
		if err != nil {
			t.Fatal(err)
		}
		createdMCP = append(createdMCP, id)
		toolName := "mcp__" + name + "__fixture"
		if err := pg.SaveMCPTools(id, []db.MCPTool{{Name: toolName, Description: "cached fixture", Schema: json.RawMessage(`{"type":"object"}`)}}); err != nil {
			t.Fatal(err)
		}
		return id, toolName
	}
	directID, directTool := makeMCP("t-intranet-direct")
	_, baseGatedTool := makeMCP("t-intranet-base")
	variantOnlyID, variantOnlyTool := makeMCP("t-intranet-only")
	if err := pg.SetAgentVisibilityKind(worker.ID, "mcp", []int64{directID}); err != nil {
		t.Fatal(err)
	}
	if err := pg.SetAgentVisibilityKind(variant.ID, "mcp", []int64{variantOnlyID}); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	for name, mcps := range map[string]string{
		"outer-skill": "[t-intranet-base]",
		"inner-skill": "[t-intranet-base, t-intranet-only]",
	} {
		path := filepath.Join(dir, name)
		if err := os.MkdirAll(path, 0o755); err != nil {
			t.Fatal(err)
		}
		body := "---\nname: " + name + "\ndescription: fixture\nmcps: " + mcps + "\n---\n" + name + " body"
		if err := os.WriteFile(filepath.Join(path, "SKILL.md"), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := pg.SetAgentSkillVisibility(worker.ID, []string{"outer-skill"}); err != nil {
		t.Fatal(err)
	}
	if err := pg.SetAgentSkillVisibility(variant.ID, []string{"inner-skill"}); err != nil {
		t.Fatal(err)
	}
	oldAugment := agent.ToolAugment
	t.Cleanup(func() { agent.ToolAugment = oldAugment })
	wireAgentAugment(pg, dir, nil)
	ctx := agent.WithRunInfo(t.Context(), agent.RunInfo{TaskID: 81, IntentID: 91})
	ctx = agent.WithSkillVisibilityAgent(ctx, "worker", "worker.intranet")
	tools, def, cleanup := agent.ToolAugment(ctx, "worker")
	if cleanup != nil {
		defer cleanup()
	}
	if def.AssemblyError != nil {
		t.Fatal(def.AssemblyError)
	}
	names := map[string]bool{}
	for _, tool := range tools {
		names[tool.Name()] = true
	}
	if !names[directTool] || !names[baseGatedTool] || names[variantOnlyTool] {
		t.Fatalf("execution MCP authorization changed: %v", names)
	}
	if !def.Unlock.Has(directTool) || def.Unlock.Has(baseGatedTool) || def.Unlock.Has(variantOnlyTool) {
		t.Fatal("MCP unlock state changed before Skill load")
	}
	skill := variantSkillTool(tools)
	if skill == nil {
		t.Fatal("intranet Skill was not assembled")
	}
	result, err := skill.Call(ctx, json.RawMessage(`{"name":"inner-skill"}`), nil)
	if err != nil || result.IsError || !def.Unlock.Has(baseGatedTool) || def.Unlock.Has(variantOnlyTool) {
		t.Fatalf("intranet Skill load/unlock: %+v %v", result, err)
	}
	result, err = skill.Call(ctx, json.RawMessage(`{"name":"outer-skill"}`), nil)
	if err != nil || !result.IsError {
		t.Fatal("intranet run retained an unassigned ordinary-worker Skill")
	}
	// Explicitly empty variant grants must stay empty, and edits only apply to a
	// freshly assembled run. The first run keeps its immutable registry snapshot.
	if err := pg.SetAgentSkillVisibility(variant.ID, nil); err != nil {
		t.Fatal(err)
	}
	newTools, newDef, newCleanup := agent.ToolAugment(ctx, "worker")
	if newCleanup != nil {
		defer newCleanup()
	}
	if newDef.AssemblyError != nil || variantSkillTool(newTools) != nil {
		t.Fatal("empty intranet grants fell back to ordinary-worker Skills")
	}
	result, err = skill.Call(ctx, json.RawMessage(`{"name":"inner-skill"}`), nil)
	if err != nil || result.IsError {
		t.Fatal("new grant edit changed an already assembled run")
	}
	ordinary, ordinaryDef, ordinaryCleanup := agent.ToolAugment(t.Context(), "worker")
	if ordinaryCleanup != nil {
		defer ordinaryCleanup()
	}
	if ordinaryDef.AssemblyError != nil || variantSkillTool(ordinary) == nil {
		t.Fatal("external worker Skill visibility changed")
	}
	result, err = variantSkillTool(ordinary).Call(t.Context(), json.RawMessage(`{"name":"outer-skill"}`), nil)
	if err != nil || result.IsError {
		t.Fatal("external worker lost its own Skill")
	}
	_, missingDef, _ := agent.ToolAugment(agent.WithSkillVisibilityAgent(t.Context(), "worker", "missing.intranet"), "worker")
	if missingDef.AssemblyError == nil {
		t.Fatal("missing Skill role silently fell back to worker")
	}
}

func TestIntranetSkillMetadataReadErrorDoesNotFallback(t *testing.T) {
	closed, err := sql.Open("pgx", "postgres://fixture.invalid/unused")
	if err != nil {
		t.Fatal(err)
	}
	if err := closed.Close(); err != nil {
		t.Fatal(err)
	}
	oldAugment := agent.ToolAugment
	t.Cleanup(func() { agent.ToolAugment = oldAugment })
	wireAgentAugment(&db.DB{DB: closed}, t.TempDir(), nil)
	ctx := agent.WithSkillVisibilityAgent(t.Context(), "worker", "worker.intranet")
	tools, def, cleanup := agent.ToolAugment(ctx, "worker")
	if cleanup != nil {
		cleanup()
	}
	if len(tools) != 0 || def.AssemblyError == nil {
		t.Fatal("metadata error silently assembled ordinary-worker access")
	}
}
