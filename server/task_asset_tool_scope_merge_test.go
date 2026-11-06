package server

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/Autumn-27/artex/agent"
	"github.com/Autumn-27/artex/db"
	actool "github.com/Autumn-27/norma/tool"
)

func taskAssetScopeCatalog(t *testing.T, pg *db.DB) {
	t.Helper()
	for _, key := range []string{"list_assets", "list_companies", "insert_assets"} {
		old, err := pg.GetTool(key)
		if err != nil || old == nil {
			t.Fatal("asset tool catalog missing", key, err)
		}
		t.Cleanup(func() {
			grants, _ := json.Marshal(old.Agents)
			if _, err := pg.Exec(`UPDATE tools SET enabled=$2,agents=$3,system=$4,kind=$5 WHERE key=$1`, old.Key, old.Enabled, grants, old.System, old.Kind); err != nil {
				t.Error("restore asset tool catalog", err)
			}
		})
		roles := `["worker","auto","asset_mapper"]`
		if key == "insert_assets" {
			roles = `["worker","auto"]`
		}
		if _, err := pg.Exec(`UPDATE tools SET enabled=true,system=true,kind='builtin',agents=$2 WHERE key=$1`, key, roles); err != nil {
			t.Fatal(err)
		}
	}
}

func taskAssetScopeContext(task *db.Task, session string) context.Context {
	return agent.WithoutTaskToolSet(agent.WithRunInfo(context.Background(), agent.RunInfo{
		TaskID: task.ID, ExplorationID: task.ExplorationID, SessionID: session,
	}))
}

func taskAssetScopeAssemble(t *testing.T, ctx context.Context, role string, base []actool.CoreTool) map[string]actool.CoreTool {
	t.Helper()
	tools, info, cleanup := agent.AugmentTools(ctx, role, base)
	cleanup()
	if info.AssemblyError != nil {
		t.Fatal("asset tool assembly failed", info.AssemblyError)
	}
	return names(tools)
}

func taskAssetScopeCall(t *testing.T, tool actool.CoreTool, ctx context.Context, input string) string {
	t.Helper()
	if tool == nil {
		t.Fatal("asset tool was not assembled")
	}
	result, err := tool.Call(ctx, json.RawMessage(input), nil)
	if err != nil || result.IsError {
		t.Fatal("asset tool call failed", err, result.Flatten())
	}
	return result.Flatten()
}

func taskAssetScopeFixture(t *testing.T) (*db.DB, *db.Task, *db.ExplorationStore, *db.Task, int64, int64, string, string) {
	t.Helper()
	pg := taskToolRegistryPG(t)
	taskAssetScopeCatalog(t, pg)
	own, ownStore, _ := taskToolRegistryTask(t, pg, "asset-scope-owner")
	foreign, _, _ := taskToolRegistryTask(t, pg, "asset-scope-foreign")
	labels := []string{fmt.Sprintf("own-asset-scope-%d.invalid", own.ID), fmt.Sprintf("foreign-asset-scope-%d.invalid", foreign.ID)}
	ids := []int64{}
	for i, task := range []*db.Task{own, foreign} {
		id, err := pg.Assets().UpsertRootDomain(db.UpsertRootDomainReq{Domain: labels[i], TaskID: task.ID})
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
		company, _, err := pg.Companies().UpsertCompany(labels[i], "")
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = pg.Companies().DeleteCompany(company) })
		if _, err := pg.Exec(`UPDATE assets SET company_id=$2 WHERE id=$1`, id, company); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() { _, _ = pg.Assets().DeleteByIDs(ids) })
	return pg, own, ownStore, foreign, ids[0], ids[1], labels[0], labels[1]
}

func TestTaskAssetToolScopeAssociatedReadersAndTasklessAdmin(t *testing.T) {
	pg, own, _, _, ownID, foreignID, ownLabel, foreignLabel := taskAssetScopeFixture(t)
	input := fmt.Sprintf(`{"ids":[%d,%d]}`, ownID, foreignID)
	for _, role := range []string{"auto", "asset_mapper"} {
		ctx := taskAssetScopeContext(own, "asset-reader-"+role)
		tools := taskAssetScopeAssemble(t, ctx, role, []actool.CoreTool{})
		body := taskAssetScopeCall(t, tools["list_assets"], ctx, input)
		if !strings.Contains(body, ownLabel) || strings.Contains(body, foreignLabel) {
			t.Fatal("associated asset reader did not stay task-scoped", role)
		}
		companies := taskAssetScopeCall(t, tools["list_companies"], ctx, `{}`)
		if !strings.Contains(companies, ownLabel) || strings.Contains(companies, foreignLabel) {
			t.Fatal("associated company reader did not stay task-scoped", role)
		}
		shared := true
		if _, err := pg.UpdateTask(own.ID, db.TaskPatch{ShareGlobalAssets: &shared}); err != nil {
			t.Fatal(err)
		}
		if body = taskAssetScopeCall(t, tools["list_assets"], ctx, input); !strings.Contains(body, foreignLabel) {
			t.Fatal("explicit global-sharing opt-in was not live")
		}
		shared = false
		if _, err := pg.UpdateTask(own.ID, db.TaskPatch{ShareGlobalAssets: &shared}); err != nil {
			t.Fatal(err)
		}
		if body = taskAssetScopeCall(t, tools["list_assets"], ctx, input); strings.Contains(body, foreignLabel) {
			t.Fatal("global-sharing opt-out did not apply to the existing reader")
		}
	}
	// The shared prototype was never rebound by either associated run.
	ctx := agent.WithRunInfo(context.Background(), agent.RunInfo{SessionID: "explicit-admin"})
	tools := taskAssetScopeAssemble(t, ctx, "auto", []actool.CoreTool{})
	body := taskAssetScopeCall(t, tools["list_assets"], ctx, input)
	if !strings.Contains(body, ownLabel) || !strings.Contains(body, foreignLabel) {
		t.Fatal("taskless admin asset view was accidentally narrowed")
	}
}

func TestTaskAssetToolScopeNativeOptionalCompanies(t *testing.T) {
	pg, own, store, _, _, _, ownLabel, foreignLabel := taskAssetScopeFixture(t)
	ts := agent.NewToolSet(store, "worker-scope")
	ts.SetAssetStore(pg.Assets(), pg.Companies())
	ts.SetTaskID(own.ID)
	ctx := agent.WithRunInfo(context.Background(), agent.RunInfo{TaskID: own.ID, ExplorationID: own.ExplorationID})
	ctx = agent.WithTaskToolSet(ctx, "worker", ts)
	tools := taskAssetScopeAssemble(t, ctx, "worker", ts.WorkerTools())
	body := taskAssetScopeCall(t, tools["list_companies"], ctx, `{}`)
	if !strings.Contains(body, ownLabel) || strings.Contains(body, foreignLabel) {
		t.Fatal("optional worker company reader used the global prototype")
	}
	for _, update := range []string{`enabled=false`, `enabled=true,agents='["auto"]'`} {
		if _, err := pg.Exec(`UPDATE tools SET ` + update + ` WHERE key='list_companies'`); err != nil {
			t.Fatal(err)
		}
		if tools = taskAssetScopeAssemble(t, ctx, "worker", ts.WorkerTools()); tools["list_companies"] != nil {
			t.Fatal("asset reader ignored disabled or unbound selection")
		}
	}
}

func TestTaskAssetToolScopeInsertedAssetsKeepAssociatedOwner(t *testing.T) {
	pg, own, _, foreign, _, _, _, _ := taskAssetScopeFixture(t)
	ctx := taskAssetScopeContext(own, "associated-insertion")
	tools := taskAssetScopeAssemble(t, ctx, "auto", []actool.CoreTool{})
	label := fmt.Sprintf("associated-insert-%d.invalid", own.ID)
	taskAssetScopeCall(t, tools["insert_assets"], ctx, fmt.Sprintf(`{"assets":[{"type":"root_domain","domain":%q}]}`, label))
	assets, err := pg.Assets().QueryDSLInScope("domain=="+label, "", own.ID, 5, 0)
	if err != nil || len(assets) != 1 {
		t.Fatal("associated insert was not visible to its real owner", err)
	}
	t.Cleanup(func() { _, _ = pg.Assets().DeleteByIDs([]int64{assets[0].ID}) })
	if len(assets[0].TaskIDs) != 1 || assets[0].TaskIDs[0] != own.ID {
		t.Fatal("associated insert lost authoritative ownership")
	}
	if got, err := pg.Assets().QueryDSLInScope("domain=="+label, "", foreign.ID, 5, 0); err != nil || len(got) != 0 {
		t.Fatal("associated insert became visible to an unrelated isolated task", err)
	}
}

func TestTaskAssetToolScopeRejectsWrongDeletedAndReusedContext(t *testing.T) {
	pg, own, _, _, ownID, _, _, _ := taskAssetScopeFixture(t)
	ctx := taskAssetScopeContext(own, "validated-run")
	tools := taskAssetScopeAssemble(t, ctx, "auto", []actool.CoreTool{})
	wrong := agent.WithRunInfo(context.Background(), agent.RunInfo{TaskID: own.ID, ExplorationID: own.ExplorationID + 9999, SessionID: "wrong"})
	_, info, cleanup := agent.AugmentTools(wrong, "auto", []actool.CoreTool{})
	cleanup()
	if info.AssemblyError == nil {
		t.Fatal("wrong task/exploration identity fell back to global assets")
	}
	result, err := tools["list_assets"].Call(wrong, json.RawMessage(fmt.Sprintf(`{"id":%d}`, ownID)), nil)
	if err != nil || !result.IsError {
		t.Fatal("an old scoped asset tool ran under another owner context", err)
	}
	if _, err := pg.Exec(`UPDATE tasks SET deleted_at=now() WHERE id=$1`, own.ID); err != nil {
		t.Fatal(err)
	}
	result, err = tools["list_assets"].Call(ctx, json.RawMessage(fmt.Sprintf(`{"id":%d}`, ownID)), nil)
	if err != nil || !result.IsError {
		t.Fatal("deleted task kept reading through a previously assembled tool", err)
	}
	_, info, cleanup = agent.AugmentTools(ctx, "auto", []actool.CoreTool{})
	cleanup()
	if info.AssemblyError == nil {
		t.Fatal("deleted task kept an attributed global asset fallback")
	}
}
