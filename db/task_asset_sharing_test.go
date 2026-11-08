package db

import (
	"encoding/json"
	"fmt"
	"testing"
)

func TestTaskAssetSharingOffAllowsManualAssociations(t *testing.T) {
	d, assets, _ := testSetup(t)
	defer d.Close()
	task, err := d.CreateTask("manual assets while sharing off", "goal", nil, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer d.DeleteTask(task.ID)
	if task.ShareGlobalAssets {
		t.Fatal("task must default to sharing off")
	}
	var ids []int64
	defer func() { _, _ = assets.DeleteByIDs(ids) }()
	for _, name := range []string{"selected", "unrelated"} {
		id, err := assets.UpsertRootDomain(UpsertRootDomainReq{Domain: fmt.Sprintf("manual-sharing-%d-%s.invalid", task.ID, name)})
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}
	if got, err := assets.GetByIDsInScope(task.ID, ids); err != nil || len(got) != 0 {
		t.Fatalf("unassociated global assets must be invisible: got=%v err=%v", got, err)
	}
	if _, err := assets.AttachAssetsToTask(task.ID, ids[:1], "用户明确选择的测试资产"); err != nil {
		t.Fatal(err)
	}
	got, err := assets.GetByIDsInScope(task.ID, ids)
	if err != nil || len(got) != 1 || got[0].ID != ids[0] {
		t.Fatalf("only the manually associated asset should be visible: got=%v err=%v", got, err)
	}
	query := fmt.Sprintf("id==%d OR id==%d", ids[0], ids[1])
	got, err = assets.QueryDSLInScope(query, "", task.ID, 10, 0)
	if err != nil || len(got) != 1 || got[0].ID != ids[0] {
		t.Fatalf("DSL must allow the association without exposing unrelated assets: got=%v err=%v", got, err)
	}
}

func TestTaskAssetSharingLegacyArchiveDefault(t *testing.T) {
	d, _, _ := testSetup(t)
	defer d.Close()
	task, err := d.CreateTaskWithOptions("legacy sharing archive", "goal", TaskCreateOptions{ShareGlobalAssets: true})
	if err != nil {
		t.Fatal(err)
	}
	defer d.DeleteTask(task.ID)
	var raw []byte
	if err := d.QueryRow(`SELECT row_to_json(t) FROM tasks t WHERE id=$1`, task.ID).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	var row map[string]any
	if err := json.Unmarshal(raw, &row); err != nil {
		t.Fatal(err)
	}
	delete(row, "share_global_assets")
	tx, err := d.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if err := restoreTaskStub(tx, row, task.ID, 0); err != nil {
		t.Fatal(err)
	}
	var enabled bool
	if err := tx.QueryRow(`SELECT share_global_assets FROM tasks WHERE id=$1`, task.ID).Scan(&enabled); err != nil {
		t.Fatal(err)
	}
	if enabled {
		t.Fatal("archives created before the sharing field must restore with sharing disabled")
	}
}

func TestTaskAssetSharingDeletedContextDoesNotReadSources(t *testing.T) {
	d, assets, companies := testSetup(t)
	defer d.Close()
	source, err := d.CreateTaskWithOptions("sharing deleted-source guard", "fixture", TaskCreateOptions{ShareGlobalAssets: true})
	if err != nil {
		t.Fatal(err)
	}
	defer d.DeleteTask(source.ID)
	current, err := d.CreateTaskWithOptions("sharing deleted-current guard", "fixture", TaskCreateOptions{SourceTaskIDs: []int64{source.ID}})
	if err != nil {
		t.Fatal(err)
	}
	defer d.DeleteTask(current.ID)
	domain := fmt.Sprintf("deleted-current-sharing-%d.invalid", current.ID)
	assetID, err := assets.UpsertRootDomain(UpsertRootDomainReq{Domain: domain, TaskID: source.ID})
	if err != nil {
		t.Fatal(err)
	}
	defer assets.DeleteByIDs([]int64{assetID})
	if _, err := assets.AddAgentScope(source.ID, "root_domain", domain, "fixture", "agent"); err != nil {
		t.Fatal(err)
	}
	if got, err := assets.GetByIDsInScope(current.ID, []int64{assetID}); err != nil || len(got) != 1 {
		t.Fatalf("live direct source must remain visible: count=%d err=%v", len(got), err)
	}
	if _, err := d.Exec(`UPDATE tasks SET deleted_at=now() WHERE id=$1`, current.ID); err != nil {
		t.Fatal(err)
	}
	if got, err := assets.GetByIDsInScope(current.ID, []int64{assetID}); err != nil || len(got) != 0 {
		t.Fatalf("deleted current task exposed source IDs: count=%d err=%v", len(got), err)
	}
	if got, err := assets.QueryDSLInScope(fmt.Sprintf("id==%d", assetID), "", current.ID, 10, 0); err != nil || len(got) != 0 {
		t.Fatalf("deleted current task exposed source DSL rows: count=%d err=%v", len(got), err)
	}
	if got, err := assets.ListTaskScopeWithSources(current.ID); err != nil || len(got) != 0 {
		t.Fatalf("deleted current task exposed source scope: count=%d err=%v", len(got), err)
	}
	if got, err := assets.HostsByTaskWithSources(current.ID); err != nil || len(got) != 0 {
		t.Fatalf("deleted current task exposed source hosts: count=%d err=%v", len(got), err)
	}
	if got, err := assets.TaskCoverageWithSources(current.ID); err != nil || got.Denominator != 0 || got.ScopeRows != 0 {
		t.Fatalf("deleted current task exposed source coverage: %+v err=%v", got, err)
	}
	if got, err := assets.BuildCoverageGraph(current.ID, current.ExplorationID); err != nil || len(got.Nodes) != 0 {
		t.Fatalf("deleted current task exposed source graph: %+v err=%v", got, err)
	}
	if _, err := companies.ListCompaniesForTask(current.ID); err == nil {
		t.Fatal("deleted current task was accepted as an active company context")
	}
}
