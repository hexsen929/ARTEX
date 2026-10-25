package server

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"log"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Autumn-27/artex/db"
	"github.com/Autumn-27/norma/permission"
	actool "github.com/Autumn-27/norma/tool"
)

// Slots cover both discovery and real sessions. Jshook counts processes across
// server entries, so aliases must share this queue, not each get their own cap.
var jshookSlots mcpSlotQueue

type mcpSlotWaiter struct {
	ready   chan struct{}
	limit   int
	granted bool
}

type mcpSlotQueue struct {
	mu      sync.Mutex
	active  int
	waiters []*mcpSlotWaiter
}

func (q *mcpSlotQueue) dispatchLocked() {
	for len(q.waiters) > 0 && q.active < q.waiters[0].limit {
		w := q.waiters[0]
		q.waiters = q.waiters[1:]
		q.active++
		w.granted = true
		close(w.ready)
	}
}

func (q *mcpSlotQueue) acquire(ctx context.Context, limit int) (func(), error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if limit <= 0 {
		return func() {}, nil
	}
	w := &mcpSlotWaiter{ready: make(chan struct{}), limit: limit}
	q.mu.Lock()
	q.waiters = append(q.waiters, w)
	q.dispatchLocked()
	q.mu.Unlock()
	var once sync.Once
	release := func() {
		once.Do(func() {
			q.mu.Lock()
			q.active--
			q.dispatchLocked()
			q.mu.Unlock()
		})
	}
	select {
	case <-w.ready:
		if err := ctx.Err(); err != nil {
			release()
			return nil, err
		}
		return release, nil
	case <-ctx.Done():
		q.mu.Lock()
		granted := w.granted
		if !granted {
			for i, candidate := range q.waiters {
				if candidate == w {
					q.waiters = append(q.waiters[:i], q.waiters[i+1:]...)
					break
				}
			}
			q.dispatchLocked()
		}
		q.mu.Unlock()
		if granted {
			release()
		}
		return nil, ctx.Err()
	}
}

func jshookLimit(m *db.MCPServer) int {
	if m.Transport != "stdio" {
		return 0
	}
	n, _ := strconv.Atoi(strings.TrimSpace(jsonStrMap(m.Env)["JSHOOK_MAX_INSTANCES"]))
	return n
}

type leasedMCPClient struct {
	mcpClient
	once    sync.Once
	release func()
	err     error
}

func (c *leasedMCPClient) Close() error {
	c.once.Do(func() {
		c.err = c.mcpClient.Close()
		c.release()
	})
	return c.err
}

// Metadata hydration is serialized per configuration, including across Agents.
// The persisted schema cache is rechecked after entering the gate. No clients,
// browser state, credentials or tool results are shared between sessions.
var mcpMetadataGates sync.Map

func mcpMetadataGate(m *db.MCPServer) chan struct{} {
	raw, _ := json.Marshal([]any{m.ID, m.Name, m.Transport, m.Command, jsonStrSlice(m.Args), jsonStrMap(m.Env), m.URL, m.Insecure})
	key := sha256.Sum256(raw)
	v, _ := mcpMetadataGates.LoadOrStore(key, make(chan struct{}, 1))
	return v.(chan struct{})
}

func mcpSchemasReady(tools []db.MCPTool) bool {
	if len(tools) == 0 {
		return false
	}
	for _, t := range tools {
		var schema map[string]any
		if json.Unmarshal(t.Schema, &schema) != nil || schema == nil {
			return false
		}
	}
	return true
}

func cacheMCPMetadata(ctx context.Context, pg *db.DB, m *db.MCPServer, force bool) ([]db.MCPTool, error) {
	gate := mcpMetadataGate(m)
	select {
	case gate <- struct{}{}:
		defer func() { <-gate }()
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	cached, err := pg.MCPToolsDetailed(m.ID)
	if err != nil {
		return nil, err
	}
	if !force && mcpSchemasReady(cached) {
		return cached, nil
	}
	cl, err := connectMCP(ctx, m)
	if err != nil {
		return cached, err
	}
	defer cl.Close() // metadata discovery must never reserve an idle session slot
	ts, err := cl.Tools(ctx)
	if err != nil {
		return cached, err
	}
	metadata := make([]db.MCPTool, 0, len(ts))
	for _, t := range ts {
		schema, err := json.Marshal(t.InputSchema())
		if err != nil {
			return cached, err
		}
		metadata = append(metadata, db.MCPTool{Name: t.Name(), Description: t.Description(), Schema: schema})
	}
	if err := pg.SaveMCPToolsForConfig(m, metadata); err != nil {
		return cached, err
	}
	return metadata, nil
}

// lazyMCPSession owns exactly one stdio connection, and creates it only on Call,
// never on Name/Description/InputSchema or deferred-tool search. Once stateful
// work starts, retain that connection through the run so browser/debugger and
// activate_tools state are not silently lost between LLM turns.
type lazyMCPSession struct {
	ctx            context.Context
	cancel         context.CancelFunc
	stop           func() bool
	server         *db.MCPServer
	dial           func(context.Context, *db.MCPServer) (mcpClient, error)
	serial         chan struct{}
	mu             sync.Mutex
	client         mcpClient
	tools          map[string]actool.CoreTool
	closed         bool
	stateful       bool
	idle           *time.Timer
	idleGeneration uint64
	idleTTL        time.Duration
}

func newLazyMCPSession(ctx context.Context, m *db.MCPServer) *lazyMCPSession {
	ctx, cancel := context.WithCancel(ctx)
	s := &lazyMCPSession{ctx: ctx, cancel: cancel, server: m, dial: connectMCP, serial: make(chan struct{}, 1), idleTTL: 30 * time.Second}
	s.mu.Lock()
	s.stop = context.AfterFunc(ctx, func() { _ = s.Close() })
	s.mu.Unlock()
	return s
}

func (s *lazyMCPSession) Close() error {
	s.cancel() // interrupt slot waits/initialization before taking the mutex
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil
	}
	s.closed = true
	if s.stop != nil {
		s.stop()
	}
	return s.disconnectLocked()
}

func (s *lazyMCPSession) disconnectLocked() error {
	s.idleGeneration++
	if s.idle != nil {
		s.idle.Stop()
		s.idle = nil
	}
	var err error
	if s.client != nil {
		err = s.client.Close()
	}
	s.client, s.tools = nil, nil
	return err
}

func (s *lazyMCPSession) tool(metadata db.MCPTool) actool.CoreTool {
	var schema map[string]any
	if json.Unmarshal(metadata.Schema, &schema) != nil || schema == nil {
		// Keep known tools visible after a temporary metadata-discovery failure.
		// Actual execution resolves and validates against the live tool schema.
		schema = map[string]any{"type": "object", "additionalProperties": true}
	}
	return actool.Build(actool.Spec{
		Name: metadata.Name, Description: metadata.Description, Schema: schema,
		Permissions: func(context.Context, json.RawMessage, permission.Context) permission.Decision {
			return permission.AskUser("call MCP tool " + metadata.Name + "?")
		},
		Run: func(ctx context.Context, in json.RawMessage, tc *actool.ToolContext) (actool.Result, error) {
			return s.call(ctx, metadata.Name, in, tc)
		},
	})
}

func (s *lazyMCPSession) call(ctx context.Context, name string, in json.RawMessage, tc *actool.ToolContext) (actool.Result, error) {
	ctx, cancel := context.WithCancel(ctx)
	stop := context.AfterFunc(s.ctx, cancel)
	defer stop()
	defer cancel()
	select {
	case s.serial <- struct{}{}:
		defer func() { <-s.serial }()
	case <-ctx.Done():
		return actool.Errorf("MCP 等待调用时终止: " + ctx.Err().Error()), nil
	}
	s.mu.Lock()
	if s.closed || s.ctx.Err() != nil || ctx.Err() != nil {
		s.mu.Unlock()
		return actool.Errorf("MCP 会话已结束"), nil
	}
	if s.idle != nil {
		s.idle.Stop()
		s.idle = nil
	}
	s.idleGeneration++
	if s.client == nil {
		cl, err := s.dial(ctx, s.server)
		if err != nil {
			s.mu.Unlock()
			// Do not remove the proxy, or replay a tools/call. A later explicit
			// invocation can retry initialization after capacity becomes available.
			return actool.Errorf(fmt.Sprintf("MCP %s 按需连接失败（工具仍保留，可稍后重试）: %v", s.server.Name, err)), nil
		}
		ts, err := cl.Tools(ctx)
		if err != nil {
			_ = cl.Close()
			s.mu.Unlock()
			return actool.Errorf(fmt.Sprintf("MCP %s 工具目录读取失败: %v", s.server.Name, err)), nil
		}
		s.client = cl
		s.tools = make(map[string]actool.CoreTool, len(ts))
		for _, t := range ts {
			s.tools[t.Name()] = t
		}
		log.Printf("[mcp] %s 按需连接已建立", s.server.Name)
	}
	t := s.tools[name]
	if t == nil {
		_ = s.disconnectLocked()
		s.mu.Unlock()
		return actool.Errorf("MCP 工具目录已变化，请刷新该服务器的工具缓存后重新运行"), nil
	}
	// Only jshook's stateless catalog operations may expire between calls. All
	// other tools (including activate_*, call_tool and unknown future tools) pin
	// the connection until cleanup to preserve the user's ongoing work.
	short := strings.TrimPrefix(name, "mcp__"+s.server.Name+"__")
	metadataOnly := jshookLimit(s.server) > 0 && jshookMetadataOnly(short, in)
	if !metadataOnly {
		s.stateful = true
	}
	s.mu.Unlock()
	result, err := t.Call(ctx, in, tc) // exactly once: never automatically replay operations
	s.mu.Lock()
	transportClosed := err != nil && strings.Contains(err.Error(), "connection closed")
	for _, block := range result.Content {
		if result.IsError && strings.HasPrefix(block.Text, "Error: mcp: connection closed") {
			transportClosed = true
		}
	}
	if transportClosed {
		_ = s.disconnectLocked()
		s.stateful = false
	}
	if !s.closed && !s.stateful && s.client != nil {
		s.idleGeneration++
		generation := s.idleGeneration
		s.idle = time.AfterFunc(s.idleTTL, func() {
			// A tool call owns serial throughout initialization and execution.
			// If it is busy, its completion installs the next idle deadline.
			select {
			case s.serial <- struct{}{}:
				defer func() { <-s.serial }()
			default:
				return
			}
			s.mu.Lock()
			defer s.mu.Unlock()
			if !s.closed && !s.stateful && s.idleGeneration == generation {
				_ = s.disconnectLocked()
			}
		})
	}
	s.mu.Unlock()
	return result, err
}

func jshookMetadataOnly(name string, in json.RawMessage) bool {
	var args map[string]any
	if json.Unmarshal(in, &args) != nil {
		return false
	}
	switch name {
	case "describe_tool":
		return true
	case "search_tools":
		// Jshook 0.4.2 defaults auto_activate to TRUE: a plain search is
		// stateful unless the caller explicitly opts out of activation.
		return args["auto_activate"] == false
	case "route_tool":
		if args["context"] == nil {
			return true
		}
		options, ok := args["context"].(map[string]any)
		return ok && (options["autoActivate"] == nil || options["autoActivate"] == false)
	default:
		return false
	}
}
