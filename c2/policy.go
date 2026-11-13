package c2

import "strings"

// TaskNeedsApproval matches the original runtime's type-based decision. Shell
// and sleep are not automatically escalated merely because the source is AI;
// the caller's existing tool guard remains responsible for command policy.
func TaskNeedsApproval(kind string) bool {
	switch strings.ToLower(strings.TrimSpace(kind)) {
	case "upload", "download", "persist", "selfdel", "exit", "kill":
		return true
	default:
		return false
	}
}
