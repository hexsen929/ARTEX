// Package llmguard shares error classification and a logical-request HTTP budget
// across provider SDK retries, stream retries and profile failover.
package llmguard

import (
	"context"
	"errors"
	"io"
	"net"
	"regexp"
	"strconv"
	"strings"
	"sync/atomic"
)

type Kind string

const (
	Canceled         Kind = "canceled"
	Quota            Kind = "quota_exhausted"
	Auth             Kind = "authentication"
	ModelUnavailable Kind = "model_unavailable"
	RateLimit        Kind = "rate_limited"
	ContextLimit     Kind = "context_limit"
	InvalidRequest   Kind = "invalid_request"
	ProviderFailure  Kind = "provider_failure"
	TransportFailure Kind = "transport_failure"
	UnknownFailure   Kind = "unknown_failure"
	RetryBudget      Kind = "retry_budget_exhausted"
	CoolingDown      Kind = "cooling_down"
)

var statusPattern = regexp.MustCompile(`(?i)(?:status(?:\s+code)?|http(?:\s+status)?)\s*[=:]?\s*(\d{3})\b`)

func Status(err error) int {
	if err == nil {
		return 0
	}
	// Structured adapter errors retain status without making callers parse a
	// provider message. A successful HTTP 200 carrying a failed SSE event is not
	// an error status; let its ErrorCode identify the failure instead.
	var status interface{ HTTPStatus() int }
	if errors.As(err, &status) {
		if n := status.HTTPStatus(); n >= 400 && n <= 599 {
			return n
		}
	}
	m := statusPattern.FindStringSubmatch(err.Error())
	if len(m) < 2 {
		return 0
	}
	n, _ := strconv.Atoi(m[1])
	return n
}
func MissingModel(s string) bool {
	s = strings.ToLower(s)
	for _, marker := range []string{"model_not_found", "no available channel", "no_available_channel", "model does not exist", "model is not available"} {
		if strings.Contains(s, marker) {
			return true
		}
	}
	return false
}
func Classify(err error) Kind {
	if err == nil {
		return ""
	}
	s := strings.ToLower(err.Error())
	switch {
	case errors.Is(err, context.Canceled):
		return Canceled
	case strings.Contains(s, "retry_budget_exhausted"):
		return RetryBudget
	case strings.Contains(s, "llm_cooling_down"):
		return CoolingDown
	}
	if kind := structuredCodeKind(err); kind != "" {
		return kind
	}
	switch {
	case strings.Contains(s, "context length"), strings.Contains(s, "context_length"), strings.Contains(s, "maximum context"), strings.Contains(s, "too long"), Status(err) == 413:
		return ContextLimit
	case MissingModel(s):
		return ModelUnavailable
	}
	switch n := Status(err); {
	case n == 401 || n == 403:
		return Auth
	case n == 402:
		return Quota
	case n == 404:
		return ModelUnavailable
	case n == 429:
		return RateLimit
	case n == 408:
		return TransportFailure
	case n >= 500:
		return ProviderFailure
	case n >= 400:
		return InvalidRequest
	}
	if transportError(err, s) {
		return TransportFailure
	}
	// An unrecognized provider event or adapter error is not evidence that the
	// network failed. Keep the diagnosis neutral, including generic stream errors.
	return UnknownFailure
}

// structuredCodeKind understands protocol-level errors that arrive after HTTP
// headers, when there may be no failing HTTP status at all. The small interface
// works through normal error wrapping without coupling providers to this package.
func structuredCodeKind(err error) Kind {
	var coded interface{ ErrorCode() string }
	if !errors.As(err, &coded) {
		return ""
	}
	switch strings.ToLower(strings.TrimSpace(coded.ErrorCode())) {
	case "invalid_api_key", "authentication_error", "permission_denied", "access_denied":
		return Auth
	case "insufficient_quota", "insufficient_user_quota", "billing_hard_limit_reached", "billing_not_active":
		return Quota
	case "model_not_found", "no_available_channel":
		return ModelUnavailable
	case "rate_limit_exceeded", "rate_limit_error":
		return RateLimit
	case "context_length_exceeded", "max_context_length_exceeded", "context_window_exceeded":
		return ContextLimit
	case "invalid_request", "invalid_request_error":
		return InvalidRequest
	case "server_error", "internal_server_error", "overloaded_error", "service_unavailable":
		return ProviderFailure
	}
	return ""
}

func transportError(err error, message string) bool {
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		return true
	}
	var networkError net.Error
	if errors.As(err, &networkError) {
		return true
	}
	// Some SDKs flatten network failures to strings. Match concrete transport
	// signatures, not the generic phrase "stream error" used for API SSE failures.
	if message == "eof" || strings.HasSuffix(message, ": eof") {
		return true
	}
	for _, marker := range []string{
		"unexpected eof", "dial tcp", "dial udp", "dial unix", "no such host",
		"connection reset", "connection refused", "connection aborted", "broken pipe",
		"network is unreachable", "temporary network error", "i/o timeout",
		"tls handshake timeout", "client.timeout exceeded", "context deadline exceeded",
		"server closed idle connection", "stream error: stream id", "http2: stream closed",
		"internal_error; received from peer", "llm call_timeout:",
	} {
		if strings.Contains(message, marker) {
			return true
		}
	}
	return false
}

func Retryable(err error) bool {
	if err == nil || errors.Is(err, context.Canceled) {
		return false
	}
	switch Classify(err) {
	case RateLimit, ProviderFailure, TransportFailure:
		return true
	case UnknownFailure:
		// Preserve the existing retry policy: unknown errors previously fell back
		// to TransportFailure. Neutral diagnosis must not change retry counts or
		// the caller's pre-commit-only replay window.
		return true
	}
	return false
}
func HardFailure(err error) bool {
	switch Classify(err) {
	case Auth, Quota, ModelUnavailable:
		return true
	}
	return false
}

const DefaultHTTPAttempts = 8

var ErrRetryBudget = errors.New("retry_budget_exhausted: 本次模型请求的 HTTP 尝试总预算已耗尽")

type budgetKey struct{}
type budget struct{ remaining atomic.Int64 }

// WithBudget reuses an enclosing budget; it never resets it on failover/retry.
func WithBudget(ctx context.Context, attempts int) context.Context {
	if ctx.Value(budgetKey{}) != nil {
		return ctx
	}
	if attempts <= 0 {
		attempts = DefaultHTTPAttempts
	}
	b := &budget{}
	b.remaining.Store(int64(attempts))
	return context.WithValue(ctx, budgetKey{}, b)
}

// TakeHTTP counts actual outgoing HTTP attempts, including SDK-internal retries.
func TakeHTTP(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if b, ok := ctx.Value(budgetKey{}).(*budget); ok && b.remaining.Add(-1) < 0 {
		return ErrRetryBudget
	}
	return nil
}
