//go:build ignore

package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Autumn-27/artex/db"
	"github.com/Autumn-27/artex/intercept"
)

func testCaseServerRuoJi6(t *testing.T) (*Server, int64, []int64) {
	t.Helper()
	dsn, _, err := db.DSN()
	if err != nil {
		t.Fatal(err)
	}
	pg, err := db.Open(dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { pg.Close() })
	task, err := pg.CreateTask("case server", "goal", nil, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	ids := []int64{}
	for _, sev := range []string{"high", "medium", "low"} {
		id, err := pg.AddFinding(task.ID, 0, "IDOR", "orders", sev, "same endpoint", "proof "+sev, "worker", nil)
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}
	t.Cleanup(func() { _, _ = pg.Exec(`DELETE FROM findings WHERE task_id=$1`, task.ID); _ = pg.DeleteTask(task.ID) })
	return &Server{m: &Manager{pg: pg}}, task.ID, ids
}
func TestFindingCaseAPIPaginationAndToolsRuoJi6(t *testing.T) {
	s, task, ids := testCaseServerRuoJi6(t)
	ctx := context.Background()
	a := findingCaseRequest{TaskID: json.RawMessage(fmt.Sprint(task)), Title: "订单越权", Reason: "same check"}
	for _, id := range ids {
		a.IDs = append(a.IDs, json.RawMessage(fmt.Sprint(id)))
	}
	result, err := s.performFindingCaseTool(ctx, "merge_finding_records", a)
	if err != nil {
		t.Fatal(err)
	}
	cid := result.(map[string]string)["case_id"]
	r := httptest.NewRequest("GET", fmt.Sprintf("/api/exploration/finding-cases?task_id=%d&severity=low&page=1&limit=1", task), nil)
	w := httptest.NewRecorder()
	s.listFindingCases(w, r)
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body)
	}
	var page struct {
		Total int `json:"total"`
		Items []struct {
			Case    db.FindingCase `json:"case"`
			Matched []int64        `json:"matched_ids"`
		} `json:"items"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &page); err != nil {
		t.Fatal(err)
	}
	if page.Total != 1 || page.Items[0].Case.Count != 3 || page.Items[0].Case.Report != "" || len(page.Items[0].Matched) != 1 {
		t.Fatalf("page %+v", page)
	}
	r = httptest.NewRequest("GET", "/", nil)
	r.SetPathValue("id", cid)
	w = httptest.NewRecorder()
	s.getFindingCaseMembers(w, r)
	if w.Code != 200 || !strings.Contains(w.Body.String(), "proof high") {
		t.Fatal(w.Code, w.Body)
	}
	a.CaseID = json.RawMessage(cid)
	result, err = s.performFindingCaseTool(ctx, "get_finding_case", a)
	if err != nil {
		t.Fatal(err)
	}
	c := result.(map[string]any)["case"].(*db.FindingCase)
	a.Version = c.Version
	a.Report = completeFindingReviewReport
	a.Severity = "high"
	a.SeverityReason = "verified data disclosure"
	if _, err := s.performFindingCaseTool(ctx, "update_finding_case_report", a); err != nil {
		t.Fatal(err)
	}
	if _, err := s.m.pg.Exec(`UPDATE findings SET evidence='new data' WHERE id=$1`, ids[1]); err != nil {
		t.Fatal(err)
	}
	if _, err := s.performFindingCaseTool(ctx, "update_finding_case_report", a); err != db.ErrFindingCaseConflict {
		t.Fatalf("stale report accepted %v", err)
	}
}

func TestFindingCaseSelectedReviewCannotRewriteOtherOriginalRuoJi6(t *testing.T) {
	s, task, ids := testCaseServerRuoJi6(t)
	conv, err := s.m.pg.CreateConversation("reporter", "review", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer s.m.pg.Exec(`DELETE FROM conversations WHERE id=$1`, conv.ID)
	raw, _ := json.Marshal(ids[:1])
	if _, err := s.m.pg.Exec(`INSERT INTO finding_case_review_runs(conversation_id,task_id,finding_ids) VALUES($1,$2,$3)`, conv.ID, task, string(raw)); err != nil {
		t.Fatal(err)
	}
	ctx := intercept.WithConvID(context.Background(), conv.ID)
	if err := s.checkCaseReviewScope(ctx, "update_finding_report", findingCaseRequest{FindingID: json.RawMessage(fmt.Sprint(ids[1]))}); err == nil {
		t.Fatal("unselected original allowed")
	}
	if err := s.checkCaseReviewScope(ctx, "update_finding_report", findingCaseRequest{FindingID: json.RawMessage(fmt.Sprint(ids[0]))}); err == nil {
		t.Fatal("selected original was allowed to be overwritten")
	}
}

func TestFindingCaseListIncludesContextAndFilteredCounts(t *testing.T) {
	s, task, ids := testCaseServerRuoJi6(t)
	cid, err := s.m.pg.MergeFindingCase(t.Context(), task, ids[:3], "same defect", "verified root cause", "human")
	if err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest("GET", fmt.Sprintf("/api/finding-cases?task_id=%d&severity=medium", task), nil)
	w := httptest.NewRecorder()
	s.listFindingCases(w, r)
	if w.Code != 200 {
		t.Fatalf("status %d %s", w.Code, w.Body.String())
	}
	var page struct {
		Total           int `json:"total"`
		MatchingReports int `json:"matching_reports"`
		Items           []struct {
			Case            *db.FindingCase   `json:"case"`
			TaskDescription string            `json:"task_description"`
			Assets          []FindingAssetDTO `json:"assets"`
			AssetCount      int               `json:"asset_count"`
			LastFoundAt     string            `json:"last_found_at"`
		} `json:"items"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &page); err != nil {
		t.Fatal(err)
	}
	if page.Total != 1 || page.MatchingReports != 1 || len(page.Items) != 1 || page.Items[0].Case.ID != cid || page.Items[0].Case.Count != 3 || page.Items[0].Case.Report != "" || page.Items[0].TaskDescription == "" || page.Items[0].LastFoundAt == "" || page.Items[0].Assets == nil {
		t.Fatalf("list payload %s", w.Body.String())
	}
}

func TestOriginalFindingRowsKeepEvidenceAndOmitReportBody(t *testing.T) {
	s, task, ids := testCaseServerRuoJi6(t)
	if _, err := s.m.pg.Exec(`UPDATE findings SET report=$2 WHERE id=$1`, ids[0], "large original report"); err != nil {
		t.Fatal(err)
	}
	for _, original := range []bool{false, true} {
		suffix := ""
		if original {
			suffix = "&original_rows=1"
		}
		r := httptest.NewRequest("GET", fmt.Sprintf("/api/finding-cases?task_id=%d%s", task, suffix), nil)
		w := httptest.NewRecorder()
		s.listFindingCases(w, r)
		if w.Code != 200 {
			t.Fatalf("status %d %s", w.Code, w.Body)
		}
		var page struct {
			Items []struct {
				Finding FindingDTO `json:"finding"`
			} `json:"items"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &page); err != nil {
			t.Fatal(err)
		}
		if len(page.Items) != 3 {
			t.Fatalf("lost original rows %s", w.Body)
		}
		for _, row := range page.Items {
			if row.Finding.Report != "" {
				t.Fatal("list loaded report body")
			}
			if original && !strings.HasPrefix(row.Finding.Evidence, "proof ") {
				t.Fatal("original table evidence missing")
			}
			if !original && row.Finding.Evidence != "" {
				t.Fatal("compact list should omit evidence")
			}
		}
	}
}
