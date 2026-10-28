package server

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/Autumn-27/artex/agent"
	"github.com/Autumn-27/artex/db"
	actool "github.com/Autumn-27/norma/tool"
)

// hasSkillTool reports whether the packed tool set contains the Skill meta-tool.
func hasSkillTool(tools []actool.CoreTool) bool {
	for _, t := range tools {
		if t.Name() == "Skill" {
			return true
		}
	}
	return false
}

func TestAssembleCachedStdioToolsWithoutStartingProcess(t *testing.T) {
	dsn, _, err := db.DSN()
	if err != nil {
		t.Skip("no test database")
	}
	pg, err := db.Open(dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = pg.Close() })
	ag, err := pg.GetAgentByKey("planner")
	if err != nil || ag == nil {
		t.Fatal("planner missing")
	}
	orig, err := pg.AgentVisible(ag.ID, "mcp")
	if err != nil {
		t.Fatal(err)
	}
	origSkills, err := pg.AgentSkillNames(ag.ID)
	if err != nil {
		t.Fatal(err)
	}
	// This fixture tests cached MCP assembly only. Seeded default Skill grants
	// require real indexes and must not be pointed at this empty temporary root.
	if err := pg.SetAgentSkillVisibility(ag.ID, nil); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = pg.SetAgentSkillVisibility(ag.ID, origSkills) })
	_, _ = pg.Exec(`DELETE FROM mcp_servers WHERE name='t-lazy-cached'`)
	m := &db.MCPServer{Name: "t-lazy-cached", Transport: "stdio", Command: "/fixture/must-not-be-started", Enabled: true}
	m.ID, err = pg.SaveMCP(m)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = pg.SetAgentVisibilityKind(ag.ID, "mcp", orig); _ = pg.DeleteMCP(m.ID) })
	const toolName = "mcp__t-lazy-cached__fixture"
	if err = pg.SaveMCPTools(m.ID, []db.MCPTool{{Name: toolName, Description: "cached schema fixture", Schema: json.RawMessage(`{"type":"object","properties":{"query":{"type":"string"}}}`)}}); err != nil {
		t.Fatal(err)
	}
	if err = pg.SetAgentVisibilityKind(ag.ID, "mcp", []int64{m.ID}); err != nil {
		t.Fatal(err)
	}
	old := agent.ToolAugment
	wireAgentAugment(pg, t.TempDir(), nil)
	t.Cleanup(func() { agent.ToolAugment = old })
	for range 8 {
		tools, def, cleanup := agent.ToolAugment(context.Background(), "planner")
		if cleanup != nil {
			defer cleanup()
		}
		if def.AssemblyError != nil {
			t.Fatal("cached MCP fixture failed authorization assembly", def.AssemblyError)
		}
		found := false
		for _, tool := range tools {
			if tool.Name() == toolName {
				found = true
				if tool.InputSchema()["type"] != "object" {
					t.Fatal("cached schema not exposed")
				}
			}
		}
		if !found || !contains(def.GlobalNames, toolName) {
			t.Fatal("cached MCP tool was dropped or process startup was attempted")
		}
	}
}

// TestAssembleVisibleSkill verifies a filesystem skill made visible to an agent is
// packed into that agent's tool set (as the single Skill meta-tool) via the wired
// ToolAugment hook. Skills live on disk under skillDir; visibility is a per-agent
// (agent × skill_name) row keyed by the skill's directory name.
func TestAssembleVisibleSkill(t *testing.T) {
	dsn, _, err := db.DSN()
	if err != nil {
		t.Skipf("no database config (%v) — skipping", err)
	}
	pg, err := db.Open(dsn)
	if err != nil {
		t.Skipf("postgres unavailable (%v) — skipping", err)
	}
	t.Cleanup(func() { _ = pg.Close() })

	// Filesystem skill fixture: <skillDir>/t-assemble/SKILL.md. The directory name
	// (t-assemble) is the visibility key matched against AgentSkillNames.
	skillDir := t.TempDir()
	const skillName = "t-assemble"
	if err := os.MkdirAll(filepath.Join(skillDir, skillName), 0o755); err != nil {
		t.Fatal(err)
	}
	md := "---\nname: t-assemble\ndescription: do the thing\n---\ndo the thing"
	if err := os.WriteFile(filepath.Join(skillDir, skillName, "SKILL.md"), []byte(md), 0o644); err != nil {
		t.Fatal(err)
	}

	oldAugment, oldResolve := agent.ToolAugment, agent.ToolResolve
	wireAgentAugment(pg, skillDir, nil)
	agent.ToolResolve = nil // this fixture isolates Skill packing, not catalog resolution
	t.Cleanup(func() { agent.ToolAugment, agent.ToolResolve = oldAugment, oldResolve })

	ag, _ := pg.GetAgentByKey("planner")
	if ag == nil {
		t.Fatal("planner agent missing")
	}

	// Isolate this test from whatever skill visibility the planner already has in the
	// shared DB: clear it now, restore it on cleanup.
	orig, err := pg.AgentSkillNames(ag.ID)
	if err != nil {
		t.Fatal(err)
	}
	origMCP, err := pg.AgentVisible(ag.ID, "mcp")
	if err != nil {
		t.Fatal(err)
	}
	if err := pg.SetAgentVisibilityKind(ag.ID, "mcp", nil); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = pg.SetAgentVisibilityKind(ag.ID, "mcp", origMCP) })
	if err := pg.SetAgentSkillVisibility(ag.ID, nil); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = pg.SetAgentSkillVisibility(ag.ID, orig) })

	// before: skill not visible → no Skill meta-tool
	extra, def, cleanup := agent.ToolAugment(context.Background(), "planner")
	if cleanup != nil {
		cleanup()
	}
	if def.AssemblyError != nil {
		t.Fatal("empty Skill grant fixture failed assembly", def.AssemblyError)
	}
	if hasSkillTool(extra) {
		t.Fatalf("expected no Skill meta-tool before the skill is made visible")
	}

	// make the skill visible to planner
	if err := pg.ToggleSkillVisibility(ag.ID, skillName, true); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = pg.ToggleSkillVisibility(ag.ID, skillName, false) })

	// after: the Skill meta-tool is packed in
	extra, def, cleanup = agent.ToolAugment(context.Background(), "planner")
	if cleanup != nil {
		defer cleanup()
	}
	if def.AssemblyError != nil {
		t.Fatal("visible Skill fixture failed assembly", def.AssemblyError)
	}
	if !hasSkillTool(extra) {
		t.Fatalf("expected the Skill meta-tool after making the skill visible")
	}

	// AugmentTools appends extra to base without filtering base
	combined, combinedDef, c2 := agent.AugmentTools(context.Background(), "planner", nil)
	if c2 != nil {
		defer c2()
	}
	if combinedDef.AssemblyError != nil {
		t.Fatal("visible Skill fixture failed combined assembly", combinedDef.AssemblyError)
	}
	if !hasSkillTool(combined) {
		t.Fatalf("AugmentTools should surface the Skill meta-tool")
	}
}
