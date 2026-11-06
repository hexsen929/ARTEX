package agent

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/Autumn-27/artex/db"
)

func TestTaskAssetSharingLiveToolPolicy(t *testing.T) {
	d := testDB(t)
	defer d.Close()
	task, err := d.CreateTask("tool sharing", "goal", nil, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer d.DeleteTask(task.ID)
	assets := d.Assets()
	company, _, err := d.Companies().UpsertCompany(fmt.Sprintf("sharing-tool-%d", task.ID), "")
	if err != nil {
		t.Fatal(err)
	}
	defer d.Companies().DeleteCompany(company)
	foreign, err := assets.UpsertRootDomain(db.UpsertRootDomainReq{Domain: fmt.Sprintf("sharing-tool-%d.invalid", task.ID)})
	if err != nil {
		t.Fatal(err)
	}
	defer assets.DeleteByIDs([]int64{foreign})
	if _, err = d.Exec(`UPDATE assets SET company_id=$2 WHERE id=$1`, foreign, company); err != nil {
		t.Fatal(err)
	}
	tools := NewToolSet(d.Exploration(task.ExplorationID), "planner")
	tools.SetTaskID(task.ID)
	tools.SetAssetStore(assets, d.Companies())
	idJSON, _ := json.Marshal(foreign)
	intent := intentItem{Summary: "reference foreign target", AssetIDs: []json.RawMessage{idJSON, idJSON}}
	if _, err := tools.addOneIntent(intent); err == nil {
		t.Fatal("foreign intent should fail while sharing off")
	}
	query := fmt.Sprintf(`{"id":%d}`, foreign)
	if got := callReadJSON(t, tools.listAssets(), query); strings.Contains(fmt.Sprint(got), fmt.Sprintf("sharing-tool-%d.invalid", task.ID)) {
		t.Fatalf("foreign asset leaked: %v", got)
	}
	for _, enabled := range []bool{true, false} {
		if _, err := d.UpdateTask(task.ID, db.TaskPatch{ShareGlobalAssets: &enabled}); err != nil {
			t.Fatal(err)
		}
		got := callReadJSON(t, tools.listAssets(), query)
		visible := strings.Contains(fmt.Sprint(got), fmt.Sprintf("sharing-tool-%d.invalid", task.ID))
		if visible != enabled {
			t.Fatalf("live list_assets enabled=%v result=%v", enabled, got)
		}
		companies, err := d.Companies().ListCompaniesForTask(task.ID)
		if err != nil {
			t.Fatal(err)
		}
		found := false
		for _, c := range companies {
			if c.ID == company {
				found = true
			}
		}
		if found != enabled {
			t.Fatalf("live company policy enabled=%v found=%v", enabled, found)
		}
	}
	enabled := true
	if _, err := d.UpdateTask(task.ID, db.TaskPatch{ShareGlobalAssets: &enabled}); err != nil {
		t.Fatal(err)
	}
	if _, err := tools.addOneIntent(intent); err != nil {
		t.Fatalf("explicit opt-in should allow target and duplicate IDs: %v", err)
	}
}

func TestTaskAssetSharingModelAnchorsCannotGrantRead(t *testing.T) {
	d := testDB(t)
	defer d.Close()
	task, err := d.CreateTask("model anchor sharing guard", "fixture", nil, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer d.DeleteTask(task.ID)
	defer d.Exec(`DELETE FROM findings WHERE task_id=$1`, task.ID)
	assets := d.Assets()
	domain := fmt.Sprintf("model-anchor-sharing-%d.invalid", task.ID)
	foreign, err := assets.UpsertRootDomain(db.UpsertRootDomainReq{Domain: domain})
	if err != nil {
		t.Fatal(err)
	}
	defer assets.DeleteByIDs([]int64{foreign})
	tools := NewToolSet(d.Exploration(task.ExplorationID), "worker")
	tools.SetTaskID(task.ID)
	tools.SetAssetStore(assets, d.Companies())
	rawID := json.RawMessage(fmt.Sprint(foreign))
	if _, _, err := tools.recordOneFact(factItem{Summary: "must not acquire a foreign asset", AssetIDs: []json.RawMessage{rawID}}, 0); err == nil {
		t.Fatal("record_fact accepted a previously invisible asset reference")
	}
	if _, err := tools.addOneHint(hintItem{Text: "must not acquire a foreign asset", AssetIDs: []json.RawMessage{rawID}}); err == nil {
		t.Fatal("add_hint accepted a previously invisible asset reference")
	}
	for _, explicit := range []bool{true, false} {
		input := map[string]any{"vulnclass": "fixture", "severity": "low", "summary": "saved lead for http://" + domain + "/", "evidence": "fixture evidence stays available"}
		if explicit {
			input["asset_ids"] = []int64{foreign}
		}
		raw, err := json.Marshal(input)
		if err != nil {
			t.Fatal(err)
		}
		result, err := tools.addFinding().Call(t.Context(), raw, nil)
		if err != nil || !strings.HasPrefix(result.Flatten(), "finding recorded: ") {
			t.Fatalf("finding content was discarded instead of preserving an unanchored lead: %v %s", err, result.Flatten())
		}
		var output struct {
			db.RecordedFinding
			AssetNote    string `json:"asset_note"`
			AutoAnchored bool   `json:"auto_anchored"`
		}
		if err := json.Unmarshal([]byte(strings.SplitN(result.Flatten(), "\n", 2)[1]), &output); err != nil {
			t.Fatal(err)
		}
		if output.AutoAnchored || (explicit && output.AssetNote == "") {
			t.Fatal("finding silently linked to an invisible asset")
		}
		saved, err := d.GetFinding(output.FindingID)
		if err != nil || saved == nil || len(saved.AssetIDs) != 0 || saved.Evidence != "fixture evidence stays available" {
			t.Fatalf("finding body/asset isolation changed: %+v %v", saved, err)
		}
		visible, err := assets.GetByIDsInScope(task.ID, []int64{foreign})
		if err != nil || len(visible) != 0 {
			t.Fatalf("recording a lead granted visibility: count=%d err=%v", len(visible), err)
		}
	}
	var anchors int
	if err := d.QueryRow(`SELECT count(*) FROM exploration_anchors anchor JOIN exploration_nodes node ON node.id=anchor.node_id WHERE node.exploration_id=$1 AND anchor.asset_id=$2`, task.ExplorationID, foreign).Scan(&anchors); err != nil || anchors != 0 {
		t.Fatalf("rejected references created anchors: count=%d err=%v", anchors, err)
	}
	// An explicit operator opt-in still allows actual model references; the
	// restriction is not a blanket ban on task reuse of global assets.
	enabled := true
	if _, err := d.UpdateTask(task.ID, db.TaskPatch{ShareGlobalAssets: &enabled}); err != nil {
		t.Fatal(err)
	}
	if _, err := tools.addOneHint(hintItem{Text: "explicit sharing allows reference", AssetIDs: []json.RawMessage{rawID, rawID}}); err != nil {
		t.Fatalf("explicit opt-in rejected a valid reference: %v", err)
	}
}
