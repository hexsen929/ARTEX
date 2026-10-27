package agent

import (
	"testing"

	"github.com/Autumn-27/artex/db"
)

// Only independent confirmed verification counts as proof; uncertain/history
// findings stay recorded without being promoted by their triage/node state.
func TestProvenFinding(t *testing.T) {
	if provenFinding("") {
		t.Error("missing verification must not count as a confirmed breakthrough")
	}
	if !provenFinding(db.FindingConfirmed) {
		t.Error("confirmed should be countable")
	}
	for _, st := range []string{db.FindingPending, db.FindingFalsePositive, db.FindingFixed, db.FindingIgnored, db.FindingDuplicate, db.VerificationUnverified, db.VerificationBlocked, db.VerificationRejected, db.VerificationLegacy} {
		if provenFinding(st) {
			t.Errorf("status %q should not count as a proven finding", st)
		}
	}
}
