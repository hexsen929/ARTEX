package server

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Autumn-27/artex/agent"
	"github.com/Autumn-27/artex/db"
)

// Exercise the HTTP handlers and actual outgoing requests: an omitted option
// inherits the saved profile, while an explicitly empty option selects Go's UA.
func TestLLMProbeUserAgentInheritance(t *testing.T) {
	dsn, _, err := db.DSN()
	if err != nil {
		t.Skipf("no database config (%v) — skipping", err)
	}
	d, err := db.Open(dsn)
	if err != nil {
		t.Skipf("postgres unavailable (%v) — skipping", err)
	}
	t.Cleanup(func() { d.Close() })
	s := &Server{m: &Manager{pg: d}}

	const storedUA = "stored-probe/1.0"
	const overrideUA = "custom-probe/2.0"
	cases := []struct {
		name      string
		userAgent any
		includeUA bool
		context1M any
		include1M bool
		wantUA    string
		want1M    bool
	}{
		{name: "omitted", wantUA: storedUA, want1M: true},
		{name: "empty", includeUA: true, userAgent: "", wantUA: "Go-http-client/1.1", want1M: true},
		{name: "whitespace", includeUA: true, userAgent: "  ", wantUA: "Go-http-client/1.1", want1M: true},
		{name: "override", includeUA: true, userAgent: overrideUA, wantUA: overrideUA, want1M: true},
		{name: "codex_preset", includeUA: true, userAgent: "codex", wantUA: agent.UserAgentCodex, want1M: true},
		{name: "claude_code_preset", includeUA: true, userAgent: "claude-code", wantUA: agent.UserAgentClaudeCode, want1M: true},
		{name: "explicit_defaults", includeUA: true, userAgent: "", include1M: true, context1M: false, wantUA: "Go-http-client/1.1"},
	}
	for _, provider := range []string{"openai", "anthropic"} {
		t.Run(provider, func(t *testing.T) {
			id, err := d.SaveProfile(&db.LLMProfile{
				Name: "t-probe-ua-" + provider, Format: provider, Model: "test-model",
				APIKey: "fixture-key", UserAgent: storedUA, Context1M: true,
			})
			if err != nil {
				t.Fatalf("save probe profile: %v", err)
			}
			t.Cleanup(func() {
				if _, err := d.Exec(`DELETE FROM llm_profiles WHERE id=$1`, id); err != nil {
					t.Errorf("delete probe profile: %v", err)
				}
			})
			for _, endpoint := range []string{"test", "models"} {
				for _, tc := range cases {
					t.Run(endpoint+"/"+tc.name, func(t *testing.T) {
						headers := make(chan http.Header, 1)
						upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
							headers <- r.Header.Clone()
							w.Header().Set("Content-Type", "application/json")
							if endpoint == "models" {
								_, _ = io.WriteString(w, `{"data":[{"id":"test-model"}]}`)
							} else if provider == "anthropic" {
								_, _ = io.WriteString(w, `{"id":"msg_test","type":"message","role":"assistant","model":"test-model","content":[{"type":"text","text":"OK"}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`)
							} else {
								_, _ = io.WriteString(w, `{"id":"chat_test","object":"chat.completion","model":"test-model","choices":[{"index":0,"message":{"role":"assistant","content":"OK"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`)
							}
						}))
						defer upstream.Close()
						body := map[string]any{
							"provider": provider, "model": "test-model", "base_url": upstream.URL,
							"profile_id": id, "streaming": false,
						}
						if tc.includeUA {
							body["user_agent"] = tc.userAgent
						}
						if tc.include1M {
							body["context_1m"] = tc.context1M
						}
						raw, err := json.Marshal(body)
						if err != nil {
							t.Fatal(err)
						}
						req := httptest.NewRequest(http.MethodPost, "/api/llm/"+endpoint, bytes.NewReader(raw))
						rec := httptest.NewRecorder()
						if endpoint == "test" {
							s.testLLM(rec, req)
						} else {
							s.pgListModels(rec, req)
						}
						var result struct {
							OK bool `json:"ok"`
						}
						if err := json.Unmarshal(rec.Body.Bytes(), &result); err != nil || rec.Code != http.StatusOK || !result.OK {
							t.Fatalf("probe failed: status=%d response=%s decode=%v", rec.Code, rec.Body.String(), err)
						}
						var got http.Header
						select {
						case got = <-headers:
						default:
							t.Fatal("probe never reached the local upstream")
						}
						if ua := got.Get("User-Agent"); ua != tc.wantUA {
							t.Fatalf("User-Agent=%q, want %q", ua, tc.wantUA)
						}
						wantBeta := ""
						if provider == "anthropic" && tc.want1M {
							wantBeta = "context-1m-2025-08-07"
						}
						if beta := got.Get("anthropic-beta"); beta != wantBeta {
							t.Fatalf("anthropic-beta=%q, want %q", beta, wantBeta)
						}
					})
				}
			}
		})
	}
}
