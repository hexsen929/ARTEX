// Package llmguard shares error classification and a logical-request HTTP budget
// across provider SDK retries, stream retries and profile failover.
package llmguard

import (
	"context"
	"errors"
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
	RetryBudget      Kind = "retry_budget_exhausted"
	CoolingDown      Kind = "cooling_down"
)

var statusPattern = regexp.MustCompile(`(?i)(?:status(?:\s+code)?|http(?:\s+status)?)\s*[=:]?\s*(\d{3})\b`)

func Status(err error) int {
	if err == nil {
		return 0
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
	return TransportFailure
}
func Retryable(err error) bool {
	if err == nil || errors.Is(err, context.Canceled) {
		return false
	}
	switch Classify(err) {
	case RateLimit, ProviderFailure, TransportFailure:
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
