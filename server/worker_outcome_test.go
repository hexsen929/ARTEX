package server

import (
	"context"
	"errors"
	"strings"
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
		{harness.ReasonModelError, errors.New("openai-responses: stream error"), "blocked", "unknown_failure"},
	} {
		state, code, msg := workerOutcome(tc.reason, tc.err)
		if state != tc.state || code != tc.code || msg == "" {
			t.Errorf("%s: %s %s %q", tc.reason, state, code, msg)
		}
	}
}

func TestWorkerOutcomeDoesNotInventRetryExhaustion(t *testing.T) {
	for _, tc := range []struct {
		err     error
		code    string
		message string
	}{
		{errors.New("openai-responses: stream error"), "unknown_failure", "具体原因尚未确定"},
		{errors.New("status 429: rate limit"), "rate_limited", "模型服务限流"},
		{errors.New("status 503: upstream error"), "provider_failure", "模型服务或中转返回错误"},
	} {
		state, code, message := workerOutcome(harness.ReasonModelError, tc.err)
		if state != "blocked" || code != tc.code || !strings.Contains(message, tc.message) {
			t.Errorf("outcome = %s %s %q", state, code, message)
		}
		for _, unsupported := range []string{"重试已结束", "重试用尽", "基本没真正探成"} {
			if strings.Contains(message, unsupported) {
				t.Errorf("outcome invented %q: %s", unsupported, message)
			}
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
