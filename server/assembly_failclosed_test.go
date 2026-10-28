package server

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Autumn-27/artex/agent"
	"github.com/Autumn-27/artex/db"
	actool "github.com/Autumn-27/norma/tool"
)

func TestToolCatalogLookupFailureStopsAssembly(t *testing.T) {
	closed, err := sql.Open("pgx", "postgres://fixture.invalid/unused")
	if err != nil {
		t.Fatal(err)
	}
	_ = closed.Close()
	oldAugment, oldResolve := agent.ToolAugment, agent.ToolResolve
	oldTraffic, oldIntelligence, oldRecord := agent.FindingTrafficBindingEnabled, agent.IntelligenceEnabled, agent.IntelligenceRecord
	t.Cleanup(func() {
		agent.ToolAugment, agent.ToolResolve = oldAugment, oldResolve
		agent.FindingTrafficBindingEnabled, agent.IntelligenceEnabled, agent.IntelligenceRecord = oldTraffic, oldIntelligence, oldRecord
	})
	agent.ToolAugment = nil
	wireTools(&db.DB{DB: closed}, nil)
	base := actool.DefaultTools()
	tools, def, cleanup := agent.AugmentTools(t.Context(), "worker", base)
	cleanup()
	if len(tools) != 0 || def.AssemblyError == nil {
		t.Fatal("catalog error returned unfiltered base capabilities")
	}
}

func TestAuthorizationLookupFailureStopsAllAgentRoles(t *testing.T) {
	closed, err := sql.Open("pgx", "postgres://fixture.invalid/unused")
	if err != nil {
		t.Fatal(err)
	}
	_ = closed.Close()
	oldAugment := agent.ToolAugment
	t.Cleanup(func() { agent.ToolAugment = oldAugment })
	wireAgentAugment(&db.DB{DB: closed}, t.TempDir(), nil)
	for _, role := range []string{"worker", "planner", "pentest", "goals"} {
		tools, def, cleanup := agent.ToolAugment(t.Context(), role)
		if cleanup != nil {
			cleanup()
		}
		if len(tools) != 0 || def.AssemblyError == nil {
			t.Fatalf("role %s silently ignored failed authorization metadata", role)
		}
	}
}

func TestGoalsSkillAssemblyCannotExposeMCPHostOrShell(t *testing.T) {
	pg := openIntranetTestDB(t)
	goals, err := pg.GetAgentByKey("goals")
	if err != nil || goals == nil {
		t.Fatal("goals fixture unavailable", err)
	}
	oldSkills, err := pg.AgentSkillNames(goals.ID)
	if err != nil {
		t.Fatal(err)
	}
	oldMCP, err := pg.AgentVisible(goals.ID, "mcp")
	if err != nil {
		t.Fatal(err)
	}
	oldAugment, oldResolve := agent.ToolAugment, agent.ToolResolve
	oldTraffic, oldIntelligence, oldRecord := agent.FindingTrafficBindingEnabled, agent.IntelligenceEnabled, agent.IntelligenceRecord
	t.Cleanup(func() {
		agent.ToolAugment, agent.ToolResolve = oldAugment, oldResolve
		agent.FindingTrafficBindingEnabled, agent.IntelligenceEnabled, agent.IntelligenceRecord = oldTraffic, oldIntelligence, oldRecord
		_ = pg.SetAgentSkillVisibility(goals.ID, oldSkills)
		_ = pg.SetAgentVisibilityKind(goals.ID, "mcp", oldMCP)
	})
	mcpID, err := pg.SaveMCP(&db.MCPServer{Name: "goals-no-execution", Transport: "stdio", Command: "/fixture/must-not-start", Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = pg.DeleteMCP(mcpID) })
	if err := pg.SetAgentVisibilityKind(goals.ID, "mcp", []int64{mcpID}); err != nil {
		t.Fatal(err)
	}
	if err := pg.SetAgentSkillVisibility(goals.ID, []string{"goal-guidance"}); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	skillDir := filepath.Join(dir, "goal-guidance")
	if err := os.MkdirAll(skillDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(skillDir, "SKILL.md"), []byte("---\nname: goal-guidance\ndescription: read-only fixture\nmcps: [goals-no-execution]\n---\nUse guidance for decomposing goals, not execution.\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	hostCalls := 0
	wireAgentAugment(pg, dir, func() ([]actool.CoreTool, map[string][]string) {
		hostCalls++
		return []actool.CoreTool{actool.NewBash()}, map[string][]string{"Bash": {"goals"}}
	})
	wireTools(pg, map[string]actool.CoreTool{"Bash": actool.NewBash()})
	ctx := agent.WithRunInfo(t.Context(), agent.RunInfo{TaskID: 991118, ExplorationID: 991119})
	tools, def, cleanup := agent.AugmentTools(ctx, "goals", []actool.CoreTool{actool.NewBash()})
	defer cleanup()
	if def.AssemblyError != nil || hostCalls != 0 || len(def.Deferred) != 0 || def.Unlock != nil || def.UnlockSkill != nil {
		t.Fatal("goals assembled execution dependencies", def.AssemblyError)
	}
	got := names(tools)
	if len(tools) != 2 || got["Skill"] == nil || got["read_skill_reference"] == nil || !got["read_skill_reference"].IsReadOnly(nil) || def.SkillDirs["goal-guidance"] != skillDir {
		t.Fatal("goals lost authorized read-only guidance or exposed shell")
	}
	result, err := got["Skill"].Call(ctx, json.RawMessage(`{"name":"goal-guidance"}`), nil)
	if err != nil || result.IsError {
		t.Fatal("goals guidance did not actually load", err)
	}
	var count int
	if err := pg.QueryRow(`SELECT COUNT(*) FROM skill_usage WHERE skill='goal-guidance' AND agent_key='goals' AND task_id=991118 AND exploration_id=991119`).Scan(&count); err != nil || count != 1 {
		t.Fatal("goals Skill load was not attributed/metered", err)
	}
	t.Cleanup(func() { _, _ = pg.Exec(`DELETE FROM skill_usage WHERE skill='goal-guidance' AND task_id=991118`) })
}

func TestMissingAuthorizedSkillStopsOrdinaryAssembly(t *testing.T) {
	pg := openIntranetTestDB(t)
	worker, err := pg.GetAgentByKey("worker")
	if err != nil || worker == nil {
		t.Fatal("worker fixture unavailable", err)
	}
	oldSkills, err := pg.AgentSkillNames(worker.ID)
	if err != nil {
		t.Fatal(err)
	}
	oldAugment := agent.ToolAugment
	t.Cleanup(func() {
		agent.ToolAugment = oldAugment
		_ = pg.SetAgentSkillVisibility(worker.ID, oldSkills)
	})
	if err := pg.SetAgentSkillVisibility(worker.ID, []string{"missing-authorized-fixture"}); err != nil {
		t.Fatal(err)
	}
	wireAgentAugment(pg, t.TempDir(), nil)
	tools, def, cleanup := agent.ToolAugment(context.Background(), "worker")
	if cleanup != nil {
		cleanup()
	}
	if len(tools) != 0 || def.AssemblyError == nil {
		t.Fatal("missing authorized Skill was treated as an empty successful registry")
	}
}

func TestProcessorLegacyGrantsNeverInitializeExecutionDependencies(t *testing.T) {
	pg := openIntranetTestDB(t)
	processor, err := pg.GetAgentByKey(db.CredentialAgentKey)
	if err != nil {
		t.Fatal(err)
	}
	created := processor == nil
	if created {
		processor, err = pg.CreateAgent(db.CredentialAgentKey, "processor fixture", "data-only fixture")
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = pg.DeleteAgent(db.CredentialAgentKey) })
	}
	oldSkills, err := pg.AgentSkillNames(processor.ID)
	if err != nil {
		t.Fatal(err)
	}
	oldAugment, oldResolve := agent.ToolAugment, agent.ToolResolve
	oldTraffic, oldIntelligence, oldRecord := agent.FindingTrafficBindingEnabled, agent.IntelligenceEnabled, agent.IntelligenceRecord
	t.Cleanup(func() {
		agent.ToolAugment, agent.ToolResolve = oldAugment, oldResolve
		agent.FindingTrafficBindingEnabled, agent.IntelligenceEnabled, agent.IntelligenceRecord = oldTraffic, oldIntelligence, oldRecord
		if !created {
			_ = pg.SetAgentSkillVisibility(processor.ID, oldSkills)
		}
	})
	if err := pg.SetAgentSkillVisibility(processor.ID, []string{"missing-legacy-processor-grant"}); err != nil {
		t.Fatal(err)
	}
	wireAgentAugment(pg, filepath.Join(t.TempDir(), "not-created"), func() ([]actool.CoreTool, map[string][]string) {
		t.Fatal("processor initialized executable host tools")
		return nil, nil
	})
	tools, def, cleanup := agent.ToolAugment(t.Context(), db.CredentialAgentKey)
	if cleanup != nil {
		defer cleanup()
	}
	if def.AssemblyError != nil || tools == nil || len(tools) != 0 || len(def.SkillDirs) != 0 || def.Unlock != nil {
		t.Fatal("legacy processor Skill grants changed its data-only contract", def.AssemblyError)
	}
	wireTools(pg, nil)
	if direct := agent.ToolResolve(t.Context(), db.CredentialAgentKey, actool.DefaultTools()); direct == nil || len(direct) != 0 {
		t.Fatal("processor resolver retained default execution tools")
	}
}

func TestAuthorizedAliasCollisionWinnerStillStopsAssembly(t *testing.T) {
	pg := openIntranetTestDB(t)
	worker, err := pg.GetAgentByKey("worker")
	if err != nil || worker == nil {
		t.Fatal("worker fixture unavailable", err)
	}
	oldSkills, err := pg.AgentSkillNames(worker.ID)
	if err != nil {
		t.Fatal(err)
	}
	oldAugment := agent.ToolAugment
	t.Cleanup(func() {
		agent.ToolAugment = oldAugment
		_ = pg.SetAgentSkillVisibility(worker.ID, oldSkills)
	})
	if err := pg.SetAgentSkillVisibility(worker.ID, []string{"zzz-granted"}); err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	for _, name := range []string{"aaa-ungranted", "zzz-granted"} {
		entry := filepath.Join(root, name)
		if err := os.MkdirAll(entry, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(entry, "SKILL.md"), []byte("---\nname: shared-alias\ndescription: fixture\n---\nfixture guidance"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	wireAgentAugment(pg, root, nil)
	tools, def, cleanup := agent.ToolAugment(t.Context(), "worker")
	if cleanup != nil {
		cleanup()
	}
	if len(tools) != 0 || def.AssemblyError == nil {
		t.Fatal("SDK collision winner was advertised as loadable despite invalid catalog metadata")
	}
}

func TestOptionalMCPUnavailableDoesNotDiscardOtherAuthorizedTools(t *testing.T) {
	for _, scenario := range []string{"initialize-failure", "list-failure", "missing-schema"} {
		t.Run(scenario, func(t *testing.T) {
			pg := openIntranetTestDB(t)
			worker, err := pg.GetAgentByKey("worker")
			if err != nil || worker == nil {
				t.Fatal("worker fixture unavailable", err)
			}
			oldSkills, err := pg.AgentSkillNames(worker.ID)
			if err != nil {
				t.Fatal(err)
			}
			oldMCP, err := pg.AgentVisible(worker.ID, "mcp")
			if err != nil {
				t.Fatal(err)
			}
			oldAugment, oldResolve := agent.ToolAugment, agent.ToolResolve
			oldTraffic, oldIntelligence, oldRecord := agent.FindingTrafficBindingEnabled, agent.IntelligenceEnabled, agent.IntelligenceRecord
			t.Cleanup(func() {
				agent.ToolAugment, agent.ToolResolve = oldAugment, oldResolve
				agent.FindingTrafficBindingEnabled, agent.IntelligenceEnabled, agent.IntelligenceRecord = oldTraffic, oldIntelligence, oldRecord
				_ = pg.SetAgentSkillVisibility(worker.ID, oldSkills)
				_ = pg.SetAgentVisibilityKind(worker.ID, "mcp", oldMCP)
			})
			if err := pg.SetAgentSkillVisibility(worker.ID, nil); err != nil {
				t.Fatal(err)
			}
			const privateResponse = "fixture-secret-never-expose"
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if scenario == "initialize-failure" {
					http.Error(w, privateResponse, http.StatusServiceUnavailable)
					return
				}
				var request struct {
					ID     json.RawMessage `json:"id"`
					Method string          `json:"method"`
				}
				if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
					w.WriteHeader(http.StatusBadRequest)
					return
				}
				if request.Method == "notifications/initialized" {
					w.WriteHeader(http.StatusNoContent)
					return
				}
				w.Header().Set("Content-Type", "application/json")
				if request.Method == "initialize" {
					_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": request.ID, "result": map[string]any{"protocolVersion": "2025-06-18", "capabilities": map[string]any{}}})
					return
				}
				_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": request.ID, "error": map[string]any{"code": -32603, "message": privateResponse}})
			}))
			defer server.Close()
			unavailable := &db.MCPServer{Name: "optional-" + scenario, Transport: "http", URL: server.URL + "/mcp?token=" + privateResponse, Enabled: true}
			if scenario == "missing-schema" {
				// An empty stdio command fails discovery without starting any
				// process, and a name-only legacy cache is not a valid schema.
				unavailable.Transport, unavailable.Command, unavailable.URL = "stdio", "", ""
			}
			unavailableID, err := pg.SaveMCP(unavailable)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = pg.DeleteMCP(unavailableID) })
			if scenario == "missing-schema" {
				if err := pg.SaveMCPTools(unavailableID, []db.MCPTool{{Name: "mcp__optional_missing_schema__legacy"}}); err != nil {
					t.Fatal(err)
				}
			}
			readyID, err := pg.SaveMCP(&db.MCPServer{Name: "ready-" + scenario, Transport: "stdio", Command: "/fixture/must-not-start", Enabled: true})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = pg.DeleteMCP(readyID) })
			readyName := "mcp__ready-" + scenario + "__inspect"
			if err := pg.SaveMCPTools(readyID, []db.MCPTool{{Name: readyName, Schema: json.RawMessage(`{"type":"object"}`)}}); err != nil {
				t.Fatal(err)
			}
			if err := pg.SetAgentVisibilityKind(worker.ID, "mcp", []int64{unavailableID, readyID}); err != nil {
				t.Fatal(err)
			}
			wireAgentAugment(pg, t.TempDir(), nil)
			wireTools(pg, nil)
			tools, def, cleanup := agent.AugmentTools(t.Context(), "worker", []actool.CoreTool{actool.NewBash()})
			defer cleanup()
			if def.AssemblyError != nil || len(def.AssemblyWarnings) != 1 {
				t.Fatal("optional transport failure stopped the entire authorized run", def.AssemblyError)
			}
			if !strings.Contains(def.AssemblyWarnings[0], unavailable.Name) || strings.Contains(def.AssemblyWarnings[0], privateResponse) || strings.Contains(def.AssemblyWarnings[0], server.URL) {
				t.Fatal("availability warning omitted the service or leaked transport details")
			}
			got := names(tools)
			if got["Bash"] == nil || got[readyName] == nil {
				t.Fatal("optional failure discarded correctly authorized base/cache tools")
			}
			for name := range got {
				if strings.Contains(name, "optional") {
					t.Fatal("unavailable service was exposed as a callable tool")
				}
			}
			if len(def.Deferred) != 1 || def.Deferred[0] != readyName || !def.Unlock.Has(readyName) {
				t.Fatal("deferred list advertised missing schemas or lost the valid cache")
			}
		})
	}
}
