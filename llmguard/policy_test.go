package llmguard

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"testing"
)

func TestClassification(t *testing.T) {
	for _, tc := range []struct {
		s     string
		k     Kind
		retry bool
		hard  bool
	}{
		{`status 503: {"code":"model_not_found","message":"No available channel"}`, ModelUnavailable, false, true},
		{"status 521: gateway down", ProviderFailure, true, false}, {"status 429: rate limit", RateLimit, true, false},
		{"HTTP 401: invalid key", Auth, false, true}, {"status 402: payment", Quota, false, true},
		{"status 400: context_length_exceeded", ContextLimit, false, false}, {"status 422: invalid", InvalidRequest, false, false},
		{"unexpected EOF", TransportFailure, true, false}, {ErrRetryBudget.Error(), RetryBudget, false, false},
		{"llm_cooling_down", CoolingDown, false, false},
	} {
		t.Run(tc.s, func(t *testing.T) {
			e := errors.New(tc.s)
			if Classify(e) != tc.k || Retryable(e) != tc.retry || HardFailure(e) != tc.hard {
				t.Fatalf("classification %v retry=%v hard=%v", Classify(e), Retryable(e), HardFailure(e))
			}
		})
	}
}
func TestBudgetSharedAndConcurrent(t *testing.T) {
	ctx := WithBudget(context.Background(), 3)
	ctx = WithBudget(ctx, 100)
	var success atomic.Int64
	var wg sync.WaitGroup
	for range 20 {
		wg.Go(func() {
			if TakeHTTP(ctx) == nil {
				success.Add(1)
			}
		})
	}
	wg.Wait()
	if success.Load() != 3 {
		t.Fatalf("budget allowed %d requests", success.Load())
	}
	if !errors.Is(TakeHTTP(ctx), ErrRetryBudget) {
		t.Fatal("budget reset")
	}
	if TakeHTTP(context.Background()) != nil {
		t.Fatal("unbudgeted probes should work")
	}
	cancelCtx, cancel := context.WithCancel(WithBudget(context.Background(), 1))
	cancel()
	if !errors.Is(TakeHTTP(cancelCtx), context.Canceled) {
		t.Fatal("cancellation not preserved")
	}
}

// codedPolicyError models an adapter error without depending on a provider's
// concrete type. It deliberately keeps the diagnostic text free of HTTP/code
// clues so these tests exercise structured errors and normal wrapping.
type codedPolicyError struct {
	code    string
	status  int
	message string
	cause   error
}

func (e *codedPolicyError) Error() string     { return e.message }
func (e *codedPolicyError) ErrorCode() string { return e.code }
func (e *codedPolicyError) HTTPStatus() int   { return e.status }
func (e *codedPolicyError) Unwrap() error     { return e.cause }

func TestStructuredProviderErrorClassification(t *testing.T) {
	for _, tc := range []struct {
		code        string
		kind        Kind
		retry, hard bool
	}{
		{"invalid_api_key", Auth, false, true},
		{"authentication_error", Auth, false, true},
		{"permission_denied", Auth, false, true},
		{"insufficient_quota", Quota, false, true},
		{"insufficient_user_quota", Quota, false, true},
		{"billing_hard_limit_reached", Quota, false, true},
		{"model_not_found", ModelUnavailable, false, true},
		{"no_available_channel", ModelUnavailable, false, true},
		{"rate_limit_exceeded", RateLimit, true, false},
		{"rate_limit_error", RateLimit, true, false},
		{"context_length_exceeded", ContextLimit, false, false},
		{"context_window_exceeded", ContextLimit, false, false},
		{"invalid_request_error", InvalidRequest, false, false},
		{"server_error", ProviderFailure, true, false},
		{"overloaded_error", ProviderFailure, true, false},
		{"service_unavailable", ProviderFailure, true, false},
		{" SERVER_ERROR ", ProviderFailure, true, false},
		{"vendor_specific_error", UnknownFailure, true, false},
		{"", UnknownFailure, true, false},
	} {
		t.Run(tc.code, func(t *testing.T) {
			err := fmt.Errorf("task model call: %w", &codedPolicyError{code: tc.code, status: 200, message: "provider failure event"})
			if got := Classify(err); got != tc.kind {
				t.Fatalf("classification=%q, want %q", got, tc.kind)
			}
			if got := Status(err); got != 0 {
				t.Fatalf("successful SSE HTTP status became failure status: %d", got)
			}
			if Retryable(err) != tc.retry || HardFailure(err) != tc.hard {
				t.Fatalf("retry=%v hard=%v, want %v/%v", Retryable(err), HardFailure(err), tc.retry, tc.hard)
			}
		})
	}
}

func TestStructuredStatusAndLegacyStatusFallback(t *testing.T) {
	for _, tc := range []struct {
		status  int
		message string
		want    int
		kind    Kind
	}{
		{429, "request rejected", 429, RateLimit},
		{401, "request rejected", 401, Auth},
		{402, "request rejected", 402, Quota},
		{422, "request rejected", 422, InvalidRequest},
		{503, "request rejected", 503, ProviderFailure},
		{200, "request rejected", 0, UnknownFailure},
		{200, "openai: status 502: gateway", 502, ProviderFailure},
		{0, "HTTP status: 404: missing", 404, ModelUnavailable},
		{700, "HTTP status code=422: invalid", 422, InvalidRequest},
	} {
		t.Run(fmt.Sprintf("%d-%s", tc.status, tc.message), func(t *testing.T) {
			err := fmt.Errorf("wrapped: %w", &codedPolicyError{status: tc.status, message: tc.message})
			if got := Status(err); got != tc.want {
				t.Fatalf("status=%d, want %d", got, tc.want)
			}
			if got := Classify(err); got != tc.kind {
				t.Fatalf("classification=%q, want %q", got, tc.kind)
			}
		})
	}
	if Status(nil) != 0 || Classify(nil) != "" || Retryable(nil) || HardFailure(nil) {
		t.Fatal("nil must remain non-error")
	}
}

func TestOnlyConcreteTransportErrorsAreDiagnosedAsTransport(t *testing.T) {
	for _, err := range []error{
		io.EOF,
		fmt.Errorf("provider scanner: %w", io.ErrUnexpectedEOF),
		fmt.Errorf("deadline: %w", context.DeadlineExceeded),
		&net.OpError{Op: "read", Net: "tcp", Err: errors.New("socket ended")},
		errors.New("unexpected EOF"),
		errors.New("openai: EOF"),
		errors.New("dial tcp: network is unreachable"),
		errors.New("dial tcp: connection refused"),
		errors.New("connection reset by peer"),
		errors.New("temporary network error"),
		errors.New("stream error: stream ID 47; INTERNAL_ERROR; received from peer"),
		errors.New("llm call_timeout: request deadline reached"),
	} {
		t.Run(err.Error(), func(t *testing.T) {
			if got := Classify(err); got != TransportFailure || !Retryable(err) {
				t.Fatalf("classification=%q retry=%v, want transport/retryable", got, Retryable(err))
			}
		})
	}
	for _, message := range []string{
		"openai-responses: stream error",
		"invalid character in response JSON",
		"provider returned an unrecognized error event",
	} {
		err := errors.New(message)
		if got := Classify(err); got != UnknownFailure {
			t.Errorf("%q: classification=%q, want neutral unknown", message, got)
		}
		// Only the diagnosis changes. Preserve the previous retry decision and
		// leave counts, budgets and post-output replay windows to their callers.
		if !Retryable(err) || HardFailure(err) {
			t.Errorf("%q: unknown retry behavior changed", message)
		}
	}
}

func TestCancellationWinsOverStructuredProviderError(t *testing.T) {
	err := fmt.Errorf("wrapped: %w", &codedPolicyError{code: "server_error", status: 503, message: "request canceled", cause: context.Canceled})
	if Classify(err) != Canceled || Retryable(err) || HardFailure(err) {
		t.Fatalf("cancellation lost: kind=%q retry=%v hard=%v", Classify(err), Retryable(err), HardFailure(err))
	}
}
