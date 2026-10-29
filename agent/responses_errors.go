package agent

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"iter"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"unicode"
	"unicode/utf8"

	"github.com/Autumn-27/norma/llm"
)

// Norma v0.4.3 discards response.error and top-level message/code on SSE
// failures. Observe only bounded diagnostic metadata, leaving both its parser
// and the recorder's original wire bytes unchanged. Remove this compatibility
// layer when the upstream adapter exposes structured Responses failures.
const (
	maxResponsesErrorFrameBytes   = 64 * 1024
	maxResponsesErrorMessageBytes = 2048
	maxResponsesErrorCodeBytes    = 128
)

type responsesErrorContextKey struct{}

type responsesErrorCall struct {
	mu      sync.Mutex
	current *responsesErrorAttempt
	secret  string
}

type responsesErrorAttempt struct {
	call    *responsesErrorCall
	secret  string
	failure *responsesStreamError // protected by call.mu
}

// A streaming request can have HTTP 200 while the response itself fails. Keep
// that distinction: HTTPStatus is zero rather than inventing a non-200 status.
// Unwrap preserves the exact original parser error for errors.Is/errors.As.
type responsesStreamError struct {
	event             string
	code              string
	message           string
	expectedErrorHash [sha256.Size]byte
	cause             error
}

func (e *responsesStreamError) Error() string {
	if e.code == "" {
		return fmt.Sprintf("openai-responses: %s: %s", e.event, e.message)
	}
	return fmt.Sprintf("openai-responses: %s [code=%s]: %s", e.event, e.code, e.message)
}

func (e *responsesStreamError) ErrorCode() string { return e.code }
func (e *responsesStreamError) HTTPStatus() int   { return 0 }
func (e *responsesStreamError) Unwrap() error     { return e.cause }

type responsesErrorProvider struct {
	inner  llm.Provider
	secret string
}

func wrapResponsesErrors(inner llm.Provider, c Config) llm.Provider {
	if inner == nil || c.Format != llm.FormatOpenAIResponses {
		return inner
	}
	return responsesErrorProvider{inner: inner, secret: c.APIKey}
}

func newResponsesErrorCall(ctx context.Context, secret string) (context.Context, *responsesErrorCall) {
	c := &responsesErrorCall{secret: secret}
	return context.WithValue(ctx, responsesErrorContextKey{}, c), c
}

func (p responsesErrorProvider) Stream(ctx context.Context, req llm.CompletionRequest) iter.Seq2[llm.StreamEvent, error] {
	return func(yield func(llm.StreamEvent, error) bool) {
		callCtx, call := newResponsesErrorCall(ctx, p.secret)
		for ev, err := range p.inner.Stream(callCtx, req) {
			if !yield(ev, call.enrich(err)) {
				return
			}
		}
	}
}

func (p responsesErrorProvider) Complete(ctx context.Context, req llm.CompletionRequest) (llm.Message, string, llm.Usage, error) {
	callCtx, call := newResponsesErrorCall(ctx, p.secret)
	msg, stop, usage, err := p.inner.Complete(callCtx, req)
	return msg, stop, usage, call.enrich(err)
}

func (c *responsesErrorCall) enrich(err error) error {
	// Never turn a network, cancellation, budget, timeout or unrelated parser
	// error into an API failure, even if the HTTP reader read ahead to a frame.
	if err == nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.current == nil || c.current.failure == nil ||
		sha256.Sum256([]byte(err.Error())) != c.current.failure.expectedErrorHash {
		return err
	}
	failure := *c.current.failure
	failure.cause = err
	return &failure
}

// Called for each outgoing HTTP attempt, not just once per shared provider.
// An earlier retry's body must never annotate an error from a later attempt.
func startResponsesErrorAttempt(req *http.Request) *responsesErrorAttempt {
	if req == nil || req.Method != http.MethodPost || req.URL == nil || !strings.HasSuffix(req.URL.Path, "/responses") {
		return nil
	}
	c, _ := req.Context().Value(responsesErrorContextKey{}).(*responsesErrorCall)
	if c == nil {
		return nil
	}
	// Norma may resolve a credential from the environment when Config.APIKey is
	// empty. Redact the actual outgoing bearer too, without retaining headers or
	// changing authentication. Keep the value local to this HTTP attempt.
	secret := c.secret
	if scheme, value, ok := strings.Cut(req.Header.Get("Authorization"), " "); ok && strings.EqualFold(scheme, "Bearer") {
		if value = strings.TrimSpace(value); value != "" {
			secret = value
		}
	}
	a := &responsesErrorAttempt{call: c, secret: secret}
	c.mu.Lock()
	c.current = a
	c.mu.Unlock()
	return a
}

func (a *responsesErrorAttempt) observe(status int, body io.ReadCloser) io.ReadCloser {
	if a == nil || status != http.StatusOK || body == nil {
		return body
	}
	return &responsesErrorBody{body: body, attempt: a}
}

type responsesErrorBody struct {
	body       io.ReadCloser
	attempt    *responsesErrorAttempt
	line       []byte
	data       []byte
	event      string
	frameBytes int
	dropping   bool
	lineNonCR  bool
}

func (b *responsesErrorBody) Read(p []byte) (int, error) {
	n, err := b.body.Read(p)
	if n > 0 {
		b.feed(p[:n])
	}
	// Mirror Norma's EOF handling only for fully terminated data lines. Never
	// treat an unfinished line/JSON frame or a network failure as an API error.
	if err == io.EOF && len(b.line) == 0 && !b.dropping && len(b.data) > 0 {
		b.dispatch()
		b.resetFrame()
	}
	return n, err
}

func (b *responsesErrorBody) Close() error { return b.body.Close() }

func (b *responsesErrorBody) feed(chunk []byte) {
	for _, ch := range chunk {
		if ch == '\n' {
			if !b.lineNonCR {
				if !b.dropping {
					b.dispatch()
				}
				b.resetFrame()
				continue
			}
			if !b.dropping {
				b.processLine(bytes.TrimRight(b.line, "\r"))
			}
			b.line = b.line[:0]
			b.lineNonCR = false
			continue
		}
		if ch != '\r' {
			b.lineNonCR = true
		}
		b.frameBytes++
		if b.frameBytes > maxResponsesErrorFrameBytes {
			// Diagnostics are best-effort. Drop oversized frames until the next
			// separator without changing downstream bytes, reads or errors.
			b.dropping = true
			b.line = nil
			b.data = nil
			b.event = ""
		}
		if !b.dropping {
			b.line = append(b.line, ch)
		}
	}
}

func (b *responsesErrorBody) resetFrame() {
	b.line = b.line[:0]
	b.data = b.data[:0]
	b.event = ""
	b.frameBytes = 0
	b.dropping = false
	b.lineNonCR = false
}

func (b *responsesErrorBody) processLine(line []byte) {
	switch {
	case bytes.HasPrefix(line, []byte("event:")):
		b.event = strings.TrimSpace(string(line[len("event:"):]))
	case bytes.HasPrefix(line, []byte("data:")):
		data := bytes.TrimPrefix(line[len("data:"):], []byte(" "))
		if len(b.data) > 0 {
			b.data = append(b.data, '\n')
		}
		b.data = append(b.data, data...)
	}
}

type responsesErrorFields struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

func (b *responsesErrorBody) dispatch() {
	// Named normal events dominate token streams; no second JSON decode is
	// needed for them. Bare data still needs its JSON type discriminator.
	if b.event != "" && b.event != "response.failed" && b.event != "error" {
		return
	}
	var frame struct {
		Type string `json:"type"`
		responsesErrorFields
		Error    *responsesErrorFields `json:"error"`
		Response struct {
			Error *responsesErrorFields `json:"error"`
		} `json:"response"`
	}
	if len(b.data) == 0 || json.Unmarshal(b.data, &frame) != nil {
		return
	}
	event := b.event
	if event == "" {
		event = frame.Type
	}
	if event != "response.failed" && event != "error" {
		return
	}
	fields := frame.responsesErrorFields
	// Match the exact error Norma will emit for THIS frame, without retaining
	// its unsanitized provider message merely for later comparison. Legacy
	// error.message shapes keep their codes too, but unrelated errors do not.
	parserMessage := "stream error"
	if frame.Error != nil {
		fields = *frame.Error
		parserMessage = frame.Error.Message
	}
	if event == "response.failed" && frame.Response.Error != nil {
		fields = *frame.Response.Error
	}
	c := b.attempt.call
	code := sanitizeResponsesError(fields.Code, b.attempt.secret, maxResponsesErrorCodeBytes)
	message := sanitizeResponsesError(fields.Message, b.attempt.secret, maxResponsesErrorMessageBytes)
	if message == "" {
		message = "response stream reported failure without an error message"
	}
	failure := &responsesStreamError{
		event: event, code: code, message: message,
		expectedErrorHash: sha256.Sum256([]byte("openai-responses: " + parserMessage)),
	}
	c.mu.Lock()
	// A single HTTP Read may contain several frames. Norma stops at the first
	// failure; a later read-ahead frame must not overwrite its diagnostics.
	if c.current == b.attempt && b.attempt.failure == nil {
		b.attempt.failure = failure
	}
	c.mu.Unlock()
}

var (
	responsesKeyPattern    = regexp.MustCompile(`(?i)\bsk-[a-z0-9_.~\-]+`)
	responsesBearerPattern = regexp.MustCompile(`(?i)\bBearer[\t ]+[^\s,;"'<>]+`)
	responsesSecretField   = regexp.MustCompile(`(?i)\b(api[_ -]?key|access[_ -]?token|authorization|password|secret|token)["']?\s*[:=]\s*("[^"]*"|'[^']*'|[^\s,;}]+)`)
)

func sanitizeResponsesError(s, secret string, limit int) string {
	if secret != "" {
		s = strings.ReplaceAll(s, secret, "[REDACTED]")
	}
	s = responsesKeyPattern.ReplaceAllString(s, "[REDACTED]")
	s = responsesBearerPattern.ReplaceAllString(s, "Bearer [REDACTED]")
	s = responsesSecretField.ReplaceAllString(s, "$1=[REDACTED]")
	s = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return ' '
		}
		return r
	}, strings.ToValidUTF8(s, "�"))
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > limit {
		s = s[:limit]
		for !utf8.ValidString(s) {
			s = s[:len(s)-1]
		}
	}
	return s
}
