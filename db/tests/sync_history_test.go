//go:build ignore

package db

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

func syncTestRun(t *testing.T, d *DB, sourceID *int64) *SyncRun {
	t.Helper()
	run, err := d.CreateSyncRun(t.Context(), sourceID, json.RawMessage(`{"dimension":"task","targets":["test"],"asset_types":["app"]}`), json.RawMessage(`{"status":"running","errors":[]}`))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = d.Exec(`DELETE FROM sync_runs WHERE id=$1`, run.ID) })
	return run
}

func TestSyncObservationAtomicMutationAndCounters(t *testing.T) {
	d, _, _ := testSetup(t)
	t.Cleanup(func() { _ = d.Close() })
	run := syncTestRun(t, d, nil)
	if err := d.AddSyncFetched(t.Context(), run.ID, 2); err != nil {
		t.Fatal(err)
	}
	name := fmt.Sprintf("sync-atomic-%d", time.Now().UnixNano())
	item := &SyncObservation{RunID: run.ID, Target: "test", AssetType: "app", Status: "written", Raw: json.RawMessage(`{"name":"test","id":"source-1"}`)}
	err := d.ApplySyncObservation(t.Context(), item, func(as *AssetStore) error {
		id, err := as.UpsertApp(UpsertAppReq{Name: name})
		item.AssetID = &id
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = d.Exec(`DELETE FROM assets WHERE app_name=$1`, name) })
	if item.Outcome != "created" || len(item.AssetChanges) != 1 || !item.AssetChanges[0].Direct {
		t.Fatalf("first write did not record creation: %+v", item)
	}
	stored, err := d.GetSyncRun(t.Context(), run.ID)
	if err != nil {
		t.Fatal(err)
	}
	var result struct {
		Stats  SyncStats      `json:"stats"`
		Synced map[string]int `json:"synced"`
	}
	if err := json.Unmarshal(stored.Result, &result); err != nil || result.Stats.Written != 1 || result.Stats.Fetched != 2 || result.Synced["app"] != 1 {
		t.Fatalf("stored result=%s error=%v", stored.Result, err)
	}
	// Repeating identical source data is a successful operation, but only a
	// last_seen update; it must be classified as unchanged, with one unique asset.
	item.ID, item.ItemIndex = 0, 1
	err = d.ApplySyncObservation(t.Context(), item, func(as *AssetStore) error {
		id, err := as.UpsertApp(UpsertAppReq{Name: name})
		if err == nil && id != *item.AssetID {
			t.Fatalf("repeat changed canonical asset id: %d -> %d", *item.AssetID, id)
		}
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	if item.Outcome != "unchanged" {
		t.Fatalf("repeat reported as %q", item.Outcome)
	}
	var assets int
	if err := d.QueryRow(`SELECT count(*) FROM assets WHERE app_name=$1`, name).Scan(&assets); err != nil || assets != 1 {
		t.Fatalf("asset count=%d error=%v", assets, err)
	}
	items, total, err := d.ListSyncObservations(t.Context(), run.ID, 20, 0, false)
	if err != nil || total != 2 || len(items) != 2 || len(items[0].Raw) != 0 {
		t.Fatalf("history total=%d items=%+v error=%v", total, items, err)
	}
	stored, err = d.GetSyncRun(t.Context(), run.ID)
	if err != nil {
		t.Fatal(err)
	}
	var counted struct {
		Stats  SyncStats        `json:"stats"`
		Unique SyncUniqueAssets `json:"unique_assets"`
	}
	if err := json.Unmarshal(stored.Result, &counted); err != nil || counted.Stats.Created != 1 || counted.Stats.Unchanged != 1 || counted.Unique.Total != 1 || counted.Unique.Created != 1 {
		t.Fatalf("repeat counts=%s error=%v", stored.Result, err)
	}
}

func TestSyncTracksUpdatedContentAndDerivedAssets(t *testing.T) {
	d, _, _ := testSetup(t)
	t.Cleanup(func() { _ = d.Close() })
	run := syncTestRun(t, d, nil)
	host := fmt.Sprintf("host.sync-%d.test", time.Now().UnixNano())
	t.Cleanup(func() {
		_, _ = d.Exec(`DELETE FROM assets WHERE domain=$1 OR root_domain=$2 OR (type='ip' AND ip='192.0.2.178')`, host, strings.TrimPrefix(host, "host."))
	})
	url := "https://" + host
	for index, title := range []string{"original", "updated", "updated"} {
		item := &SyncObservation{RunID: run.ID, Target: "test", AssetType: "service", ItemIndex: index, Status: "written"}
		if err := d.ApplySyncObservation(t.Context(), item, func(as *AssetStore) error {
			id, err := as.UpsertHTTPService(UpsertHTTPServiceReq{URL: url, IP: "192.0.2.178", PageTitle: title, Technologies: []string{"nginx", "linux"}})
			item.AssetID = &id
			return err
		}); err != nil {
			t.Fatal(err)
		}
		want := []string{"created", "updated", "unchanged"}[index]
		if item.Outcome != want || len(item.AssetChanges) != 4 {
			t.Fatalf("observation=%+v want outcome=%s with 4 assets", item, want)
		}
	}
	stored, err := d.GetSyncRun(t.Context(), run.ID)
	if err != nil {
		t.Fatal(err)
	}
	var counted struct {
		Stats  SyncStats        `json:"stats"`
		Unique SyncUniqueAssets `json:"unique_assets"`
	}
	if err := json.Unmarshal(stored.Result, &counted); err != nil || counted.Stats.Created != 1 || counted.Stats.Updated != 1 || counted.Stats.Unchanged != 1 || counted.Unique.Total != 4 || counted.Unique.Direct != 1 || counted.Unique.Derived != 3 {
		t.Fatalf("counts=%s error=%v", stored.Result, err)
	}
}

func TestSyncMutationFailureAndInactiveRunRollBackAssets(t *testing.T) {
	d, _, _ := testSetup(t)
	t.Cleanup(func() { _ = d.Close() })
	run := syncTestRun(t, d, nil)
	name := fmt.Sprintf("sync-rollback-%d", time.Now().UnixNano())
	cause := errors.New("derived write rejected")
	item := &SyncObservation{RunID: run.ID, Target: "test", AssetType: "app", Status: "written", Raw: json.RawMessage(`{"name":"test"}`)}
	err := d.ApplySyncObservation(t.Context(), item, func(as *AssetStore) error {
		_, err := as.UpsertApp(UpsertAppReq{Name: name})
		if err != nil {
			return err
		}
		return cause
	})
	if !errors.Is(err, cause) {
		t.Fatalf("mutation error=%v", err)
	}
	var assets int
	if err := d.QueryRow(`SELECT count(*) FROM assets WHERE app_name=$1`, name).Scan(&assets); err != nil || assets != 0 {
		t.Fatalf("rolled-back asset exists: count=%d error=%v", assets, err)
	}
	item.Status, item.Reason = "failed", cause.Error()
	if err := d.RecordSyncObservation(t.Context(), item); err != nil {
		t.Fatal(err)
	}
	if err := d.UpdateSyncRun(t.Context(), run.ID, "failed", json.RawMessage(`{"status":"failed"}`)); err != nil {
		t.Fatal(err)
	}
	item.ItemIndex, item.Status = 1, "written"
	err = d.ApplySyncObservation(t.Context(), item, func(as *AssetStore) error {
		_, err := as.UpsertApp(UpsertAppReq{Name: name})
		return err
	})
	if !errors.Is(err, ErrSyncRunInactive) {
		t.Fatalf("inactive run accepted late mutation: %v", err)
	}
	if err := d.QueryRow(`SELECT count(*) FROM assets WHERE app_name=$1`, name).Scan(&assets); err != nil || assets != 0 {
		t.Fatalf("late asset survived rollback: count=%d error=%v", assets, err)
	}
	if err := d.UpdateSyncRun(t.Context(), run.ID, "success", json.RawMessage(`{}`)); !errors.Is(err, ErrSyncRunInactive) {
		t.Fatalf("late success replaced final state: %v", err)
	}
}

func TestSyncHistorySurvivesAssetAndSourceDeletion(t *testing.T) {
	d, _, _ := testSetup(t)
	t.Cleanup(func() { _ = d.Close() })
	stamp := time.Now().UnixNano()
	sourceID, err := d.SaveMCP(&MCPServer{Name: fmt.Sprintf("sync-history-%d", stamp), Transport: "http", URL: "http://127.0.0.1.invalid"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.DeleteMCP(sourceID) })
	run := syncTestRun(t, d, &sourceID)
	name := fmt.Sprintf("sync-source-%d", stamp)
	item := &SyncObservation{RunID: run.ID, Target: "test", SourceTargetID: "stable-task", SourceAssetID: "stable-asset", AssetType: "app", Status: "written", Raw: json.RawMessage(`{"name":"test","source":"original"}`)}
	if err := d.ApplySyncObservation(t.Context(), item, func(as *AssetStore) error {
		id, err := as.UpsertApp(UpsertAppReq{Name: name})
		item.AssetID = &id
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Assets().DeleteByIDs([]int64{*item.AssetID}); err != nil {
		t.Fatal(err)
	}
	if err := d.DeleteMCP(sourceID); err != nil {
		t.Fatal(err)
	}
	stored, err := d.GetSyncRun(t.Context(), run.ID)
	if err != nil || stored.SourceMCPID != nil {
		t.Fatalf("source deletion did not detach history: run=%+v error=%v", stored, err)
	}
	items, total, err := d.ListSyncObservations(t.Context(), run.ID, 20, 0, true)
	if err != nil || total != 1 || items[0].AssetID != nil || items[0].SourceAssetID != "stable-asset" || !strings.Contains(string(items[0].Raw), "original") {
		t.Fatalf("asset deletion lost provenance: items=%+v error=%v", items, err)
	}
}

func TestSyncObservationOmitsOversizedRawWithoutLosingReason(t *testing.T) {
	d, _, _ := testSetup(t)
	t.Cleanup(func() { _ = d.Close() })
	run := syncTestRun(t, d, nil)
	raw, _ := json.Marshal(map[string]string{"payload": strings.Repeat("x", SyncObservationRawLimit)})
	item := &SyncObservation{RunID: run.ID, Target: "test", AssetType: "app", Status: "skipped", Reason: "missing name", Raw: raw}
	if err := d.RecordSyncObservation(t.Context(), item); err != nil {
		t.Fatal(err)
	}
	items, _, err := d.ListSyncObservations(t.Context(), run.ID, 20, 0, true)
	if err != nil || len(items) != 1 || !items[0].RawOmitted || len(items[0].Raw) != 0 || items[0].Reason != "missing name" {
		t.Fatalf("oversized observation=%+v error=%v", items, err)
	}
}

func TestSyncConcurrentItemsShareOneCanonicalAsset(t *testing.T) {
	d, _, _ := testSetup(t)
	t.Cleanup(func() { _ = d.Close() })
	run := syncTestRun(t, d, nil)
	name := fmt.Sprintf("sync-concurrent-%d", time.Now().UnixNano())
	t.Cleanup(func() { _, _ = d.Exec(`DELETE FROM assets WHERE app_name=$1`, name) })
	results := make(chan error, 2)
	for i := range 2 {
		go func() {
			item := &SyncObservation{RunID: run.ID, Target: "test", AssetType: "app", ItemIndex: i, Status: "written"}
			results <- d.ApplySyncObservation(t.Context(), item, func(as *AssetStore) error {
				id, err := as.UpsertApp(UpsertAppReq{Name: name})
				item.AssetID = &id
				return err
			})
		}()
	}
	for range 2 {
		if err := <-results; err != nil {
			t.Fatal(err)
		}
	}
	stored, err := d.GetSyncRun(t.Context(), run.ID)
	if err != nil {
		t.Fatal(err)
	}
	var result struct {
		Stats  SyncStats        `json:"stats"`
		Unique SyncUniqueAssets `json:"unique_assets"`
	}
	if err := json.Unmarshal(stored.Result, &result); err != nil || result.Stats.Written != 2 || result.Stats.Created != 1 || result.Stats.Unchanged != 1 || result.Unique.Total != 1 {
		t.Fatalf("concurrent result=%s error=%v", stored.Result, err)
	}
}

func TestSyncRecoveryRebuildsTargetCountsFromCommittedItems(t *testing.T) {
	d, _, _ := testSetup(t)
	t.Cleanup(func() { _ = d.Close() })
	run := syncTestRun(t, d, nil)
	if err := d.UpdateSyncRun(t.Context(), run.ID, "running", json.RawMessage(`{"status":"running","targets":[{"target":"test","asset_type":"app","status":"running","stats":{"fetched":2,"written":0}}]}`)); err != nil {
		t.Fatal(err)
	}
	item := &SyncObservation{RunID: run.ID, Target: "test", AssetType: "app", Status: "skipped", Reason: "missing name"}
	if err := d.RecordSyncObservation(t.Context(), item); err != nil {
		t.Fatal(err)
	}
	if _, err := d.RecoverSyncRuns(t.Context()); err != nil {
		t.Fatal(err)
	}
	stored, err := d.GetSyncRun(t.Context(), run.ID)
	if err != nil {
		t.Fatal(err)
	}
	var result struct {
		Targets []struct {
			Stats SyncStats `json:"stats"`
		} `json:"targets"`
	}
	if err := json.Unmarshal(stored.Result, &result); err != nil || stored.Status != "interrupted" || len(result.Targets) != 1 || result.Targets[0].Stats.Skipped != 1 || result.Targets[0].Stats.Fetched != 2 {
		t.Fatalf("recovered=%+v result=%s error=%v", stored, stored.Result, err)
	}
}
