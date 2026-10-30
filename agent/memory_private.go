package agent

import (
	"context"
	"errors"
	"fmt"
	"iter"
	"net"
	"regexp"
	"strconv"
	"strings"

	"github.com/Autumn-27/artex/llmguard"
	"github.com/Autumn-27/artex/memcontext"
	"github.com/Autumn-27/norma/llm"
)

// memoryPrivateProvider belongs outside the concrete SDK/Responses/timeout
// adapters and INSIDE recording/routing. Sanitizing only at the harness exit is
// too late: the routing pool and task retry loop log provider errors themselves.
type memoryPrivateProvider struct{ inner llm.Provider }

func (p memoryPrivateProvider) Complete(ctx context.Context, request llm.CompletionRequest) (llm.Message, string, llm.Usage, error) {
	message, stop, usage, err := p.inner.Complete(ctx, request)
	return message, stop, usage, privateMemoryError(ctx, err)
}

func (p memoryPrivateProvider) Stream(ctx context.Context, request llm.CompletionRequest) iter.Seq2[llm.StreamEvent, error] {
	if !memcontext.MetadataOnly(ctx) {
		return p.inner.Stream(ctx, request)
	}
	return func(yield func(llm.StreamEvent, error) bool) {
		for event, err := range p.inner.Stream(ctx, request) {
			if !yield(event, privateMemoryError(ctx, err)) {
				return
			}
		}
	}
}

// Preserve the legacy pool's string-status parser as well as the structured
// HTTPStatus interface. Turning an originally status-less error into "status
// 400", for example, would silently change that pool's failover behavior.
var memoryLegacyStatus = regexp.MustCompile(`status (\d{3})`)

type memoryPrivateError struct {
	cause        error
	kind         llmguard.Kind
	status       int
	legacyStatus int
	code         string
	quota        bool
}

func (e *memoryPrivateError) Error() string {
	if e.code == "memory_invalidated" {
		return (&memcontext.StaleError{}).Error()
	}
	message := "memory_private: " + string(e.kind)
	if e.legacyStatus != 0 {
		message += fmt.Sprintf(" status %d", e.legacyStatus)
	}
	// These are fixed host-owned markers, never text copied from the vendor.
	if e.kind == llmguard.CoolingDown {
		message += " llm_cooling_down"
	}
	// The task-specific quota router intentionally uses a stricter message
	// predicate than Classify. Preserve that predicate independently rather than
	// accidentally turning every structured quota code into a chain transition.
	if e.quota {
		message += " insufficient_quota"
	}
	return message
}

func (e *memoryPrivateError) HTTPStatus() int   { return e.status }
func (e *memoryPrivateError) ErrorCode() string { return e.code }
func (e *memoryPrivateError) Unwrap() error     { return e.cause }

// Only transport-classified errors implement net.Error. This preserves the
// classifier's flattened-network-error behavior without inventing a specific
// failure message (such as claiming every transport failure was a reset).
type memoryPrivateTransportError struct{ *memoryPrivateError }

func (e *memoryPrivateTransportError) Timeout() bool {
	var network net.Error
	return errors.Is(e.cause, context.DeadlineExceeded) || (errors.As(e.cause, &network) && network.Timeout())
}

func (e *memoryPrivateTransportError) Temporary() bool {
	var network net.Error
	return errors.As(e.cause, &network) && network.Temporary()
}

func privateMemoryError(ctx context.Context, err error) error {
	if !memcontext.MetadataOnly(ctx) {
		return err
	}
	if stale := memcontext.ErrorFrom(ctx); stale != nil && err == nil {
		err = stale
	}
	if err == nil {
		return nil
	}
	kind := llmguard.Classify(err)
	code := safeMemoryErrorCode(err, kind)
	if memcontext.IsStale(err) || memcontext.ErrorFrom(ctx) != nil {
		kind, code = llmguard.Canceled, "memory_invalidated"
	}
	status := 0
	if match := memoryLegacyStatus.FindStringSubmatch(err.Error()); len(match) == 2 {
		status, _ = strconv.Atoi(match[1])
	}
	private := &memoryPrivateError{
		cause: err, kind: kind, status: llmguard.Status(err), legacyStatus: status,
		code: code, quota: IsQuotaExhaustedMessage(err.Error()),
	}
	if kind == llmguard.TransportFailure {
		return &memoryPrivateTransportError{private}
	}
	return private
}

// Error codes are attacker-controlled strings too. Preserve only protocol codes
// understood by the shared classifier; everything else becomes a fixed class.
func safeMemoryErrorCode(err error, kind llmguard.Kind) string {
	var coded interface{ ErrorCode() string }
	if errors.As(err, &coded) {
		code := strings.ToLower(strings.TrimSpace(coded.ErrorCode()))
		switch code {
		case "invalid_api_key", "authentication_error", "permission_denied", "access_denied",
			"insufficient_quota", "insufficient_user_quota", "billing_hard_limit_reached", "billing_not_active",
			"model_not_found", "no_available_channel", "rate_limit_exceeded", "rate_limit_error",
			"context_length_exceeded", "max_context_length_exceeded", "context_window_exceeded",
			"invalid_request", "invalid_request_error", "server_error", "internal_server_error", "overloaded_error", "service_unavailable":
			return code
		}
	}
	switch kind {
	case llmguard.Auth:
		return "authentication_error"
	case llmguard.Quota:
		return "insufficient_quota"
	case llmguard.ModelUnavailable:
		return "model_not_found"
	case llmguard.RateLimit:
		return "rate_limit_error"
	case llmguard.ContextLimit:
		return "context_length_exceeded"
	case llmguard.InvalidRequest:
		return "invalid_request_error"
	case llmguard.ProviderFailure:
		return "server_error"
	default:
		return string(kind)
	}
}
