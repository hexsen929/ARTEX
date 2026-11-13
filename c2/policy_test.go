package c2

import "testing"

func TestOperatorOriginalTaskApprovalPolicy(t *testing.T) {
	for _, kind := range []string{"upload", "download", "persist", "selfdel", "exit", "kill", " UPLOAD "} {
		if !TaskNeedsApproval(kind) {
			t.Fatalf("approval missing for %q", kind)
		}
	}
	for _, kind := range []string{"shell", "sleep", "", "unknown"} {
		if TaskNeedsApproval(kind) {
			t.Fatalf("unexpected type-based approval for %q", kind)
		}
	}
}
