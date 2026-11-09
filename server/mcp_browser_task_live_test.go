package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Autumn-27/artex/db"
	actool "github.com/Autumn-27/norma/tool"
)

// This opt-in cloud test drives real Playwright MCP processes. Its proxies serve
// only synthetic responses and never forward traffic to any external target.
func TestTaskBrowserMCPCloakConcurrentRealBrowsers(t *testing.T) {
	entry, executable := os.Getenv("ARTEX_TEST_BROWSER_MCP_CLI"), os.Getenv("ARTEX_TEST_BROWSER_EXECUTABLE")
	if entry == "" || executable == "" {
		t.Skip("requires the isolated cloud Playwright MCP runtime")
	}
	if os.Getenv("CI") == "" {
		t.Fatal("real browser regression must run in cloud CI")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()
	var mu sync.Mutex
	seen := map[string][]string{}
	proxy := func(route string) *httptest.Server {
		return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method != "GET" || r.URL.Host != "artex-browser-fixture.invalid" {
				http.Error(w, "fixture proxy does not forward requests", http.StatusForbidden)
				return
			}
			mu.Lock()
			seen[route] = append(seen[route], r.URL.Path)
			mu.Unlock()
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			fmt.Fprintf(w, "<!doctype html><title>%s</title><body>synthetic browser isolation fixture</body>", route)
		}))
	}
	firstProxy, secondProxy := proxy("route-one"), proxy("route-two")
	defer firstProxy.Close()
	defer secondProxy.Close()
	root := t.TempDir()
	configPath := filepath.Join(root, "cloak.json")
	original, err := json.Marshal(map[string]any{
		"browser": map[string]any{
			"browserName": "chromium",
			"launchOptions": map[string]any{
				"headless": true, "executablePath": executable,
				"args": []string{"--no-sandbox"}, // Explicit only for the cloud test browser.
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(configPath, original, 0o600); err != nil {
		t.Fatal(err)
	}
	server := &db.MCPServer{
		Name: "browser-cloak", Transport: "stdio", Command: "node", Enabled: true,
		Args: encodeJSON([]string{entry, "--config", configPath}), Env: encodeJSON(map[string]string{}),
	}
	const toolPrefix = "mcp__browser-cloak__"
	type run struct {
		session *lazyMCPSession
		id      int64
		owner   string
		proxy   string
		route   string
	}
	runs := []run{
		{id: 7, owner: "worker-A", proxy: firstProxy.URL, route: "route-one"},
		{id: 7, owner: "worker-B", proxy: firstProxy.URL, route: "route-one"},
		{id: 8, owner: "worker-C", proxy: secondProxy.URL, route: "route-two"},
	}
	privateDir := filepath.Join(root, "mcp-private")
	for i := range runs {
		item := &runs[i]
		item.session = newLazyMCPSession(ctx, server)
		t.Cleanup(func() { _ = item.session.Close() })
		// A missed alias must fail here rather than contact an unscoped browser.
		item.session.dial = func(context.Context, *db.MCPServer) (mcpClient, error) {
			return nil, fmt.Errorf("browser-cloak did not receive task routing")
		}
		attachTaskMCPDialer(item.session, item.id, func(ctx context.Context, m *db.MCPServer, id int64) (mcpClient, error) {
			return dialTaskBrowserMCP(ctx, m, id, privateDir, item.proxy, "", connectMCP)
		})
	}
	call := func(item run, name string, params map[string]any) (string, error) {
		input, err := json.Marshal(params)
		if err != nil {
			return "", err
		}
		result, err := item.session.tool(lifecycleMetadata(toolPrefix+name)).Call(ctx, input, &actool.ToolContext{WorkingDir: root})
		if err != nil {
			return "", err
		}
		var text strings.Builder
		for _, block := range result.Content {
			text.WriteString(block.Text)
		}
		if result.IsError {
			return "", fmt.Errorf("%s: %s", name, text.String())
		}
		return text.String(), nil
	}
	// Workers A and B belong to the same task; C must use another task route.
	results := make(chan error, len(runs))
	for _, item := range runs {
		go func() {
			_, err := call(item, "browser_navigate", map[string]any{"url": "http://artex-browser-fixture.invalid/" + item.owner})
			results <- err
		}()
	}
	for range runs {
		if err := <-results; err != nil {
			t.Fatal(err)
		}
	}
	paths, err := os.ReadDir(privateDir)
	if err != nil || len(paths) != len(runs) {
		t.Fatalf("each connection needs a separate private config: count=%d error=%v", len(paths), err)
	}
	for _, item := range runs {
		fn := `() => { if (document.cookie || localStorage.getItem('owner')) throw new Error('state crossed workers'); document.cookie='owner=` + item.owner + `; path=/'; localStorage.setItem('owner',` + strconv.Quote(item.owner) + `); return 'initialized'; }`
		if _, err := call(item, "browser_evaluate", map[string]any{"function": fn}); err != nil {
			t.Fatal(err)
		}
	}
	verify := func(item run) {
		t.Helper()
		fn := `() => { if (document.cookie !== 'owner=` + item.owner + `' || localStorage.getItem('owner') !== ` + strconv.Quote(item.owner) + ` || location.pathname !== '/` + item.owner + `' || document.title !== ` + strconv.Quote(item.route) + `) throw new Error('browser ownership or route crossed'); return 'ownership-verified'; }`
		text, err := call(item, "browser_evaluate", map[string]any{"function": fn})
		if err != nil || !strings.Contains(text, "ownership-verified") {
			t.Fatalf("%s state mismatch: %v %s", item.owner, err, text)
		}
	}
	for _, item := range runs {
		verify(item)
	}
	if err := runs[0].session.Close(); err != nil {
		t.Fatal(err)
	}
	verify(runs[1])
	verify(runs[2])
	paths, err = os.ReadDir(privateDir)
	if err != nil || len(paths) != 2 {
		t.Fatal("closing one browser removed another run's private configuration")
	}
	for _, item := range runs[1:] {
		if err := item.session.Close(); err != nil {
			t.Fatal(err)
		}
	}
	paths, err = os.ReadDir(privateDir)
	if err != nil || len(paths) != 0 {
		t.Fatal("browser cleanup left private task configuration")
	}
	if raw, _ := os.ReadFile(configPath); string(raw) != string(original) {
		t.Fatal("shared Cloak configuration changed")
	}
	mu.Lock()
	defer mu.Unlock()
	for _, item := range runs {
		if !contains(seen[item.route], "/"+item.owner) {
			t.Fatalf("worker %s did not use its task proxy", item.owner)
		}
	}
	if contains(seen["route-one"], "/worker-C") || contains(seen["route-two"], "/worker-A") || contains(seen["route-two"], "/worker-B") {
		t.Fatal("task requests crossed proxy routes")
	}
	t.Log("three concurrent MCP browsers kept per-worker cookies, storage, pages, task proxy routes, and independent cleanup")
}
