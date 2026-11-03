package server

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Autumn-27/artex/agent"
	"github.com/Autumn-27/artex/db"
	"github.com/Autumn-27/norma/skill"
)

func TestSkillMCPDependencyWarningsDescribeUnavailableServicesWithoutChangingGrants(t *testing.T) {
	servers := []*db.MCPServer{
		{ID: 1, Name: "disabled", Enabled: false},
		{ID: 2, Name: "direct", Enabled: true},
		{ID: 3, Name: "base-gated", Enabled: true},
		{ID: 4, Name: "variant-only", Enabled: true},
	}
	reg := skill.NewRegistry(skill.Skill{Name: "fixture-skill", MCPs: []string{"missing", "disabled", "direct", "base-gated", "variant-only", "missing"}})
	direct, base := map[int64]bool{2: true}, map[string]bool{"base-gated": true}
	before, _ := json.Marshal([]any{servers, direct, base})
	warnings := skillMCPDependencyWarnings(reg, servers, direct, true, base)
	if len(warnings) != 3 || !strings.Contains(warnings[0], "服务未登记") ||
		!strings.Contains(warnings[1], "服务已停用") || !strings.Contains(warnings[2], "执行角色未获该依赖授权") {
		t.Fatalf("dependency states were not explicit: %v", warnings)
	}
	joined := strings.Join(warnings, "\n")
	if strings.Contains(joined, `"direct"`) || strings.Contains(joined, `"base-gated"`) {
		t.Fatal("authorized execution dependencies were misreported as unavailable")
	}
	after, _ := json.Marshal([]any{servers, direct, base})
	if string(after) != string(before) {
		t.Fatal("dependency observation changed service state or authorization")
	}
	ordinary := skillMCPDependencyWarnings(reg, servers, nil, false, nil)
	if len(ordinary) != 2 || strings.Contains(strings.Join(ordinary, "\n"), "未获") {
		t.Fatal("an ordinary role lost its authorized Skill-gated dependencies")
	}
}

func TestSkillMCPDependencyWarningsAreBoundedBodyFreeMetadata(t *testing.T) {
	const private = "fixture-secret-never-echo"
	reg := skill.NewRegistry(skill.Skill{Name: "https://skill.invalid/?token=" + private, MCPs: []string{"https://dependency.invalid/?token=" + private}})
	warnings := skillMCPDependencyWarnings(reg, nil, nil, false, nil)
	if len(warnings) != 1 || strings.Contains(warnings[0], private) || strings.Contains(warnings[0], "https://") {
		t.Fatal("dependency warning echoed private/untrusted index text")
	}
	entry := skill.Skill{Name: "bounded"}
	for i := 0; i < maxSkillDependencyWarnings+7; i++ {
		entry.MCPs = append(entry.MCPs, fmt.Sprintf("missing-%d", i))
	}
	warnings = skillMCPDependencyWarnings(skill.NewRegistry(entry), nil, nil, false, nil)
	if len(warnings) != maxSkillDependencyWarnings+1 || !strings.Contains(warnings[len(warnings)-1], "7 项") {
		t.Fatal("dependency diagnostic growth was not bounded")
	}
	if got := skillMCPDependencyWarnings(nil, nil, nil, false, nil); got == nil || len(got) != 0 {
		t.Fatal("no Skill registry must have a known empty warning set")
	}
}

func TestSkillMCPDependencyWarningsAreWiredWithoutStartingOrGrantingServices(t *testing.T) {
	for _, scenario := range []struct{ name, category string }{
		{"missing", "服务未登记"}, {"disabled", "服务已停用"}, {"variant-unauthorized", "执行角色未获该依赖授权"},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			pg := openIntranetTestDB(t)
			// Shadow configuration tables on this fixture's one connection. The
			// test does not erase pre-existing false grants or initialization flags.
			pg.SetMaxOpenConns(1)
			pg.SetMaxIdleConns(1)
			if _, err := pg.Exec(`CREATE TEMP TABLE settings (LIKE public.settings INCLUDING ALL);
CREATE TEMP TABLE agent_skill_visibility (LIKE public.agent_skill_visibility INCLUDING ALL);
CREATE TEMP TABLE agent_visibility (LIKE public.agent_visibility INCLUDING ALL)`); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				_, _ = pg.Exec(`DROP TABLE IF EXISTS pg_temp.agent_visibility, pg_temp.agent_skill_visibility, pg_temp.settings`)
			})
			worker, err := pg.GetAgentByKey("worker")
			if err != nil || worker == nil {
				t.Fatal("worker fixture missing", err)
			}
			variant, err := pg.GetAgentByKey("worker.intranet")
			if err != nil || variant == nil {
				t.Fatal("variant fixture missing", err)
			}
			oldAugment := agent.ToolAugment
			t.Cleanup(func() { agent.ToolAugment = oldAugment })
			if err := pg.SetAgentVisibilityKind(worker.ID, "mcp", nil); err != nil {
				t.Fatal(err)
			}
			dependency := fmt.Sprintf("fixture-skill-dependency-%d", time.Now().UnixNano())
			var serverID int64
			if scenario.name != "missing" {
				serverID, err = pg.SaveMCP(&db.MCPServer{Name: dependency, Transport: "stdio", Command: "/fixture/must-not-start", Enabled: scenario.name != "disabled"})
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = pg.DeleteMCP(serverID) })
			}
			root := t.TempDir()
			const skillName = "fixture-dependency-guidance"
			dir := filepath.Join(root, skillName)
			if err := os.MkdirAll(dir, 0o755); err != nil {
				t.Fatal(err)
			}
			index := fmt.Sprintf("---\nname: %s\ndescription: dependency diagnostics fixture\nmcps: [%s]\n---\nRead the registered guidance; do not infer dependency readiness.\n", skillName, dependency)
			if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte(index), 0o644); err != nil {
				t.Fatal(err)
			}
			ctx := agent.WithRunInfo(t.Context(), agent.RunInfo{TaskID: 701, IntentID: 702})
			if scenario.name == "variant-unauthorized" {
				if err := pg.SetAgentSkillVisibility(worker.ID, nil); err != nil {
					t.Fatal(err)
				}
				if err := pg.SetAgentSkillVisibility(variant.ID, []string{skillName}); err != nil {
					t.Fatal(err)
				}
				ctx = agent.WithSkillVisibilityAgent(ctx, "worker", "worker.intranet")
			} else if err := pg.SetAgentSkillVisibility(worker.ID, []string{skillName}); err != nil {
				t.Fatal(err)
			}
			wireAgentAugment(pg, root, nil)
			tools, info, cleanup := agent.ToolAugment(ctx, "worker")
			defer cleanup()
			if info.AssemblyError != nil || len(info.AssemblyWarnings) != 1 ||
				!strings.Contains(info.AssemblyWarnings[0], dependency) || !strings.Contains(info.AssemblyWarnings[0], scenario.category) {
				t.Fatalf("missing dependency diagnosis: warnings=%v err=%v", info.AssemblyWarnings, info.AssemblyError)
			}
			hasSkill := false
			for _, tool := range tools {
				if tool.Name() == "Skill" {
					hasSkill = true
				}
				if strings.Contains(tool.Name(), dependency) {
					t.Fatal("unavailable dependency was exposed as a callable tool")
				}
			}
			if !hasSkill || len(info.Deferred) != 0 || len(info.GlobalNames) != 0 {
				t.Fatal("warning removed guidance or advertised unavailable dependencies")
			}
			grants, err := pg.AgentVisible(worker.ID, "mcp")
			if err != nil || len(grants) != 0 {
				t.Fatal("dependency observation changed execution-role MCP grants", err)
			}
			if serverID != 0 {
				var enabled bool
				if err := pg.QueryRow(`SELECT enabled FROM mcp_servers WHERE id=$1`, serverID).Scan(&enabled); err != nil || enabled != (scenario.name != "disabled") {
					t.Fatal("dependency observation changed the service switch", err)
				}
			}
		})
	}
}
