package agent

import (
	"context"
	"fmt"
	"github.com/Autumn-27/artex/db"
	actool "github.com/Autumn-27/norma/tool"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

type deepSeekRoundTripper func(*http.Request) (*http.Response, error)

func (f deepSeekRoundTripper) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestDeepSeekSearchToolUsesConfiguredKeyAndQuery(t *testing.T) {
	client := &http.Client{Transport: deepSeekRoundTripper(func(r *http.Request) (*http.Response, error) {
		if r.URL.String() != deepSeekToolsURL+"search" {
			t.Errorf("request URL = %q", r.URL)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer test-key" {
			t.Errorf("authorization = %q", got)
		}
		body, _ := io.ReadAll(r.Body)
		if string(body) != `{"count":3,"query":"ARTEX"}` {
			t.Errorf("request body = %s", body)
		}
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(`{"results":[{"title":"ARTEX","url":"https://example.test"}]}`)), Header: make(http.Header)}, nil
	})}
	tool := deepSeekTool("deepseek_search", "", "search", "test-key", client, map[string]any{
		"query": map[string]any{"type": "string"}, "count": map[string]any{"type": "integer"},
	}, "query")
	result, err := tool.Call(context.Background(), []byte(`{"query":"ARTEX","count":3}`), nil)
	if err != nil {
		t.Fatal(err)
	}
	if result.IsError || !strings.Contains(result.Flatten(), `"results"`) {
		t.Fatalf("unexpected result: %+v", result)
	}
}

func TestDeepSeekToolsRespectGlobalAndBackendSettings(t *testing.T) {
	for _, opts := range []WebSearchOpts{
		{Enabled: false, Backend: "deepseek", DeepSeekAPIKey: "key"},
		{Enabled: true, Backend: "ddgs", DeepSeekAPIKey: "key"},
		{Enabled: true, Backend: "deepseek"},
	} {
		if got := deepSeekTools(opts); len(got) != 0 {
			t.Fatalf("deepseek tools enabled for opts %+v", opts)
		}
	}
}

func TestDeepSeekClientDirectIgnoresEnvironmentAndInvalidProxyFailsAssembly(t *testing.T) {
	t.Setenv("HTTPS_PROXY", "http://fixture-environment-proxy.invalid:99")
	client, err := deepSeekHTTPClient("")
	if err != nil {
		t.Fatal(err)
	}
	transport, ok := client.Transport.(*http.Transport)
	if !ok || transport.Proxy != nil {
		t.Fatal("empty search proxy inherited the environment")
	}
	if client.Timeout != 30*time.Second {
		t.Fatal("search lost its call timeout")
	}
	for _, proxy := range []string{"http://", "ftp://proxy.invalid", "http://user:secret@%zz"} {
		tools := deepSeekTools(WebSearchOpts{Enabled: true, Backend: "deepseek", DeepSeekAPIKey: "fixture-key", Proxy: proxy})
		if toolAssemblyError(tools) == nil {
			t.Fatal("invalid proxy silently disabled search or allowed direct fallback")
		}
	}
}

func TestDeepSeekSearchErrorDoesNotEchoProviderBody(t *testing.T) {
	const secret = "fixture-echoed-private-tool-input"
	client := &http.Client{Transport: deepSeekRoundTripper(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 503, Body: io.NopCloser(strings.NewReader(`{"error":"` + secret + `"}`)), Header: make(http.Header)}, nil
	})}
	tool := deepSeekTool("deepseek_search", "", "search", "fixture-key", client, map[string]any{"query": map[string]any{"type": "string"}}, "query")
	result, err := tool.Call(t.Context(), []byte(`{"query":"fixture"}`), nil)
	if err != nil || !result.IsError || !strings.Contains(result.Flatten(), "503") || strings.Contains(result.Flatten(), secret) {
		t.Fatalf("unsafe search error: %+v %v", result, err)
	}
}

func TestDeepSeekInvalidArgumentsMakeNoHTTPRequest(t *testing.T) {
	calls := 0
	client := &http.Client{Transport: deepSeekRoundTripper(func(*http.Request) (*http.Response, error) {
		calls++
		return nil, fmt.Errorf("unexpected HTTP")
	})}
	for _, tc := range []struct{ endpoint, required, input string }{
		{"search", "query", `{}`},
		{"search", "query", `{"query":""}`},
		{"search", "query", `{"query":"fixture","count":1.5}`},
		{"search", "query", `{"query":"fixture","count":11}`},
		{"search", "query", `{"query":"fixture","hidden":"private input"}`},
		{"fetch", "url", `{"url":"file:///etc/hosts"}`},
		{"fetch", "url", `{"url":"https://user:password@fixture.invalid"}`},
	} {
		properties := map[string]any{tc.required: map[string]any{"type": "string"}, "count": map[string]any{"type": "integer"}}
		tool := deepSeekTool("deepseek_"+tc.endpoint, "", tc.endpoint, "fixture-key", client, properties, tc.required)
		result, err := tool.Call(t.Context(), []byte(tc.input), nil)
		if err != nil || !result.IsError {
			t.Fatalf("invalid input accepted: %s: %+v %v", tc.input, result, err)
		}
	}
	if calls != 0 {
		t.Fatalf("invalid arguments made %d requests", calls)
	}
}

func TestDeepSeekChatHonorsRestrictedRolesAndFinalToolResolver(t *testing.T) {
	oldAugment, oldResolve := ToolAugment, ToolResolve
	t.Cleanup(func() { ToolAugment, ToolResolve = oldAugment, oldResolve })
	ToolAugment = nil
	for _, key := range []string{db.CredentialAgentKey, "goals", "asset_mapper", "pentest"} {
		t.Run(key, func(t *testing.T) {
			seen := false
			ToolResolve = func(_ context.Context, _ string, tools []actool.CoreTool) []actool.CoreTool {
				for _, tool := range tools {
					if strings.HasPrefix(tool.Name(), "deepseek_") {
						seen = true
					}
				}
				return []actool.CoreTool{}
			}
			provider := &credentialFixtureProvider{reply: "fixture complete", stop: "end_turn"}
			chat := NewChatAgent(provider, "fixture", t.TempDir(), nil, 0)
			chat.SetNonStreaming(func() bool { return true })
			chat.SetWebSearch(WebSearchOpts{Enabled: true, Backend: "deepseek", DeepSeekAPIKey: "fixture-key"})
			if _, err := chat.Chat(t.Context(), key, "fixture-deepseek", "fixture", 1, 0, true, nil); err != nil {
				t.Fatal(err)
			}
			if seen != (key == "pentest") {
				t.Fatal("restricted role gained search or ordinary search skipped final grants")
			}
			for _, tool := range provider.request.Tools {
				if strings.HasPrefix(tool.Name, "deepseek_") {
					t.Fatal("DeepSeek tools bypassed the final resolver")
				}
			}
		})
	}
}
