package server

import (
	"encoding/json"
	"fmt"
	"testing"

	"github.com/Autumn-27/artex/db"
)

func TestYMHInlineHistoryUsesStandaloneFindingIDs(t *testing.T) {
	s, finding, req := trafficEvidenceServer(t)
	url := fmt.Sprintf("/api/findings/%d/inline-history", finding.FindingID)
	w := req("GET", url, "")
	if w.Code != 200 {
		t.Fatalf("empty history %d: %s", w.Code, w.Body.String())
	}
	var result struct {
		Events []struct {
			ID        string `json:"id"`
			FindingID string `json:"finding_id"`
			Kind      string `json:"kind"`
		} `json:"events"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil || len(result.Events) != 0 {
		t.Fatalf("unexpected history: %s (%v)", w.Body.String(), err)
	}
	if _, err := s.m.pg.Exec(`INSERT INTO finding_inline_merge_events(finding_id,kind,payload) VALUES($1,'original',$2::jsonb)`, finding.FindingID, `{"title":"fixture","evidence":"original evidence"}`); err != nil {
		t.Fatal(err)
	}
	w = req("GET", url, "")
	if w.Code != 200 {
		t.Fatalf("history %d: %s", w.Code, w.Body.String())
	}
	if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil || len(result.Events) != 1 || result.Events[0].ID == "" || result.Events[0].FindingID != fmt.Sprint(finding.FindingID) || result.Events[0].Kind != "original" {
		t.Fatalf("lost standalone/string ids: %s (%v)", w.Body.String(), err)
	}
	if w = req("GET", "/api/findings/not-an-id/inline-history", ""); w.Code != 400 {
		t.Fatalf("invalid id: %d", w.Code)
	}
	if w = req("GET", "/api/findings/9223372036854775807/inline-history", ""); w.Code != 404 {
		t.Fatalf("missing id: %d %s", w.Code, w.Body.String())
	}
}

func TestYMHArchiveKeyRequiresOriginalInstanceForEncryptedRows(t *testing.T) {
	s := &Server{jwtKey: []byte("fixture-original-instance-master-key")}
	legacy := &db.TaskArchiveSnapshot{FormatVersion: 4, Tables: map[string]json.RawMessage{"findings": json.RawMessage(`[]`)}}
	if err := s.validateArchiveIntranetKey(legacy); err != nil {
		t.Fatalf("legacy package rejected: %v", err)
	}
	snapshot := &db.TaskArchiveSnapshot{Tables: map[string]json.RawMessage{"sessions": json.RawMessage(`[{"id":1,"secret_enc":"fixture-encrypted-record"}]`)}}
	if err := s.validateArchiveIntranetKey(snapshot); err == nil {
		t.Fatal("encrypted archive without key identity accepted")
	}
	snapshot.IntranetKeyFingerprint = db.ArchiveIntranetKeyFingerprint(s.jwtKey)
	if err := s.validateArchiveIntranetKey(snapshot); err != nil {
		t.Fatalf("matching original key rejected: %v", err)
	}
	s.jwtKey = []byte("fixture-other-instance-master-key")
	if err := s.validateArchiveIntranetKey(snapshot); err == nil {
		t.Fatal("different key accepted")
	}
	s.jwtKey = nil
	if err := s.validateArchiveIntranetKey(snapshot); err == nil {
		t.Fatal("missing key accepted")
	}
}
