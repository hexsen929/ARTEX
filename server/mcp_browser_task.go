package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/Autumn-27/artex/db"
)

// taskMCPDialer is consulted only by a run's actual lazy browser connection.
// Metadata discovery continues to use the unchanged, persisted MCP descriptor.
type taskMCPDialer func(context.Context, *db.MCPServer, int64) (mcpClient, error)

func attachTaskMCPDialer(session *lazyMCPSession, taskID int64, dial taskMCPDialer) {
	if taskID <= 0 || dial == nil || session.server.Name != browserMCPName {
		return
	}
	session.dial = func(ctx context.Context, server *db.MCPServer) (mcpClient, error) {
		return dial(ctx, server, taskID)
	}
}

// taskMCPDial never writes a task's route or secret into mcp_servers/global
// browser config. Each run receives its own isolated browser and private JSON.
func (s *Server) taskMCPDial(ctx context.Context, server *db.MCPServer, taskID int64) (mcpClient, error) {
	if server == nil {
		return nil, errors.New("任务浏览器 MCP 配置缺失")
	}
	if taskID <= 0 || server.Name != browserMCPName {
		return connectMCP(ctx, server)
	}
	if s.m == nil || s.m.dir == "" {
		return nil, errors.New("任务浏览器的路由管理器未就绪")
	}
	proxy, cert, err := s.m.TaskProxyForTaskChecked(ctx, taskID)
	if err != nil {
		return nil, fmt.Errorf("任务浏览器代理未就绪: %w", err)
	}
	if proxy == "" {
		proxy, cert = s.m.ProxyAddr(), s.m.ProxyCACert()
	}
	return dialTaskBrowserMCP(ctx, server, taskID, filepath.Join(s.m.dir, "mcp-private"), proxy, cert, connectMCP)
}

type taskBrowserClient struct {
	mcpClient
	once    sync.Once
	cleanup func()
	err     error
}

func (c *taskBrowserClient) Close() error {
	c.once.Do(func() {
		c.err = c.mcpClient.Close()
		c.cleanup()
	})
	return c.err
}

func dialTaskBrowserMCP(ctx context.Context, server *db.MCPServer, taskID int64, privateDir, proxy, cert string, dial func(context.Context, *db.MCPServer) (mcpClient, error)) (mcpClient, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	scoped, cleanup, err := prepareTaskBrowserMCP(server, taskID, privateDir, proxy, cert)
	if err != nil {
		return nil, err
	}
	client, err := dial(ctx, scoped)
	if err != nil {
		cleanup()
		return nil, err
	}
	return &taskBrowserClient{mcpClient: client, cleanup: cleanup}, nil
}

func knownPlaywrightMCP(server *db.MCPServer, args []string) bool {
	command := strings.ToLower(filepath.Base(server.Command))
	if command == "playwright-mcp" || command == "playwright-mcp.cmd" {
		return true
	}
	for _, arg := range args {
		if arg == "@playwright/mcp" || strings.HasPrefix(arg, "@playwright/mcp@") || strings.Contains(filepath.ToSlash(arg), "/@playwright/mcp/") || arg == "playwright-mcp" {
			return true
		}
	}
	return false
}

func effectiveTaskBrowserEnv(server *db.MCPServer, key string) string {
	if value, ok := jsonStrMap(server.Env)[key]; ok {
		return strings.TrimSpace(value)
	}
	return strings.TrimSpace(os.Getenv(key))
}

func browserTaskArgs(args []string) (out []string, configPath string, err error) {
	for i := 0; i < len(args); i++ {
		arg := args[i]
		switch {
		case arg == "--config":
			if i+1 >= len(args) || strings.HasPrefix(args[i+1], "--") || args[i+1] == "" {
				return nil, "", errors.New("任务浏览器的自定义配置参数不完整")
			}
			i++
			configPath = args[i] // Commander uses the last --config occurrence.
		case strings.HasPrefix(arg, "--config="):
			configPath = strings.TrimPrefix(arg, "--config=")
			if configPath == "" {
				return nil, "", errors.New("任务浏览器的自定义配置参数不完整")
			}
		case arg == "--user-data-dir" || strings.HasPrefix(arg, "--user-data-dir="):
			return nil, "", errors.New("任务浏览器要求独立会话，请移除共享 user-data-dir 并使用 isolated 模式")
		case arg == "--extension" || strings.HasPrefix(arg, "--extension=") || arg == "--cdp-endpoint" || strings.HasPrefix(arg, "--cdp-endpoint=") || arg == "--remote-endpoint" || strings.HasPrefix(arg, "--remote-endpoint="):
			return nil, "", errors.New("任务浏览器的 CDP、extension 或远程连接模式不支持自动代理和会话隔离")
		case arg == "--isolated=false":
			return nil, "", errors.New("任务浏览器要求 isolated 模式")
		case arg == "--isolated=true":
			out = append(out, "--isolated")
		case arg == "--port" || strings.HasPrefix(arg, "--port="):
			return nil, "", errors.New("任务浏览器需要 stdio 模式，不能同时设置监听端口")
		default:
			out = append(out, arg)
		}
	}
	return out, configPath, nil
}

func browserConfigObject(parent map[string]any, key string) (map[string]any, error) {
	value, exists := parent[key]
	if !exists || value == nil {
		result := map[string]any{}
		parent[key] = result
		return result, nil
	}
	object, ok := value.(map[string]any)
	if !ok {
		return nil, errors.New("任务浏览器的自定义 JSON 配置结构不受支持")
	}
	return object, nil
}

func readTaskBrowserConfig(path string) (map[string]any, error) {
	if path == "" {
		return map[string]any{}, nil
	}
	info, err := os.Stat(path)
	if err != nil || !info.Mode().IsRegular() || info.Size() > 2*1024*1024 {
		return nil, errors.New("任务浏览器的自定义 JSON 配置不可读取或大小超限")
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, errors.New("任务浏览器的自定义 JSON 配置不可读取")
	}
	defer file.Close()
	decoder := json.NewDecoder(io.LimitReader(file, 2*1024*1024+1))
	decoder.UseNumber()
	var cfg map[string]any
	if err = decoder.Decode(&cfg); err != nil || cfg == nil {
		return nil, errors.New("任务浏览器的自定义配置必须是 JSON 对象")
	}
	var extra any
	if err = decoder.Decode(&extra); err != io.EOF {
		return nil, errors.New("任务浏览器的自定义配置必须是单个 JSON 对象")
	}
	return cfg, nil
}

func prepareTaskBrowserMCP(server *db.MCPServer, taskID int64, privateDir, rawProxy, cert string) (*db.MCPServer, func(), error) {
	if server == nil || taskID <= 0 || server.Name != browserMCPName || server.Transport != "stdio" {
		return nil, nil, errors.New("任务浏览器代理仅支持独立的 Playwright stdio MCP")
	}
	args := jsonStrSlice(server.Args)
	if !knownPlaywrightMCP(server, args) {
		return nil, nil, errors.New("当前 browser MCP 启动器不支持自动任务代理，请配置独立 Playwright stdio MCP")
	}
	args, configPath, err := browserTaskArgs(args)
	if err != nil {
		return nil, nil, err
	}
	for _, key := range []string{"PLAYWRIGHT_MCP_CDP_ENDPOINT", "PLAYWRIGHT_MCP_REMOTE_ENDPOINT", "PLAYWRIGHT_MCP_USER_DATA_DIR"} {
		if effectiveTaskBrowserEnv(server, key) != "" {
			return nil, nil, errors.New("任务浏览器的环境配置启用了共享或远程浏览器模式")
		}
	}
	if extension := strings.ToLower(effectiveTaskBrowserEnv(server, "PLAYWRIGHT_MCP_EXTENSION")); extension != "" && extension != "false" && extension != "0" {
		return nil, nil, errors.New("任务浏览器的环境配置启用了 extension 模式")
	}
	if port := effectiveTaskBrowserEnv(server, "PLAYWRIGHT_MCP_PORT"); port != "" && port != "0" {
		return nil, nil, errors.New("任务浏览器的环境配置启用了独立监听模式")
	}
	if configPath == "" {
		configPath = effectiveTaskBrowserEnv(server, "PLAYWRIGHT_MCP_CONFIG")
	}
	cfg, err := readTaskBrowserConfig(configPath)
	if err != nil {
		return nil, nil, err
	}
	if extension := cfg["extension"]; extension != nil && extension != false {
		return nil, nil, errors.New("任务浏览器的自定义配置启用了 extension 模式")
	}
	if settings, ok := cfg["server"].(map[string]any); ok && settings["port"] != nil {
		return nil, nil, errors.New("任务浏览器的自定义配置启用了独立监听模式")
	}
	browser, err := browserConfigObject(cfg, "browser")
	if err != nil {
		return nil, nil, err
	}
	for _, key := range []string{"userDataDir", "cdpEndpoint", "remoteEndpoint", "browserServerEndpoint", "extension"} {
		if value := browser[key]; value != nil && value != "" && value != false {
			return nil, nil, errors.New("任务浏览器的持久、CDP 或远程自定义配置不支持自动会话隔离")
		}
	}
	// Playwright resolves these script paths relative to the config file.
	// Moving a private copy must not change where a user's scripts are loaded.
	if configPath != "" {
		absolute, err := filepath.Abs(configPath)
		if err != nil {
			return nil, nil, errors.New("任务浏览器的自定义配置路径无效")
		}
		for _, key := range []string{"initPage", "initScript"} {
			if value := browser[key]; value != nil {
				paths, ok := value.([]any)
				if !ok {
					return nil, nil, errors.New("任务浏览器的初始化脚本配置不受支持")
				}
				for i, value := range paths {
					path, ok := value.(string)
					if !ok {
						return nil, nil, errors.New("任务浏览器的初始化脚本配置不受支持")
					}
					if !filepath.IsAbs(path) {
						paths[i] = filepath.Join(filepath.Dir(absolute), path)
					}
				}
			}
		}
	}
	browser["isolated"] = true
	if rawProxy != "" {
		args = stripProxyArgs(args)
		proxy, err := url.Parse(rawProxy)
		if err != nil || proxy.Host == "" || (proxy.Scheme != "http" && proxy.Scheme != "https" && proxy.Scheme != "socks5") {
			return nil, nil, errors.New("任务浏览器代理地址无效")
		}
		settings := map[string]any{}
		if proxy.User != nil {
			if proxy.Scheme == "socks5" {
				return nil, nil, errors.New("任务浏览器使用带认证 SOCKS 时需要 HTTP 代理桥接，请启用任务 HTTP 代理")
			}
			settings["username"] = proxy.User.Username()
			settings["password"], _ = proxy.User.Password()
			proxy.User = nil
		}
		settings["server"] = proxy.String()
		launch, err := browserConfigObject(browser, "launchOptions")
		if err != nil {
			return nil, nil, err
		}
		if customArgs, ok := launch["args"].([]any); ok {
			for _, arg := range customArgs {
				if value, ok := arg.(string); ok && (strings.HasPrefix(value, "--proxy-") || strings.HasPrefix(value, "--no-proxy")) {
					return nil, nil, errors.New("任务浏览器的自定义启动参数与指定代理冲突")
				}
			}
		}
		launch["proxy"] = settings
	}
	raw, err := json.Marshal(cfg)
	if err != nil {
		return nil, nil, errors.New("任务浏览器配置编码失败")
	}
	if err = os.MkdirAll(privateDir, 0o700); err != nil {
		return nil, nil, errors.New("任务浏览器私有配置目录不可用")
	}
	info, err := os.Lstat(privateDir)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return nil, nil, errors.New("任务浏览器私有配置目录不受支持")
	}
	if err = os.Chmod(privateDir, 0o700); err != nil {
		return nil, nil, errors.New("任务浏览器私有配置目录权限设置失败")
	}
	file, err := os.CreateTemp(privateDir, fmt.Sprintf("browser-task-%d-*.json", taskID))
	if err != nil {
		return nil, nil, errors.New("任务浏览器私有配置创建失败")
	}
	path := file.Name()
	cleanup := func() { _ = os.Remove(path) }
	if err = file.Chmod(0o600); err != nil {
		_ = file.Close()
		cleanup()
		return nil, nil, errors.New("任务浏览器私有配置权限设置失败")
	}
	if _, err = file.Write(raw); err != nil {
		_ = file.Close()
		cleanup()
		return nil, nil, errors.New("任务浏览器私有配置写入失败")
	}
	if err = file.Close(); err != nil {
		cleanup()
		return nil, nil, errors.New("任务浏览器私有配置保存失败")
	}
	if !contains(args, "--isolated") {
		args = append(args, "--isolated")
	}
	args = append(args, "--config", path)
	env := jsonStrMap(server.Env)
	if env == nil {
		env = map[string]string{}
	}
	if cert != "" {
		env["NODE_EXTRA_CA_CERTS"] = cert
	}
	env["PLAYWRIGHT_MCP_CONFIG"] = path
	env["PLAYWRIGHT_MCP_ISOLATED"] = "true"
	if rawProxy != "" {
		// Environment/CLI options have precedence over config, and an env
		// proxy object also erases config username/password in Playwright.
		env["PLAYWRIGHT_MCP_PROXY_SERVER"] = ""
		env["PLAYWRIGHT_MCP_PROXY_BYPASS"] = ""
	}
	copy := *server
	copy.Args, _ = json.Marshal(args)
	copy.Env, _ = json.Marshal(env)
	return &copy, cleanup, nil
}
