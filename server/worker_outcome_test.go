package server

import (
	"context"
	"errors"
	"testing"

	"github.com/Autumn-27/norma/harness"
)

func TestWorkerOutcomeClassification(t *testing.T) {
	for _, tc := range []struct {
		reason      harness.TerminalReason
		err         error
		state, code string
	}{
		{harness.ReasonCompleted, nil, "done", "completed"},
		{harness.ReasonTimeout, context.DeadlineExceeded, "exhausted", "run_budget"},
		{harness.ReasonMaxTurns, nil, "exhausted", "turn_budget"},
		{harness.ReasonTimeout, errors.New("status 503: gateway"), "blocked", "provider_failure"},
		{harness.ReasonModelError, errors.New("status 503: model_not_found No available channel"), "blocked", "model_unavailable"},
		{harness.ReasonBlockingLimit, nil, "blocked", "context_limit"},
		{harness.ReasonHookStopped, nil, "blocked", "guard_stopped"},
	} {
		state, code, msg := workerOutcome(tc.reason, tc.err)
		if state != tc.state || code != tc.code || msg == "" {
			t.Errorf("%s: %s %s %q", tc.reason, state, code, msg)
		}
	}
}
func TestDeterministicModelErrorsDoNotReplayWorkers(t *testing.T) {
	for _, s := range []string{"status 401: bad key", "status 503: model_not_found", "context_length_exceeded", "retry_budget_exhausted"} {
		if retryableWorkerModelError(harness.ReasonModelError, errors.New(s)) {
			t.Errorf("replayed %s", s)
		}
	}
}
