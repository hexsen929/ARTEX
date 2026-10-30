package server

import "testing"

func TestFindingVerifyAutoEnabled(t *testing.T) {
	// Absence is normalized to auto by the database reader. Unknown persisted
	// modes must not be treated as a decision to start automatic verification.
	if !findingVerifyAutoEnabled("auto") {
		t.Error("known auto mode should enable auto-verification")
	}
	for _, mode := range []string{"off", "", "AUTO", "whatever", " auto "} {
		if findingVerifyAutoEnabled(mode) {
			t.Errorf("unknown/disabled mode %q started automatic verification", mode)
		}
	}
}
