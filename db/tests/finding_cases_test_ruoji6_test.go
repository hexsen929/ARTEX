//go:build ignore

package db

import (
	"encoding/json"
	"fmt"
	"reflect"
	"testing"
)

func caseTestDBRuoJi6(t *testing.T) (*DB, *Task, []int64) {
	t.Helper()
	d, err := Open(testDSN(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	task, err := d.CreateTask("case integration", "goal", nil, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	ids := []int64{}
	for _, sev := range []string{"high", "medium", "low", "high"} {
		id, err := d.AddFinding(task.ID, 0, "IDOR", "order access", sev, "GET /orders/{id}", "original PoC "+sev, "worker", nil)
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}
	t.Cleanup(func() { _, _ = d.Exec(`DELETE FROM findings WHERE task_id=$1`, task.ID); _ = d.DeleteTask(task.ID) })
	return d, task, ids
}

func TestFindingCaseViewContextAndAssetFiltering(t *testing.T) {
	d, task, ids := caseTestDBRuoJi6(t)
	if _, err := d.Exec(`UPDATE tasks SET name='Order API' WHERE id=$1`, task.ID); err != nil {
		t.Fatal(err)
	}
	assets := []int64{}
	for i := 0; i < 6; i++ {
		aid := seedTreeAsset(t, d, "service", map[string]any{"url": fmt.Sprintf("https://view-context-%d-%d.example", task.ID, i)})
		assets = append(assets, aid)
	}
	t.Cleanup(func() {
		for _, a := range assets {
			_, _ = d.Exec(`DELETE FROM assets WHERE id=$1`, a)
		}
	})
	for i, id := range ids[:3] {
		raw, _ := json.Marshal(assets[i*2 : i*2+2])
		if _, err := d.Exec(`UPDATE findings SET asset_ids=$2 WHERE id=$1`, id, string(raw)); err != nil {
			t.Fatal(err)
		}
	}
	cid, err := d.MergeFindingCase(t.Context(), task.ID, ids[:3], "order access", "same permission check", "human")
	if err != nil {
		t.Fatal(err)
	}
	contexts, err := d.FindingCaseListContexts([]int64{cid})
	if err != nil {
		t.Fatal(err)
	}
	c := contexts[cid]
	if c.TaskName != "Order API" || c.TaskDescription != task.Description || c.AssetCount != 6 || !reflect.DeepEqual(c.AssetIDs, assets[:4]) || c.LastFoundAt.IsZero() {
		t.Fatalf("context %+v", c)
	}
	filter := FindingFilter{TaskID: fmt.Sprint(task.ID), AssetScope: fmt.Sprintf("a:%d", assets[0])}
	rows, total, err := d.ListFindingCases(filter, 1, 20)
	if err != nil || total != 1 || len(rows) != 1 || rows[0].Case == nil || rows[0].Case.ID != cid || !reflect.DeepEqual(rows[0].MatchedIDs, ids[:1]) {
		t.Fatalf("filtered rows %+v total %d err %v", rows, total, err)
	}
	members, n, err := d.FindingCaseMembers(cid, 1, 20)
	if err != nil || n != 3 || len(members) != 3 {
		t.Fatalf("all folder members %d %v", n, err)
	}
	matches, err := d.MatchingFindingReportCount(filter)
	if err != nil || matches != 1 {
		t.Fatalf("matching reports %d %v", matches, err)
	}
	rows, total, err = d.ListFindingCases(FindingFilter{TaskID: fmt.Sprint(task.ID), Severity: "medium"}, 1, 20)
	if err != nil || total != 1 || len(rows) != 1 || rows[0].Case.Count != 3 || rows[0].Case.High != 1 || rows[0].Case.Medium != 1 || rows[0].Case.Low != 1 {
		t.Fatalf("severity filter %+v %v", rows, err)
	}
	matches, err = d.MatchingFindingReportCount(FindingFilter{TaskID: fmt.Sprint(task.ID), Severity: "medium"})
	if err != nil || matches != 1 {
		t.Fatalf("severity reports %d %v", matches, err)
	}
	// Group has no unified severity yet; it must sort by its original high report.
	if _, err := d.Exec(`UPDATE findings SET severity='low' WHERE id=$1`, ids[3]); err != nil {
		t.Fatal(err)
	}
	rows, total, err = d.ListFindingCases(FindingFilter{TaskID: fmt.Sprint(task.ID), Sort: "severity"}, 1, 1)
	if err != nil || total != 2 || len(rows) != 1 || rows[0].Case == nil || rows[0].Case.ID != cid {
		t.Fatalf("original severity order %+v %v", rows, err)
	}
}
