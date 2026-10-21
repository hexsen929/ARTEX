package server

import (
	"encoding/json"
	"fmt"
	"github.com/Autumn-27/artex/db"
	"strings"
	"testing"
)

func TestFindingVerificationAPIAndExports(t *testing.T) {
	s, f, req := trafficEvidenceServer(t)
	id := f.FindingID
	uri := fmt.Sprintf("/api/exploration/findings/%d/verification", id)
	// Missing evidence/basis must not silently promote a lead.
	if w := req("PATCH", uri, `{"status":"confirmed","note":"","version":0}`); w.Code != 400 {
		t.Fatalf("empty confirmation %d %s", w.Code, w.Body)
	}
	w := req("PATCH", uri, `{"status":"blocked","note":"fixture network unavailable; baseline missing","version":0}`)
	if w.Code != 200 {
		t.Fatalf("blocked %d %s", w.Code, w.Body)
	}
	var dto FindingDTO
	json.Unmarshal(w.Body.Bytes(), &dto)
	if dto.VerificationStatus != db.VerificationBlocked || dto.Severity != "low" || dto.Status != "pending" {
		t.Fatalf("lost metadata %+v", dto)
	}
	if w = req("PATCH", uri, `{"status":"confirmed","note":"proof","version":0}`); w.Code != 409 {
		t.Fatalf("stale update %d", w.Code)
	}
	w = req("PATCH", uri, `{"status":"confirmed","note":"fixture result satisfies condition","additional_evidence":"actual fixture output","version":1}`)
	if w.Code != 200 {
		t.Fatalf("confirm %d %s", w.Code, w.Body)
	}
	json.Unmarshal(w.Body.Bytes(), &dto)
	if dto.VerificationStatus != "confirmed" || !strings.Contains(dto.Evidence, "actual fixture output") {
		t.Fatalf("confirmation %+v", dto)
	}
	current, err := s.m.pg.GetFinding(id)
	if err != nil || current.EvidenceVersion != 2 {
		t.Fatalf("version %+v %v", current, err)
	}
	// No synthetic test rows are inserted into production; this fixture's server
	// uses the isolated test DB and an ephemeral data directory.
	filter := findingFilterFromQuery(map[string][]string{"verification": {"blocked"}})
	if filter.Verification != "blocked" {
		t.Fatal("query ignored verification")
	}
}
