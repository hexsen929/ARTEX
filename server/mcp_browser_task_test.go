package server

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/Autumn-27/artex/db"
	actool "github.com/Autumn-27/norma/tool"
)

func taskBrowserFixture(args ...string) *db.MCPServer {
	if len(args) == 0 {
		args = []string{"-y", "@playwright/mcp", "--headless"}
	}
	return &db.MCPServer{Name: browserMCPName, Transport: "stdio", Command: "npx", Args: encodeJSON(args), Env: json.RawMessage(`{"USER_SETTING":"preserved"}`), Enabled: true}
}

func taskBrowserConfigPath(t *testing.T, server *db.MCPServer) string {
	t.Helper()
	args := jsonStrSlice(server.Args)
	for i := 0; i+1 < len(args); i++ {
		if args[i] == "--config" {
			return args[i+1]
		}
	}
	t.Fatal("private config argument missing")
	return ""
}

func readTaskBrowserFixture(t *testing.T, path string) map[string]any {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var value map[string]any
	if err = json.Unmarshal(raw, &value); err != nil {
		t.Fatal(err)
	}
	return value
}

func TestTaskBrowserMCPConfigIsPrivateAndPreservesUserOptions(t *testing.T) {
	root := t.TempDir()
	originalConfig := filepath.Join(root, "user.json")
	original := `{"browser":{"browserName":"firefox","launchOptions":{"headless":true,"slowMo":12,"proxy":{"server":"http://old.invalid"}},"contextOptions":{"locale":"zh-CN"}},"snapshot":{"mode":"full"}}`
	if err := os.WriteFile(originalConfig, []byte(original), 0o600); err != nil {
		t.Fatal(err)
	}
	server := taskBrowserFixture("-y", "@playwright/mcp@latest", "--headless", "--proxy-server", "http://old.invalid", "--proxy-bypass=example.invalid", "--config", originalConfig)
	beforeArgs, beforeEnv := string(server.Args), string(server.Env)
	privateDir := filepath.Join(root, "mcp-private")
	scoped, cleanup, err := prepareTaskBrowserMCP(server, 17, privateDir, "http://fixture:private-password@127.0.0.1:21117", "/fixture/task-ca.pem")
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	if string(server.Args) != beforeArgs || string(server.Env) != beforeEnv {
		t.Fatal("original MCP descriptor was changed")
	}
	path := taskBrowserConfigPath(t, scoped)
	cfg := readTaskBrowserFixture(t, path)
	browser := cfg["browser"].(map[string]any)
	launch := browser["launchOptions"].(map[string]any)
	proxy := launch["proxy"].(map[string]any)
	if browser["browserName"] != "firefox" || browser["isolated"] != true || launch["headless"] != true || launch["slowMo"] != float64(12) || browser["contextOptions"].(map[string]any)["locale"] != "zh-CN" || cfg["snapshot"].(map[string]any)["mode"] != "full" {
		t.Fatal("custom non-proxy options lost")
	}
	if proxy["server"] != "http://127.0.0.1:21117" || proxy["username"] != "fixture" || proxy["password"] != "private-password" {
		t.Fatal("task route or split credentials lost")
	}
	if strings.Contains(string(scoped.Args), "private-password") || strings.Contains(string(scoped.Args), "old.invalid") || !contains(jsonStrSlice(scoped.Args), "--isolated") {
		t.Fatal("credentials/old route leaked into CLI or isolation missing")
	}
	if env := jsonStrMap(scoped.Env); env["USER_SETTING"] != "preserved" || env["NODE_EXTRA_CA_CERTS"] != "/fixture/task-ca.pem" {
		t.Fatal("task CA or user environment lost")
	}
	if runtime.GOOS != "windows" {
		for _, item := range []struct {
			path string
			mode os.FileMode
		}{{privateDir, 0o700}, {path, 0o600}} {
			info, e := os.Stat(item.path)
			if e != nil || info.Mode().Perm() != item.mode {
				t.Fatalf("private config permissions: %v", e)
			}
		}
	}
	cleanup()
	if _, err = os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("private config not removed")
	}
	if raw, _ := os.ReadFile(originalConfig); string(raw) != original {
		t.Fatal("custom config was overwritten/deleted")
	}
}

func TestTaskBrowserMCPConnectionsDoNotShareConfigOrCleanup(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "mcp-private")
	server := taskBrowserFixture()
	first, closeFirst, err := prepareTaskBrowserMCP(server, 1, dir, "http://127.0.0.1:21101", "")
	if err != nil {
		t.Fatal(err)
	}
	defer closeFirst()
	second, closeSecond, err := prepareTaskBrowserMCP(server, 2, dir, "http://127.0.0.1:21102", "")
	if err != nil {
		t.Fatal(err)
	}
	defer closeSecond()
	a, b := taskBrowserConfigPath(t, first), taskBrowserConfigPath(t, second)
	if a == b {
		t.Fatal("different runs share mutable browser config")
	}
	closeFirst()
	if _, err = os.Stat(b); err != nil {
		t.Fatal("one run deleted another's config")
	}
	if raw, _ := os.ReadFile(b); !strings.Contains(string(raw), "21102") || strings.Contains(string(raw), "21101") {
		t.Fatal("task routes crossed")
	}
}

func TestTaskBrowserMCPRejectsUnsupportedCustomModes(t *testing.T) {
	for _, test := range []struct {
		name   string
		args   []string
		config string
		proxy  string
	}{
		{"cdp", []string{"-y", "@playwright/mcp", "--cdp-endpoint", "http://fixture.invalid"}, "", "http://127.0.0.1:21101"},
		{"extension", []string{"-y", "@playwright/mcp", "--extension"}, "", "http://127.0.0.1:21101"},
		{"persistent", []string{"-y", "@playwright/mcp", "--user-data-dir", "/fixture/shared"}, "", "http://127.0.0.1:21101"},
		{"config-remote", nil, `{"browser":{"cdpEndpoint":"http://fixture.invalid"}}`, "http://127.0.0.1:21101"},
		{"config-extension", nil, `{"extension":true}`, "http://127.0.0.1:21101"},
		{"config-persistent", nil, `{"browser":{"userDataDir":"/fixture/shared"}}`, "http://127.0.0.1:21101"},
		{"invalid-config", nil, `[]`, "http://127.0.0.1:21101"},
		{"config-conflicting-switch", nil, `{"browser":{"launchOptions":{"args":["--no-proxy-server"]}}}`, "http://127.0.0.1:21101"},
		{"authenticated-socks", nil, "", "socks5://fixture:password@127.0.0.1:21101"},
	} {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			server := taskBrowserFixture(test.args...)
			if test.config != "" {
				path := filepath.Join(root, "custom.json")
				if err := os.WriteFile(path, []byte(test.config), 0o600); err != nil {
					t.Fatal(err)
				}
				server.Args = encodeJSON(append(jsonStrSlice(server.Args), "--config", path))
			}
			private := filepath.Join(root, "private")
			_, cleanup, err := prepareTaskBrowserMCP(server, 1, private, test.proxy, "")
			if err == nil {
				if cleanup != nil {
					cleanup()
				}
				t.Fatal("unsupported routing mode accepted")
			}
			if _, err = os.Stat(private); !os.IsNotExist(err) {
				t.Fatal("failed validation left private credentials on disk")
			}
		})
	}
}

func TestTaskBrowserMCPHonorsConfigEnvironmentWithoutLettingItOverrideRoute(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "custom.json")
	original := `{"browser":{"initPage":["scripts/init.ts"],"initScript":["scripts/init.js"],"contextOptions":{"storageState":"state.json"}}}`
	if err := os.WriteFile(path, []byte(original), 0o600); err != nil {
		t.Fatal(err)
	}
	server := taskBrowserFixture()
	server.Env = encodeJSON(map[string]string{"PLAYWRIGHT_MCP_CONFIG": path, "PLAYWRIGHT_MCP_PROXY_SERVER": "http://old.invalid:1234", "PLAYWRIGHT_MCP_PROXY_BYPASS": "*", "PLAYWRIGHT_MCP_ISOLATED": "false", "USER_SETTING": "preserved"})
	before := string(server.Env)
	scoped, cleanup, err := prepareTaskBrowserMCP(server, 4, filepath.Join(root, "private"), "http://fixture:private-password@127.0.0.1:21104", "")
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	env := jsonStrMap(scoped.Env)
	if env["PLAYWRIGHT_MCP_PROXY_SERVER"] != "" || env["PLAYWRIGHT_MCP_PROXY_BYPASS"] != "" || env["PLAYWRIGHT_MCP_ISOLATED"] != "true" || env["PLAYWRIGHT_MCP_CONFIG"] == path {
		t.Fatal("environment can override private route/isolation/config")
	}
	if string(server.Env) != before {
		t.Fatal("persisted environment mutated")
	}
	browser := readTaskBrowserFixture(t, taskBrowserConfigPath(t, scoped))["browser"].(map[string]any)
	if browser["initPage"].([]any)[0] != filepath.Join(root, "scripts/init.ts") || browser["initScript"].([]any)[0] != filepath.Join(root, "scripts/init.js") {
		t.Fatal("moving the config changed init script resolution")
	}
	if browser["contextOptions"].(map[string]any)["storageState"] != "state.json" {
		t.Fatal("unrelated working-directory path changed")
	}
	if raw, _ := os.ReadFile(path); string(raw) != original {
		t.Fatal("custom config changed")
	}
}

func TestTaskBrowserMCPRejectsEnvironmentRemoteModes(t *testing.T) {
	for _, key := range []string{"PLAYWRIGHT_MCP_CDP_ENDPOINT", "PLAYWRIGHT_MCP_USER_DATA_DIR", "PLAYWRIGHT_MCP_EXTENSION", "PLAYWRIGHT_MCP_PORT"} {
		t.Run(key, func(t *testing.T) {
			server := taskBrowserFixture()
			server.Env = encodeJSON(map[string]string{key: "enabled-fixture"})
			_, cleanup, err := prepareTaskBrowserMCP(server, 1, filepath.Join(t.TempDir(), "private"), "http://127.0.0.1:21101", "")
			if err == nil {
				if cleanup != nil {
					cleanup()
				}
				t.Fatal("shared/remote env mode accepted")
			}
		})
	}
}

func TestTaskBrowserMCPDoesNotChangeNonTaskOrOtherServerDialers(t *testing.T) {
	for _, test := range []struct {
		name string
		id   int64
	}{{browserMCPName, 0}, {"jshook", 7}} {
		t.Run(test.name, func(t *testing.T) {
			server := taskBrowserFixture()
			server.Name = test.name
			session := newLazyMCPSession(context.Background(), server)
			defer session.Close()
			var ordinary, scoped atomic.Int32
			name := "mcp__" + test.name + "__fixture"
			client := &lifecycleFakeClient{tools: []actool.CoreTool{lifecycleTool(name, func(context.Context, json.RawMessage, *actool.ToolContext) (actool.Result, error) {
				return actool.Text("ok"), nil
			})}}
			session.dial = func(context.Context, *db.MCPServer) (mcpClient, error) { ordinary.Add(1); return client, nil }
			attachTaskMCPDialer(session, test.id, func(context.Context, *db.MCPServer, int64) (mcpClient, error) {
				scoped.Add(1)
				return nil, errors.New("unexpected")
			})
			res, err := session.tool(lifecycleMetadata(name)).Call(context.Background(), json.RawMessage(`{}`), nil)
			if err != nil || res.IsError || ordinary.Load() != 1 || scoped.Load() != 0 {
				t.Fatal("ordinary global/other MCP routing changed")
			}
		})
	}
}

func TestTaskBrowserMCPMetadataNeverDialsAndCleanupFollowsSession(t *testing.T) {
	server := taskBrowserFixture()
	session := newLazyMCPSession(context.Background(), server)
	defer session.Close()
	var starts, calls atomic.Int32
	dir := filepath.Join(t.TempDir(), "mcp-private")
	var privateFile string
	name := "mcp__browser__browser_snapshot"
	client := &lifecycleFakeClient{tools: []actool.CoreTool{lifecycleTool(name, func(context.Context, json.RawMessage, *actool.ToolContext) (actool.Result, error) {
		calls.Add(1)
		return actool.Text("fixture"), nil
	})}}
	attachTaskMCPDialer(session, 7, func(ctx context.Context, m *db.MCPServer, id int64) (mcpClient, error) {
		return dialTaskBrowserMCP(ctx, m, id, dir, "http://127.0.0.1:21107", "", func(_ context.Context, scoped *db.MCPServer) (mcpClient, error) {
			starts.Add(1)
			privateFile = taskBrowserConfigPath(t, scoped)
			return client, nil
		})
	})
	tool := session.tool(lifecycleMetadata(name))
	_ = tool.Name()
	_ = tool.Description()
	_ = tool.InputSchema()
	if starts.Load() != 0 {
		t.Fatal("reading metadata started a process")
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatal("metadata created route state")
	}
	for range 2 {
		res, err := tool.Call(context.Background(), json.RawMessage(`{}`), nil)
		if err != nil || res.IsError {
			t.Fatalf("call failed: %v %+v", err, res)
		}
	}
	if starts.Load() != 1 || calls.Load() != 2 {
		t.Fatal("browser state was not reused privately within the run")
	}
	if _, err := os.Stat(privateFile); err != nil {
		t.Fatal("config removed before session completion")
	}
	_ = session.Close()
	_ = session.Close()
	if client.closed.Load() != 1 {
		t.Fatal("underlying client closed repeatedly")
	}
	if _, err := os.Stat(privateFile); !os.IsNotExist(err) {
		t.Fatal("private browser config survived session cleanup")
	}
}

func TestTaskBrowserMCPFailedOrCancelledDialCleansOnlyItsFiles(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "mcp-private")
	server := taskBrowserFixture()
	var starts atomic.Int32
	dial := func(context.Context, *db.MCPServer) (mcpClient, error) {
		starts.Add(1)
		return nil, errors.New("fixture startup failure")
	}
	if _, err := dialTaskBrowserMCP(context.Background(), server, 1, dir, "http://127.0.0.1:21101", "", dial); err == nil {
		t.Fatal("startup error lost")
	}
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 0 {
		t.Fatal("startup error leaked route config")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err = dialTaskBrowserMCP(ctx, server, 2, dir, "http://127.0.0.1:21102", "", dial); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if starts.Load() != 1 {
		t.Fatal("cancelled call spawned a browser")
	}
}
