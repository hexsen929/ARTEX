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
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/Autumn-27/artex/llmguard"
	"github.com/Autumn-27/artex/llmrec"
	"github.com/Autumn-27/norma/llm"
)

func responsesErrorFixtureProvider(t *testing.T, handler http.HandlerFunc, configure func(*Config)) llm.Provider {
	t.Helper()
	upstream := httptest.NewServer(handler)
	t.Cleanup(upstream.Close)
	cfg := ConfigFrom("openai-responses", "fixture-model", upstream.URL+"/v1", "fixture-api-key", "")
	cfg.Retry.ConnectAttempts = -1
	if configure != nil {
		configure(&cfg)
	}
	p, err := cfg.NewProvider()
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func responsesErrorFixtureRequest(text string) llm.CompletionRequest {
	return llm.CompletionRequest{Messages: []llm.Message{llm.UserText(text)}}
}

func responsesErrorReadStream(ctx context.Context, p llm.Provider, text string) ([]llm.StreamEvent, error) {
	var events []llm.StreamEvent
	for ev, err := range p.Stream(ctx, responsesErrorFixtureRequest(text)) {
		if err != nil {
			return events, err
		}
		events = append(events, ev)
	}
	return events, nil
}

func TestResponsesErrorStructuredFailuresPreserveRawCapture(t *testing.T) {
	for _, tc := range []struct {
		name, wire, code, message string
	}{
		{
			name: "nested_response_error",
			wire: "event: response.failed\ndata: {\"type\":\"response.failed\",\"response\":{\"status\":\"failed\",\"error\":{\"code\":\"server_error\",\"message\":\"fixture upstream failure\"}}}\n\n",
			code: "server_error", message: "fixture upstream failure",
		},
		{
			name: "top_level_error",
			wire: "event: error\ndata: {\"type\":\"error\",\"code\":\"rate_limit_exceeded\",\"message\":\"fixture rate limit\"}\n\n",
			code: "rate_limit_exceeded", message: "fixture rate limit",
		},
		{
			name: "type_only_discriminator",
			wire: "data: {\"type\":\"response.failed\",\"response\":{\"error\":{\"code\":\"context_length_exceeded\",\"message\":\"fixture context limit\"}}}\n\n",
			code: "context_length_exceeded", message: "fixture context limit",
		},
		{
			name: "legacy_error_object_keeps_code",
			wire: "event: error\ndata: {\"error\":{\"code\":\"invalid_api_key\",\"message\":\"fixture rejected\"}}\n\n",
			code: "invalid_api_key", message: "fixture rejected",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var calls atomic.Int32
			p := responsesErrorFixtureProvider(t, func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = io.WriteString(w, tc.wire)
			}, nil)
			ctx, capture := llmrec.NewCapture(t.Context())
			_, err := responsesErrorReadStream(ctx, p, "fixture")
			var failure *responsesStreamError
			if !errors.As(err, &failure) || failure.ErrorCode() != tc.code || !strings.Contains(failure.Error(), tc.message) {
				t.Fatalf("structured failure lost: %T %v", err, err)
			}
			if failure.HTTPStatus() != 0 || failure.Unwrap() == nil {
				t.Fatalf("wire 200 invented a failure HTTP status or lost original error: %v", err)
			}
			if capture.RawResponse() != tc.wire {
				t.Fatal("diagnostics changed raw SSE capture")
			}
			if calls.Load() != 1 {
				t.Fatalf("diagnostics replayed the request: %d calls", calls.Load())
			}
		})
	}
}

func TestResponsesErrorCodexCompleteUsesStructuredStreamFailure(t *testing.T) {
	p := responsesErrorFixtureProvider(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "event: response.failed\ndata: {\"response\":{\"error\":{\"code\":\"server_error\",\"message\":\"fixture complete failure\"}}}\n\n")
	}, func(c *Config) { c.UserAgent = "codex" })
	msg, stop, usage, err := p.Complete(t.Context(), responsesErrorFixtureRequest("fixture"))
	var failure *responsesStreamError
	if !errors.As(err, &failure) || failure.ErrorCode() != "server_error" || !strings.Contains(err.Error(), "fixture complete failure") {
		t.Fatalf("Codex Complete lost stream failure metadata: %v", err)
	}
	if msg.Text() != "" || stop != "" || usage != (llm.Usage{}) {
		t.Fatal("failed complete returned successful-looking partial output")
	}
}

func TestResponsesErrorNormalAndLegacyResponsesStayUnchanged(t *testing.T) {
	for _, tc := range []struct {
		name, wire, expectedError string
	}{
		{name: "normal", wire: "event: response.output_text.delta\ndata: {\"delta\":\"fixture text\"}\n\nevent: response.completed\ndata: {\"response\":{\"usage\":{\"input_tokens\":7,\"output_tokens\":2}}}\n\n"},
		{name: "legacy_error", wire: "event: error\ndata: {\"error\":{\"code\":\"fixture\",\"message\":\"existing parsed message\"}}\n\n", expectedError: "openai-responses: existing parsed message"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := responsesErrorFixtureProvider(t, func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = io.WriteString(w, tc.wire)
			}, nil)
			events, err := responsesErrorReadStream(t.Context(), p, "fixture")
			if tc.expectedError != "" {
				if err == nil || errors.Unwrap(err) == nil || errors.Unwrap(err).Error() != tc.expectedError || !strings.Contains(err.Error(), "existing parsed message") {
					t.Fatalf("legacy error changed: %v", err)
				}
				return
			}
			if err != nil || len(events) != 2 || events[0].Type != llm.SETextDelta || events[0].Text != "fixture text" || events[1].StopReason != "end_turn" || events[1].Usage.InputTokens != 7 {
				t.Fatalf("normal stream changed: events=%+v err=%v", events, err)
			}
		})
	}
}

func TestResponsesErrorHTTPRetryCountAndCaptureUnchanged(t *testing.T) {
	var calls atomic.Int32
	const wire = "event: response.failed\ndata: {\"response\":{\"error\":{\"code\":\"server_error\",\"message\":\"final fixture error\"}}}\n\n"
	p := responsesErrorFixtureProvider(t, func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = io.WriteString(w, `{"error":{"message":"fixture temporary failure"}}`)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, wire)
	}, func(c *Config) {
		c.Retry.ConnectAttempts = 1
		c.Retry.ConnectInterval = time.Millisecond
	})
	ctx, capture := llmrec.NewCapture(t.Context())
	_, err := responsesErrorReadStream(ctx, p, "fixture")
	var failure *responsesStreamError
	if !errors.As(err, &failure) || failure.code != "server_error" {
		t.Fatalf("final response error was not preserved: %v", err)
	}
	if calls.Load() != 2 {
		t.Fatalf("HTTP retry count changed: %d", calls.Load())
	}
	attempts := capture.Attempts()
	if len(attempts) != 2 || attempts[0].Status != 503 || attempts[1].Status != 200 || attempts[1].Body != wire {
		t.Fatalf("raw attempts changed: %+v", attempts)
	}
}

type responsesErrorChunkBody struct {
	reader    *strings.Reader
	chunkSize int
	readError error
	closeErr  error
	closed    int
}

func (b *responsesErrorChunkBody) Read(p []byte) (int, error) {
	if b.chunkSize > 0 && len(p) > b.chunkSize {
		p = p[:b.chunkSize]
	}
	n, err := b.reader.Read(p)
	if err == io.EOF && b.readError != nil {
		err = b.readError
	}
	return n, err
}
func (b *responsesErrorChunkBody) Close() error { b.closed++; return b.closeErr }

func responsesErrorFixtureAttempt(t *testing.T, ctx context.Context) *responsesErrorAttempt {
	t.Helper()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://fixture.invalid/v1/responses", nil)
	if err != nil {
		t.Fatal(err)
	}
	return startResponsesErrorAttempt(req)
}

func TestResponsesErrorObserverFramingBoundsAndClose(t *testing.T) {
	for _, tc := range []struct {
		name, wire string
		wantCode   string
	}{
		{name: "CRLF_multiline", wire: "event: error\r\ndata: {\"code\":\"server_error\",\r\ndata: \"message\":\"fixture\"}\r\n\r\n", wantCode: "server_error"},
		{name: "type_only", wire: "data: {\"type\":\"error\",\"code\":\"rate_limit_exceeded\",\"message\":\"fixture\"}\n\n", wantCode: "rate_limit_exceeded"},
		{name: "complete_data_line_at_EOF", wire: "data: {\"type\":\"error\",\"code\":\"server_error\",\"message\":\"fixture\"}\n", wantCode: "server_error"},
		{name: "unfinished_JSON", wire: "event: error\ndata: {\"code\":\"server_error\",\"message\":\"unfinished"},
		{name: "oversized_frame", wire: "event: error\ndata: {\"message\":\"" + strings.Repeat("x", maxResponsesErrorFrameBytes) + "\"}\n\n"},
		{name: "oversized_then_valid", wire: "data: " + strings.Repeat("x", maxResponsesErrorFrameBytes+1) + "\n\ndata: {\"type\":\"error\",\"code\":\"server_error\",\"message\":\"fixture\"}\n\n", wantCode: "server_error"},
		{name: "unrecognized_event", wire: "event: response.completed\ndata: {\"error\":{\"code\":\"server_error\",\"message\":\"not a failure event\"}}\n\n"},
	} {
		for _, chunk := range []int{1, 3, 17, 65536} {
			t.Run(fmt.Sprintf("%s/chunk_%d", tc.name, chunk), func(t *testing.T) {
				ctx, call := newResponsesErrorCall(t.Context(), "")
				attempt := responsesErrorFixtureAttempt(t, ctx)
				closeErr := errors.New("fixture close error")
				original := &responsesErrorChunkBody{reader: strings.NewReader(tc.wire), chunkSize: chunk, closeErr: closeErr}
				body := attempt.observe(200, original)
				got, err := io.ReadAll(body)
				if err != nil || string(got) != tc.wire {
					t.Fatalf("observer changed bytes/read error: %v", err)
				}
				if err := body.Close(); err != closeErr || original.closed != 1 {
					t.Fatalf("observer changed Close semantics: %v, count %d", err, original.closed)
				}
				generic := errors.New("openai-responses: stream error")
				enriched := call.enrich(generic)
				if tc.wantCode == "" {
					if enriched != generic {
						t.Fatalf("incomplete/oversized/nonfailure frame was inferred as API failure: %v", enriched)
					}
				} else {
					var failure *responsesStreamError
					if !errors.As(enriched, &failure) || failure.ErrorCode() != tc.wantCode || !errors.Is(failure, generic) {
						t.Fatalf("framed error lost: %v", enriched)
					}
				}
			})
		}
	}
}

func TestResponsesErrorAttemptIsolationAndPassthrough(t *testing.T) {
	ctx, call := newResponsesErrorCall(t.Context(), "")
	old := responsesErrorFixtureAttempt(t, ctx)
	wire := "data: {\"type\":\"error\",\"code\":\"server_error\",\"message\":\"old attempt\"}\n\n"
	_, _ = io.ReadAll(old.observe(200, io.NopCloser(strings.NewReader(wire))))
	_ = responsesErrorFixtureAttempt(t, ctx) // new attempt clears the active failure
	generic := errors.New("openai-responses: stream error")
	if got := call.enrich(generic); got != generic {
		t.Fatalf("old attempt leaked into new attempt: %v", got)
	}
	// A late read on the old response cannot overwrite the current attempt.
	_, _ = io.ReadAll(old.observe(200, io.NopCloser(strings.NewReader(wire))))
	if got := call.enrich(generic); got != generic {
		t.Fatalf("late old body read leaked into new attempt: %v", got)
	}
	latest := responsesErrorFixtureAttempt(t, ctx)
	_, _ = io.ReadAll(latest.observe(200, io.NopCloser(strings.NewReader(wire))))
	for _, original := range []error{io.ErrUnexpectedEOF, context.Canceled, context.DeadlineExceeded, llmguard.ErrRetryBudget, errors.New("openai-responses: decode response: fixture"), errors.New("stream error: stream ID 7; INTERNAL_ERROR; received from peer")} {
		if got := call.enrich(original); got != original {
			t.Fatalf("non-generic error was overwritten: %v", got)
		}
	}
	readErr := errors.New("fixture read failure")
	original := &responsesErrorChunkBody{reader: strings.NewReader("event: error\ndata: {"), readError: readErr}
	_, gotErr := io.ReadAll(latest.observe(200, original))
	if gotErr != readErr {
		t.Fatalf("network error was changed: %v", gotErr)
	}
}

func TestResponsesErrorSanitizesAndBoundsMetadata(t *testing.T) {
	message := "fixture-api-key\r\nBearer fixture-bearer-secret sk-fixture-secret password=fixture-password {\"api_key\":\"other-secret\"} {'access_token':'other-token'} " + strings.Repeat("界", 1000)
	safe := sanitizeResponsesError(message, "fixture-api-key", maxResponsesErrorMessageBytes)
	for _, secret := range []string{"fixture-api-key", "fixture-bearer-secret", "sk-fixture-secret", "fixture-password", "other-secret", "other-token", "\r", "\n"} {
		if strings.Contains(safe, secret) {
			t.Fatalf("secret/control character survived sanitization: %q", secret)
		}
	}
	if len(safe) > maxResponsesErrorMessageBytes || !utf8.ValidString(safe) || !strings.Contains(safe, "[REDACTED]") {
		t.Fatal("metadata bounds, UTF-8 or redaction marker invalid")
	}
	if got := sanitizeResponsesError(strings.Repeat("x", 1000), "", maxResponsesErrorCodeBytes); len(got) != maxResponsesErrorCodeBytes {
		t.Fatalf("code bound not enforced: %d", len(got))
	}
}

func TestResponsesErrorRedactsActualOutgoingCredential(t *testing.T) {
	ctx, call := newResponsesErrorCall(t.Context(), "")
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://fixture.invalid/v1/responses", nil)
	if err != nil {
		t.Fatal(err)
	}
	const credential = "opaque-fixture-credential"
	req.Header.Set("Authorization", "Bearer "+credential)
	attempt := startResponsesErrorAttempt(req)
	wire := "data: {\"type\":\"error\",\"code\":\"server_error\",\"message\":\"rejected opaque-fixture-credential\"}\n\n"
	_, _ = io.ReadAll(attempt.observe(200, io.NopCloser(strings.NewReader(wire))))
	got := call.enrich(errors.New("openai-responses: stream error"))
	if strings.Contains(got.Error(), credential) || !strings.Contains(got.Error(), "[REDACTED]") {
		t.Fatalf("actual outgoing credential was not redacted: %v", got)
	}
	if req.Header.Get("Authorization") != "Bearer "+credential {
		t.Fatal("diagnostics changed the outgoing credential")
	}
}

func TestResponsesErrorFirstFailureWinsReadAheadAndExactErrorMatching(t *testing.T) {
	for _, legacy := range []bool{false, true} {
		t.Run(fmt.Sprintf("legacy_%v", legacy), func(t *testing.T) {
			ctx, call := newResponsesErrorCall(t.Context(), "")
			attempt := responsesErrorFixtureAttempt(t, ctx)
			first := "data: {\"type\":\"error\",\"code\":\"first_error\",\"message\":\"first failure\"}\n\n"
			expected := "openai-responses: stream error"
			if legacy {
				first = "event: error\ndata: {\"error\":{\"code\":\"first_error\",\"message\":\"first failure\"}}\n\n"
				expected = "openai-responses: first failure"
			}
			second := "data: {\"type\":\"error\",\"code\":\"second_error\",\"message\":\"second failure\"}\n\n"
			body := attempt.observe(200, io.NopCloser(strings.NewReader(first+second)))
			_, _ = io.ReadAll(body)
			original := errors.New(expected)
			var failure *responsesStreamError
			if got := call.enrich(original); !errors.As(got, &failure) || failure.code != "first_error" || !errors.Is(got, original) {
				t.Fatalf("read-ahead replaced first failure: %v", got)
			}
			unrelated := errors.New("openai-responses: unrelated parser error")
			if got := call.enrich(unrelated); got != unrelated {
				t.Fatalf("mismatched parser error was replaced: %v", got)
			}
		})
	}
}

func TestResponsesErrorMissingMetadataDoesNotInventCodeOrStatus(t *testing.T) {
	ctx, call := newResponsesErrorCall(t.Context(), "")
	attempt := responsesErrorFixtureAttempt(t, ctx)
	_, _ = io.ReadAll(attempt.observe(200, io.NopCloser(strings.NewReader("event: error\ndata: {}\n\n"))))
	var failure *responsesStreamError
	got := call.enrich(errors.New("openai-responses: stream error"))
	if !errors.As(got, &failure) || failure.ErrorCode() != "" || failure.HTTPStatus() != 0 || !strings.Contains(failure.Error(), "without an error message") {
		t.Fatalf("missing metadata was invented or discarded: %v", got)
	}
}

func TestResponsesErrorConcurrentCallsDoNotShareFailure(t *testing.T) {
	var entered sync.WaitGroup
	entered.Add(2)
	p := responsesErrorFixtureProvider(t, func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		code := "fixture_alpha"
		if strings.Contains(string(body), "marker_beta") {
			code = "fixture_beta"
		}
		entered.Done()
		entered.Wait()
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprintf(w, "data: {\"type\":\"error\",\"code\":%q,\"message\":%q}\n\n", code, code)
	}, nil)
	var finished sync.WaitGroup
	for _, label := range []string{"alpha", "beta"} {
		finished.Add(1)
		go func(label string) {
			defer finished.Done()
			_, err := responsesErrorReadStream(t.Context(), p, "marker_"+label)
			var failure *responsesStreamError
			if !errors.As(err, &failure) || failure.ErrorCode() != "fixture_"+label {
				t.Errorf("failure crossed concurrent calls for %s: %v", label, err)
			}
		}(label)
	}
	finished.Wait()
}

type responsesErrorStubProvider struct {
	stream   func(context.Context) iter.Seq2[llm.StreamEvent, error]
	complete func(context.Context) error
}

func (p responsesErrorStubProvider) Stream(ctx context.Context, _ llm.CompletionRequest) iter.Seq2[llm.StreamEvent, error] {
	return p.stream(ctx)
}
func (p responsesErrorStubProvider) Complete(ctx context.Context, _ llm.CompletionRequest) (llm.Message, string, llm.Usage, error) {
	return llm.Message{}, "", llm.Usage{}, p.complete(ctx)
}

func TestResponsesErrorProviderCompleteAndEarlyStopPreserveSemantics(t *testing.T) {
	original := errors.New("fixture nonstream error")
	closed := false
	stub := responsesErrorStubProvider{
		complete: func(context.Context) error { return original },
		stream: func(context.Context) iter.Seq2[llm.StreamEvent, error] {
			return func(yield func(llm.StreamEvent, error) bool) {
				defer func() { closed = true }()
				if !yield(llm.StreamEvent{Type: llm.SETextDelta, Text: "fixture"}, nil) {
					return
				}
				t.Error("provider continued after consumer stopped")
			}
		},
	}
	p := wrapResponsesErrors(stub, Config{Format: llm.FormatOpenAIResponses})
	_, _, _, err := p.Complete(t.Context(), llm.CompletionRequest{})
	if err != original {
		t.Fatalf("Complete replaced a non-generic error: %v", err)
	}
	for range p.Stream(t.Context(), llm.CompletionRequest{}) {
		break
	}
	if !closed {
		t.Fatal("early stop did not unwind provider cleanup")
	}
}
