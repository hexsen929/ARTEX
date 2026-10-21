package report

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/Autumn-27/artex/db"
)

func TestVerificationPreservedAcrossExports(t *testing.T) {
	var fs []*db.DBFinding
	var ns []*db.Node
	for i, status := range []string{"confirmed", "unverified", "blocked", "rejected", "legacy"} {
		f := &db.DBFinding{ID: int64(i + 1), Severity: "high", Status: "pending", Summary: status, VerificationStatus: status, VerificationNote: "fixture verification note"}
		fs = append(fs, f)
		p, _ := json.Marshal(map[string]any{"summary": status, "severity": "high", "verification_status": status, "verification_note": f.VerificationNote})
		ns = append(ns, &db.Node{Payload: p})
		if md := SingleFindingMarkdown(f, time.Now()); !strings.Contains(md, db.VerificationLabel(status)) || !strings.Contains(md, "fixture verification note") {
			t.Fatal("single export lost metadata")
		}
	}
	for _, out := range []string{FindingsMarkdown(fs, time.Now()), string(FindingsCSV(fs)), Markdown(Input{Findings: ns})} {
		for _, label := range []string{"已确认", "待验证", "验证受阻", "已排除", "历史未复核"} {
			if !strings.Contains(out, label) {
				t.Fatalf("missing %s", label)
			}
		}
	}
	md := Markdown(Input{Findings: ns})
	if !strings.Contains(md, "已确认：1；待验证：1") {
		t.Fatalf("inflated confirmed count: %s", md)
	}
}
