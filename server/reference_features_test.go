package server

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"fmt"
	"testing"

	"github.com/Autumn-27/artex/knowledge"
	"github.com/Autumn-27/artex/report"
)

func TestReferenceFeaturesHTTPDocumentsReproductionAndWord(t *testing.T) {
	s, finding, request := trafficEvidenceServer(t)
	for _, path := range []string{"/api/knowledge", "/api/knowledge?task_id=0"} {
		w := request("GET", path, "")
		if w.Code != 200 {
			t.Fatalf("global listing %s: %d %s", path, w.Code, w.Body)
		}
	}
	docBody := fmt.Sprintf(`{"title":"HTTP reference fixture","content":"REFERENCE_HTTP_CANARY","task_id":%d}`, findingTaskID(t, s, finding.FindingID))
	w := request("POST", "/api/knowledge", docBody)
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body)
	}
	var doc knowledge.Document
	if json.Unmarshal(w.Body.Bytes(), &doc) != nil {
		t.Fatal(w.Body)
	}
	t.Cleanup(func() { s.m.pg.Exec(`DELETE FROM knowledge_docs WHERE id=$1`, doc.ID) })
	w = request("GET", "/api/knowledge?task_id=0", "")
	if w.Code != 200 || bytes.Contains(w.Body.Bytes(), []byte(`HTTP reference fixture`)) {
		t.Fatal("global list included task-owned document", w.Code, w.Body)
	}
	w = request("POST", "/api/knowledge/search", fmt.Sprintf(`{"query":"REFERENCE_HTTP_CANARY","task_id":%d}`, doc.TaskID))
	if w.Code != 200 || !bytes.Contains(w.Body.Bytes(), []byte("REFERENCE_HTTP_CANARY")) {
		t.Fatal(w.Code, w.Body)
	}
	w = request("POST", "/api/knowledge/search", `{"query":"REFERENCE_HTTP_CANARY","task_id":0}`)
	if w.Code != 200 || bytes.Contains(w.Body.Bytes(), []byte("REFERENCE_HTTP_CANARY")) {
		t.Fatal("task content crossed global scope", w.Code, w.Body)
	}
	w = request("PUT", fmt.Sprintf("/api/knowledge/%d", doc.ID), fmt.Sprintf(`{"title":"stale","content":"keep draft","task_id":%d,"version":0}`, doc.TaskID))
	if w.Code != 409 {
		t.Fatal("stale update", w.Code, w.Body)
	}
	stored, err := s.m.pg.GetFinding(finding.FindingID)
	if err != nil {
		t.Fatal(err)
	}
	w = request("PUT", fmt.Sprintf("/api/exploration/findings/%d/reproduction", stored.ID), fmt.Sprintf(`{"evidence_version":%d,"reproduction":{"entry_point":"fixture","steps":[{"title":"observed","request":"GET /synthetic","response":"saved evidence"}]}}`, stored.EvidenceVersion))
	if w.Code != 200 || !bytes.Contains(w.Body.Bytes(), []byte(`"reproduction"`)) {
		t.Fatal(w.Code, w.Body)
	}
	template := report.Template{Name: "HTTP fixture", Locale: "zh-CN", Title: "Synthetic report"}
	raw, _ := json.Marshal(template)
	w = request("POST", "/api/report-templates", string(raw))
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body)
	}
	if json.Unmarshal(w.Body.Bytes(), &template) != nil {
		t.Fatal(w.Body)
	}
	t.Cleanup(func() { s.m.pg.Exec(`DELETE FROM report_templates WHERE id=$1`, template.ID) })
	stored, err = s.m.pg.GetFinding(finding.FindingID)
	if err != nil {
		t.Fatal(err)
	}
	body := fmt.Sprintf(`{"template_id":%d,"template_version":%d,"include_unverified":true,"findings":[{"id":%d,"evidence_version":%d}]}`, template.ID, template.Version, stored.ID, stored.EvidenceVersion)
	w = request("POST", "/api/reports/docx", body)
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body)
	}
	z, err := zip.NewReader(bytes.NewReader(w.Body.Bytes()), int64(w.Body.Len()))
	if err != nil || len(z.File) < 5 {
		t.Fatal("invalid Word package", err)
	}
	stale := fmt.Sprintf(`{"template_id":%d,"template_version":%d,"include_unverified":true,"findings":[{"id":%d,"evidence_version":%d}]}`, template.ID, template.Version, stored.ID, stored.EvidenceVersion-1)
	w = request("POST", "/api/reports/docx", stale)
	if w.Code != 409 {
		t.Fatal("stale evidence exported", w.Code)
	}
}
func findingTaskID(t *testing.T, s *Server, id int64) int64 {
	t.Helper()
	f, err := s.m.pg.GetFinding(id)
	if err != nil || f == nil || f.TaskID == nil {
		t.Fatal("missing fixture task", err)
	}
	return *f.TaskID
}
