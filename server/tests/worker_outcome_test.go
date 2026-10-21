//go:build ignore

package server

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/Autumn-27/norma/harness"
	"github.com/Autumn-27/norma/llm"
)

func TestWorkerOutcomeSeparatesCheckpointAndModelDeadlines(t *testing.T) {
	for _, tc := range []struct {
		name        string
		reason      harness.TerminalReason
		err         error
		completing  bool
		state, kind string
	}{
		{"normal", harness.ReasonCompleted, nil, false, "done", "none"},
		{"time_budget", harness.ReasonTimeout, nil, false, "exhausted", "none"},
		{"turn_budget", harness.ReasonMaxTurns, nil, false, "exhausted", "none"},
		{"checkpoint_deadline", harness.ReasonTimeout, context.DeadlineExceeded, false, "exhausted", "settlement_timeout"},
		{"wrapped_checkpoint_deadline", harness.ReasonMaxTurns, fmt.Errorf("checkpoint: %w", context.DeadlineExceeded), false, "exhausted", "settlement_timeout"},
		{"completion_checkpoint_deadline", harness.ReasonSettlementRequested, context.DeadlineExceeded, true, "exhausted", "settlement_timeout"},
		{"real_provider_deadline", harness.ReasonModelError, context.DeadlineExceeded, false, "blocked", "transport"},
		{"real_provider_503", harness.ReasonModelError, &llm.HTTPError{StatusCode: 503}, false, "blocked", "transport"},
		{"real_error_on_budget", harness.ReasonTimeout, errors.New("independent failure"), false, "blocked", "unknown"},
		{"bad_request", harness.ReasonModelError, &llm.HTTPError{StatusCode: 400}, false, "blocked", "protocol"},
		{"completion_control", harness.ReasonCompleted, nil, true, "exhausted", "none"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			state, failure := workerOutcome(tc.reason, tc.err, tc.completing)
			if state != tc.state || failure.Kind != tc.kind {
				t.Fatalf("state=%s failure=%+v", state, failure)
			}
			if tc.kind == "settlement_timeout" && failure.Action != "review_saved_checkpoint" {
				t.Fatal(failure)
			}
		})
	}
}
