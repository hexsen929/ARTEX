package agent

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Autumn-27/norma/llm"
)

// The local Codex adapter must not replace the upstream profile-wide limiter
// or forget request history when a provider is reconstructed.
func TestCodexPresetRetainsSharedRequestLimiter(t *testing.T) {
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.Header.Get("User-Agent") != UserAgentCodex {
			t.Errorf("Codex user agent lost")
		}
		w.Header().Set("Content-Type", "text/event-stream")
		writeCodexTestFrame(w, "response.output_text.delta", map[string]any{"delta": "OK"})
		writeCodexTestCompleted(w)
	}))
	defer upstream.Close()
	cfg := ConfigFrom("openai-responses", "test-model", upstream.URL+"/v1", "fixture-key", "")
	cfg.UserAgent, cfg.RatePerMinute = "codex", 1
	limiter := &RequestLimiter{}
	request := llm.CompletionRequest{Messages: []llm.Message{llm.UserText("fixture")}}
	first, err := cfg.NewProviderWithLimiter(limiter)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := first.Complete(t.Context(), request); err != nil {
		t.Fatal(err)
	}
	second, err := cfg.NewProviderWithLimiter(limiter)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()
	if _, _, _, err := second.Complete(ctx, request); err == nil {
		t.Fatal("rebuilt provider bypassed the retained quota")
	}
	if calls.Load() != 1 {
		t.Fatalf("upstream calls=%d, want 1", calls.Load())
	}
}
