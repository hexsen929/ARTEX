package agent

import (
	"context"
	"errors"
	"fmt"
	"io"
	"iter"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Autumn-27/artex/llmguard"
	"github.com/Autumn-27/artex/memcontext"
	"github.com/Autumn-27/norma/llm"
)

const memoryErrorCanary = "memory-vendor-echo-canary-f94c"

type memoryCodeFixtureError struct {
	code, body string
	status     int
	cause      error
}

func (e *memoryCodeFixtureError) Error() string     { return e.body }
func (e *memoryCodeFixtureError) ErrorCode() string { return e.code }
func (e *memoryCodeFixtureError) HTTPStatus() int   { return e.status }
func (e *memoryCodeFixtureError) Unwrap() error     { return e.cause }

func TestMemoryPrivateErrorsPreserveClassificationWithoutVendorBody(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
	}{
		{"auth", errors.New("vendor status 401: " + memoryErrorCanary)},
		{"quota", errors.New("vendor status 402: " + memoryErrorCanary)},
		{"rate", errors.New("vendor status 429: " + memoryErrorCanary)},
		{"context", errors.New("vendor status 400: context_length_exceeded " + memoryErrorCanary)},
		{"invalid", errors.New("vendor status 422: " + memoryErrorCanary)},
		{"provider", errors.New("vendor status 503: " + memoryErrorCanary)},
		{"model503", errors.New("vendor status 503: model_not_found " + memoryErrorCanary)},
		{"transport_flat", errors.New("connection reset: " + memoryErrorCanary)},
		{"transport_eof", fmt.Errorf("%s: %w", memoryErrorCanary, io.ErrUnexpectedEOF)},
		{"deadline", fmt.Errorf("%s: %w", memoryErrorCanary, context.DeadlineExceeded)},
		{"cancel", fmt.Errorf("%s: %w", memoryErrorCanary, context.Canceled)},
		{"budget", fmt.Errorf("%s: %w", memoryErrorCanary, llmguard.ErrRetryBudget)},
		{"cooling", errors.New("llm_cooling_down: " + memoryErrorCanary)},
		{"unknown", errors.New("opaque failure: " + memoryErrorCanary)},
		{"legacy_quota", errors.New("credit balance exhausted: " + memoryErrorCanary)},
		{"http_not_legacy_status", errors.New("HTTP 400: " + memoryErrorCanary)},
		{"structured_auth", &memoryCodeFixtureError{code: "invalid_api_key", status: 0, body: memoryErrorCanary}},
		{"structured_quota", &memoryCodeFixtureError{code: "insufficient_quota", status: 0, body: memoryErrorCanary}},
		{"structured_rate", &memoryCodeFixtureError{code: "rate_limit_error", status: 0, body: memoryErrorCanary}},
		{"unsafe_code", &memoryCodeFixtureError{code: memoryErrorCanary, status: 0, body: memoryErrorCanary}},
		{"response_event", &responsesStreamError{event: "response.failed", code: memoryErrorCanary, message: memoryErrorCanary, cause: errors.New(memoryErrorCanary)}},
		{"stale", fmt.Errorf("%s: %w", memoryErrorCanary, &memcontext.StaleError{})},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := memcontext.WithMetadataOnly(t.Context())
			private := privateMemoryError(ctx, tc.err)
			if private == nil || strings.Contains(private.Error(), memoryErrorCanary) || strings.Contains(fmt.Sprintf("%v", private), memoryErrorCanary) {
				t.Fatalf("vendor body leaked from classified error: %v", private)
			}
			if llmguard.Classify(private) != llmguard.Classify(tc.err) || llmguard.Retryable(private) != llmguard.Retryable(tc.err) || llmguard.HardFailure(private) != llmguard.HardFailure(tc.err) || llmguard.Status(private) != llmguard.Status(tc.err) {
				t.Fatalf("sanitizing changed policy: kind=%s/%s status=%d/%d", llmguard.Classify(private), llmguard.Classify(tc.err), llmguard.Status(private), llmguard.Status(tc.err))
			}
			if IsQuotaExhaustedMessage(private.Error()) != IsQuotaExhaustedMessage(tc.err.Error()) {
				t.Fatal("sanitizing changed the task chain's strict quota policy")
			}
			originalLegacy := memoryLegacyStatus.FindStringSubmatch(tc.err.Error())
			privateLegacy := memoryLegacyStatus.FindStringSubmatch(private.Error())
			if strings.Join(originalLegacy, ":") != strings.Join(privateLegacy, ":") {
				t.Fatal("sanitizing changed legacy pool status detection")
			}
			if !errors.Is(private, tc.err) || memcontext.IsStale(private) != memcontext.IsStale(tc.err) {
				t.Fatal("sanitizing lost the original cancellation/budget/typed cause")
			}
			var coded interface{ ErrorCode() string }
			if !errors.As(private, &coded) || strings.Contains(coded.ErrorCode(), memoryErrorCanary) {
				t.Fatal("unsafe vendor code leaked through ErrorCode")
			}
			if privateMemoryError(t.Context(), tc.err) != tc.err {
				t.Fatal("ordinary non-memory diagnostic changed")
			}
		})
	}
	if privateMemoryError(memcontext.WithMetadataOnly(t.Context()), nil) != nil {
		t.Fatal("successful model call turned into an error")
	}
}

func TestMemoryPrivateProviderProtectsStreamAndCompleteOnlyWhenEnabled(t *testing.T) {
	raw := errors.New("vendor status 401: " + memoryErrorCanary)
	inner := memoryFixtureProvider{
		complete: func(context.Context, llm.CompletionRequest) (llm.Message, string, llm.Usage, error) {
			return llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{llm.TextBlock("business result")}}, "stop", llm.Usage{InputTokens: 7, OutputTokens: 3}, raw
		},
		stream: func(context.Context, llm.CompletionRequest) iter.Seq2[llm.StreamEvent, error] {
			return func(yield func(llm.StreamEvent, error) bool) {
				if !yield(llm.StreamEvent{Type: llm.SETextDelta, Text: "business result"}, nil) {
					return
				}
				yield(llm.StreamEvent{}, raw)
			}
		},
	}
	provider := memoryPrivateProvider{inner: inner}
	for _, enabled := range []bool{false, true} {
		ctx := t.Context()
		if enabled {
			ctx = memcontext.WithMetadataOnly(ctx)
		}
		message, stop, usage, err := provider.Complete(ctx, llm.CompletionRequest{})
		if message.Text() != "business result" || stop != "stop" || usage.InputTokens != 7 || usage.OutputTokens != 3 {
			t.Fatal("privacy error handling changed response/usage")
		}
		if enabled == strings.Contains(err.Error(), memoryErrorCanary) || !errors.Is(err, raw) {
			t.Fatal("atomic error privacy mode or cause changed")
		}
		text := ""
		for event, streamErr := range provider.Stream(ctx, llm.CompletionRequest{}) {
			text += event.Text
			if streamErr != nil && (enabled == strings.Contains(streamErr.Error(), memoryErrorCanary) || !errors.Is(streamErr, raw)) {
				t.Fatal("stream error privacy mode or cause changed")
			}
		}
		if text != "business result" {
			t.Fatal("privacy mode changed ordinary stream content")
		}
	}
}

func TestMemoryPrivateErrorRecoversStaleDiagnosisBeforeRouting(t *testing.T) {
	ctx, release := memcontext.WithRecall(t.Context(), []memcontext.Reference{{ID: 1, Version: 1}}, func(context.Context) error { return errors.New("revoked") })
	defer release()
	_ = memcontext.Validate(ctx)
	sdkErr := fmt.Errorf("%s: %w", memoryErrorCanary, ctx.Err())
	private := privateMemoryError(ctx, sdkErr)
	if strings.Contains(private.Error(), memoryErrorCanary) || !strings.Contains(private.Error(), "memory_invalidated") || !errors.Is(private, sdkErr) || !errors.Is(private, context.Canceled) || llmguard.Retryable(private) {
		t.Fatalf("SDK cancellation lost its local memory diagnosis: %v", private)
	}
}

// This verifies constructor placement, not just the decorator in isolation:
// all supported concrete protocols must sanitize before a caller can log errors.
func TestMemoryPrivacyIsWiredInsideConcreteProviders(t *testing.T) {
	for _, format := range []string{"anthropic", "openai", "openai-responses"} {
		t.Run(format, func(t *testing.T) {
			gateway := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, _ = io.Copy(io.Discard, r.Body)
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusBadRequest)
				_, _ = io.WriteString(w, `{"error":{"message":"`+memoryErrorCanary+`","type":"invalid_request_error"}}`)
			}))
			defer gateway.Close()
			provider, err := ConfigFrom(format, "fixture", gateway.URL+"/v1", "fixture-key", "").NewProvider()
			if err != nil {
				t.Fatal(err)
			}
			request := llm.CompletionRequest{Messages: []llm.Message{llm.UserText("fixture")}, MaxTokens: 32}
			for _, streaming := range []bool{false, true} {
				for _, enabled := range []bool{false, true} {
					ctx := t.Context()
					if enabled {
						ctx = memcontext.WithMetadataOnly(ctx)
					}
					var callErr error
					if streaming {
						for _, eventErr := range provider.Stream(ctx, request) {
							if eventErr != nil {
								callErr = eventErr
							}
						}
					} else {
						_, _, _, callErr = provider.Complete(ctx, request)
					}
					if callErr == nil || enabled == strings.Contains(callErr.Error(), memoryErrorCanary) || llmguard.Status(callErr) != 400 || llmguard.Classify(callErr) != llmguard.InvalidRequest {
						t.Fatalf("format=%s stream=%v private=%v constructor policy=%v", format, streaming, enabled, callErr)
					}
				}
			}
		})
	}
}
