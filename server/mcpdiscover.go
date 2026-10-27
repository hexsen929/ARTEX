package server

import (
	"context"
	"fmt"
	"log"
	"time"

	"github.com/Autumn-27/artex/db"
	"github.com/Autumn-27/artex/mcphttp"
	"github.com/Autumn-27/norma/mcp"
	actool "github.com/Autumn-27/norma/tool"
)

// mcpClient is the shared surface of a connected MCP server (stdio, Streamable HTTP,
// or legacy SSE),
// so tools/list and cleanup are handled uniformly regardless of transport.
type mcpClient interface {
	Tools(context.Context) ([]actool.CoreTool, error)
	Close() error
}

// connectMCP dials one MCP server per its transport. Callers must Close the client.
func connectMCP(ctx context.Context, m *db.MCPServer) (mcpClient, error) {
	switch m.Transport {
	case "stdio":
		if m.Command == "" {
			return nil, fmt.Errorf("stdio 传输缺少命令")
		}
		waitCtx, cancelWait := context.WithTimeout(ctx, 60*time.Second)
		defer cancelWait()
		release, err := jshookSlots.acquire(waitCtx, jshookLimit(m))
		if err != nil {
			return nil, fmt.Errorf("MCP %s 等待可用实例时终止: %w", m.Name, err)
		}
		startCtx, cancelStart := context.WithTimeout(ctx, 20*time.Second)
		defer cancelStart()
		cl, err := mcp.NewStdioClient(startCtx, m.Name, m.Command, jsonStrMap(m.Env), jsonStrSlice(m.Args)...)
		if err != nil {
			release()
			return nil, fmt.Errorf("MCP %s 初始化失败（请检查启动配置、实例上限或服务端错误）: %w", m.Name, err)
		}
		return &leasedMCPClient{mcpClient: cl, release: release}, nil
	case "http":
		if m.URL == "" {
			return nil, fmt.Errorf("http 传输缺少 URL")
		}
		// env map doubles as HTTP headers (e.g. Authorization).
		return mcphttp.New(ctx, m.Name, m.URL, jsonStrMap(m.Env), m.Insecure)
	case "sse":
		if m.URL == "" {
			return nil, fmt.Errorf("sse 传输缺少 URL")
		}
		return mcphttp.NewSSE(ctx, m.Name, m.URL, jsonStrMap(m.Env), m.Insecure)
	default:
		return nil, fmt.Errorf("未知传输方式 %q", m.Transport)
	}
}

// discoverAndCacheMCP connects to one MCP, lists its tools, and persists the tool
// names to mcp_tools_cache so the UI shows them without a live connection.
func (s *Server) discoverAndCacheMCP(ctx context.Context, m *db.MCPServer) error {
	tools, err := cacheMCPMetadata(ctx, s.m.pg, m, true)
	if err != nil {
		return err
	}
	log.Printf("[mcp] %s 发现 %d 个工具并已缓存", m.Name, len(tools))
	return nil
}

// discoverEmptyMCPsOnStartup fills the tool cache for any enabled MCP that has none
// yet (notably the seeded browser MCP on first run). Runs sequentially in one
// goroutine so we never spawn many stdio servers (npx) at once, and never blocks
// startup. Best-effort: a failure leaves the cache empty to retry next start.
func (s *Server) discoverEmptyMCPsOnStartup() {
	servers, err := s.m.pg.ListMCP()
	if err != nil {
		log.Printf("[mcp] 启动自动发现: 读取列表失败: %v", err)
		return
	}
	for _, m := range servers {
		if !m.Enabled {
			continue
		}
		metadata, err := s.m.pg.MCPToolsDetailed(m.ID)
		if err == nil && mcpSchemasReady(metadata) {
			continue
		}
		ctx, cancel := context.WithTimeout(s.ctx, 90*time.Second)
		if _, err := cacheMCPMetadata(ctx, s.m.pg, m, false); err != nil {
			log.Printf("[mcp] 启动自动发现 %s 失败: %v", m.Name, err)
		}
		cancel()
	}
}
