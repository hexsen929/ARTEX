//go:build ignore

package server

import (
	"context"
	"errors"
	"testing"

	"github.com/Autumn-27/norma/harness"
	"github.com/Autumn-27/norma/llm"
)

func TestModelFailureClassificationAndRetry(t *testing.T) {
	for _, tc := range []struct {
		name  string
		err   error
		kind  string
		retry bool
	}{
		{"quota", &llm.HTTPError{StatusCode: 429, Body: "insufficient_quota"}, "quota", false},
		{"rate_limit", &llm.HTTPError{StatusCode: 429, Body: "too many requests"}, "rate_limit", true},
		{"bad_request", &llm.HTTPError{StatusCode: 400, Body: "incomplete tool input"}, "protocol", false},
		{"auth", &llm.HTTPError{StatusCode: 401}, "configuration", false},
		{"refusal", &llm.HTTPError{StatusCode: 403}, "refusal", false},
		{"policy_in_400", &llm.HTTPError{StatusCode: 400, Body: "content_policy_violation"}, "refusal", false},
		{"non_json", errors.New("anthropic: decode response: invalid character '<'"), "protocol", false},
		{"incomplete_live_tool", errors.New("incomplete tool input for call_fixture"), "protocol", false},
		{"local", &llm.RequestError{Field: "format", Value: "invalid"}, "configuration", false},
		{"history", &llm.HistoryError{Reason: "invalid signature"}, "protocol", false},
		{"transport", &llm.HTTPError{StatusCode: 503}, "transport", true},
		{"deadline", context.DeadlineExceeded, "transport", true},
		{"cancel", context.Canceled, "cancelled", false},
		{"unknown", errors.New("unknown upstream failure"), "unknown", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			failure := modelFailure(tc.err)
			if failure.Kind != tc.kind || failure.Retryable != tc.retry {
				t.Fatalf("got %+v", failure)
			}
			if got := retryableWorkerModelError(harness.ReasonModelError, tc.err); got != tc.retry {
				t.Fatalf("retry=%v", got)
			}
		})
	}
	if retryableWorkerModelError(harness.ReasonModelError, nil) {
		t.Fatal("nil error retried")
	}
}
