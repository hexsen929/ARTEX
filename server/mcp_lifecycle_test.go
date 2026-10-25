package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Autumn-27/artex/db"
	actool "github.com/Autumn-27/norma/tool"
)

type lifecycleFakeClient struct {
	tools  []actool.CoreTool
	closed atomic.Int32
	err    error
}

func (c *lifecycleFakeClient) Tools(context.Context) ([]actool.CoreTool, error) {
	return c.tools, c.err
}
func (c *lifecycleFakeClient) Close() error { c.closed.Add(1); return nil }

func lifecycleTool(name string, run func(context.Context, json.RawMessage, *actool.ToolContext) (actool.Result, error)) actool.CoreTool {
	return actool.Build(actool.Spec{Name: name, Description: "fixture", Schema: map[string]any{"type": "object"}, Run: run})
}

func lifecycleMetadata(name string) db.MCPTool {
	return db.MCPTool{Name: name, Description: "fixture", Schema: json.RawMessage(`{"type":"object"}`)}
}

func lifecycleSession(t *testing.T, ctx context.Context) *lazyMCPSession {
	t.Helper()
	s := newLazyMCPSession(ctx, &db.MCPServer{Name: "jshook", Transport: "stdio", Env: json.RawMessage(`{"JSHOOK_MAX_INSTANCES":"4"}`)})
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func waitLifecycle(t *testing.T, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for !ok() {
		if time.Now().After(deadline) {
			t.Fatal("lifecycle transition timed out")
		}
		time.Sleep(time.Millisecond)
	}
}

func TestLazyMCPDoesNotConnectForMetadata(t *testing.T) {
	s := lifecycleSession(t, context.Background())
	var starts atomic.Int32
	s.dial = func(context.Context, *db.MCPServer) (mcpClient, error) {
		starts.Add(1)
		return nil, errors.New("unexpected")
	}
	tool := s.tool(lifecycleMetadata("mcp__jshook__call_tool"))
	_ = tool.Name()
	_ = tool.Description()
	_ = tool.InputSchema()
	_ = tool.Prompt()
	if starts.Load() != 0 {
		t.Fatal("reading cached metadata spawned a server")
	}
	_ = s.Close()
	if starts.Load() != 0 {
		t.Fatal("closing an unused session spawned a server")
	}
}

func TestLazyMCPReusesPrivateConnectionAndClosesOnce(t *testing.T) {
	s := lifecycleSession(t, context.Background())
	var calls, starts atomic.Int32
	name := "mcp__jshook__call_tool"
	client := &lifecycleFakeClient{tools: []actool.CoreTool{lifecycleTool(name, func(context.Context, json.RawMessage, *actool.ToolContext) (actool.Result, error) {
		calls.Add(1)
		return actool.Text("ok"), nil
	})}}
	s.dial = func(context.Context, *db.MCPServer) (mcpClient, error) { starts.Add(1); return client, nil }
	tool := s.tool(lifecycleMetadata(name))
	for range 3 {
		res, err := tool.Call(context.Background(), json.RawMessage(`{}`), nil)
		if err != nil || res.IsError {
			t.Fatalf("call failed: %+v, %v", res, err)
		}
	}
	_ = s.Close()
	_ = s.Close()
	if starts.Load() != 1 || calls.Load() != 3 || client.closed.Load() != 1 {
		t.Fatalf("starts=%d calls=%d closes=%d", starts.Load(), calls.Load(), client.closed.Load())
	}
}

func TestLazyMCPMetadataIdleReleasesAndReconnects(t *testing.T) {
	s := lifecycleSession(t, context.Background())
	s.idleTTL = 5 * time.Millisecond
	var starts, closes atomic.Int32
	name := "mcp__jshook__describe_tool"
	s.dial = func(context.Context, *db.MCPServer) (mcpClient, error) {
		starts.Add(1)
		return &lifecycleClosingClient{onClose: func() { closes.Add(1) }, lifecycleFakeClient: lifecycleFakeClient{tools: []actool.CoreTool{lifecycleTool(name, func(context.Context, json.RawMessage, *actool.ToolContext) (actool.Result, error) {
			return actool.Text("schema"), nil
		})}}}, nil
	}
	tool := s.tool(lifecycleMetadata(name))
	for i := int32(1); i <= 2; i++ {
		res, err := tool.Call(context.Background(), json.RawMessage(`{"name":"fixture"}`), nil)
		if err != nil || res.IsError {
			t.Fatalf("call failed: %+v %v", res, err)
		}
		want := i
		waitLifecycle(t, func() bool { return closes.Load() == want })
	}
	if starts.Load() != 2 {
		t.Fatalf("expected reconnection, starts=%d", starts.Load())
	}
}

type lifecycleClosingClient struct {
	lifecycleFakeClient
	onClose func()
}

func (c *lifecycleClosingClient) Close() error { c.onClose(); return c.lifecycleFakeClient.Close() }

func TestLazyMCPActivationAndBrowserStateNeverIdleExpire(t *testing.T) {
	for _, test := range []struct{ short, input string }{
		{"search_tools", `{"query":"browser"}`},
		{"search_tools", `{"query":"browser","auto_activate":true}`},
		{"route_tool", `{"task":"fixture","context":{"autoActivate":true}}`},
		{"activate_tools", `{"names":["fixture"]}`},
		{"call_tool", `{"name":"fixture"}`},
		{"future_stateful_tool", `{}`},
	} {
		t.Run(test.short+test.input, func(t *testing.T) {
			s := lifecycleSession(t, context.Background())
			s.idleTTL = time.Millisecond
			name := "mcp__jshook__" + test.short
			client := &lifecycleFakeClient{tools: []actool.CoreTool{lifecycleTool(name, func(context.Context, json.RawMessage, *actool.ToolContext) (actool.Result, error) {
				return actool.Text("ok"), nil
			})}}
			s.dial = func(context.Context, *db.MCPServer) (mcpClient, error) { return client, nil }
			res, err := s.tool(lifecycleMetadata(name)).Call(context.Background(), json.RawMessage(test.input), nil)
			if err != nil || res.IsError {
				t.Fatal("call failed")
			}
			s.mu.Lock()
			pinned := s.stateful && s.idle == nil
			s.mu.Unlock()
			if !pinned || client.closed.Load() != 0 {
				t.Fatal("stateful operation was idle-evictable")
			}
		})
	}
}

func TestLazyMCPInitializationFailureKeepsToolRetryable(t *testing.T) {
	s := lifecycleSession(t, context.Background())
	var starts, calls atomic.Int32
	name := "mcp__jshook__call_tool"
	s.dial = func(context.Context, *db.MCPServer) (mcpClient, error) {
		if starts.Add(1) == 1 {
			return nil, errors.New("capacity unavailable")
		}
		return &lifecycleFakeClient{tools: []actool.CoreTool{lifecycleTool(name, func(context.Context, json.RawMessage, *actool.ToolContext) (actool.Result, error) {
			calls.Add(1)
			return actool.Text("ok"), nil
		})}}, nil
	}
	tool := s.tool(lifecycleMetadata(name))
	r, _ := tool.Call(context.Background(), json.RawMessage(`{}`), nil)
	if !r.IsError {
		t.Fatal("expected initialization error")
	}
	r, _ = tool.Call(context.Background(), json.RawMessage(`{}`), nil)
	if r.IsError || starts.Load() != 2 || calls.Load() != 1 {
		t.Fatal("tool was lost or an operation was replayed")
	}
}

func TestLazyMCPTransportFailureDoesNotReplayOperation(t *testing.T) {
	s := lifecycleSession(t, context.Background())
	var starts, calls atomic.Int32
	name := "mcp__jshook__call_tool"
	s.dial = func(context.Context, *db.MCPServer) (mcpClient, error) {
		starts.Add(1)
		return &lifecycleFakeClient{tools: []actool.CoreTool{lifecycleTool(name, func(context.Context, json.RawMessage, *actool.ToolContext) (actool.Result, error) {
			calls.Add(1)
			return actool.Errorf("Error: mcp: connection closed"), nil
		})}}, nil
	}
	tool := s.tool(lifecycleMetadata(name))
	r, _ := tool.Call(context.Background(), json.RawMessage(`{}`), nil)
	if !r.IsError || calls.Load() != 1 || starts.Load() != 1 {
		t.Fatal("operation was automatically replayed")
	}
	_, _ = tool.Call(context.Background(), json.RawMessage(`{}`), nil)
	if calls.Load() != 2 || starts.Load() != 2 {
		t.Fatal("explicit subsequent invocation did not reconnect")
	}
}

func TestLazyMCPCloseInterruptsInitialization(t *testing.T) {
	s := lifecycleSession(t, context.Background())
	entered := make(chan struct{})
	s.dial = func(ctx context.Context, _ *db.MCPServer) (mcpClient, error) {
		close(entered)
		<-ctx.Done()
		return nil, ctx.Err()
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = s.tool(lifecycleMetadata("mcp__jshook__call_tool")).Call(context.Background(), json.RawMessage(`{}`), nil)
	}()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("initialization never started")
	}
	_ = s.Close()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("close did not cancel blocked initialization")
	}
}

func TestLazyMCPConcurrentCallsInitializeOnlyOnce(t *testing.T) {
	s := lifecycleSession(t, context.Background())
	var starts, calls atomic.Int32
	name := "mcp__jshook__call_tool"
	s.dial = func(context.Context, *db.MCPServer) (mcpClient, error) {
		starts.Add(1)
		return &lifecycleFakeClient{tools: []actool.CoreTool{lifecycleTool(name, func(context.Context, json.RawMessage, *actool.ToolContext) (actool.Result, error) {
			calls.Add(1)
			return actool.Text("ok"), nil
		})}}, nil
	}
	var wg sync.WaitGroup
	for range 12 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r, e := s.tool(lifecycleMetadata(name)).Call(context.Background(), json.RawMessage(`{}`), nil)
			if e != nil || r.IsError {
				t.Errorf("call failed: %v", e)
			}
		}()
	}
	wg.Wait()
	if starts.Load() != 1 || calls.Load() != 12 {
		t.Fatalf("starts=%d calls=%d", starts.Load(), calls.Load())
	}
}

func TestMCPSlotQueueFullWaitsUntilRelease(t *testing.T) {
	var q mcpSlotQueue
	first, err := q.acquire(context.Background(), 1)
	if err != nil {
		t.Fatal(err)
	}
	got := make(chan func(), 1)
	go func() {
		release, e := q.acquire(context.Background(), 1)
		if e == nil {
			got <- release
		}
	}()
	waitLifecycle(t, func() bool { q.mu.Lock(); defer q.mu.Unlock(); return len(q.waiters) == 1 })
	select {
	case <-got:
		t.Fatal("exceeded capacity")
	default:
	}
	first()
	first()
	select {
	case release := <-got:
		release()
	case <-time.After(time.Second):
		t.Fatal("waiter not admitted")
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.active != 0 || len(q.waiters) != 0 {
		t.Fatal("slot leak")
	}
}

func TestMCPSlotQueueCancelledWaiterDoesNotConsumeSlot(t *testing.T) {
	var q mcpSlotQueue
	first, _ := q.acquire(context.Background(), 1)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { _, err := q.acquire(ctx, 1); done <- err }()
	waitLifecycle(t, func() bool { q.mu.Lock(); defer q.mu.Unlock(); return len(q.waiters) == 1 })
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	first()
	release, err := q.acquire(context.Background(), 1)
	if err != nil {
		t.Fatal(err)
	}
	release()
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.active != 0 || len(q.waiters) != 0 {
		t.Fatal("cancelled waiter leaked a slot")
	}
}

func TestMCPSlotQueueCancellationGrantRace(t *testing.T) {
	for range 100 {
		var q mcpSlotQueue
		first, _ := q.acquire(context.Background(), 1)
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan struct{})
		go func() {
			defer close(done)
			release, err := q.acquire(ctx, 1)
			if err == nil {
				release()
			}
		}()
		var wg sync.WaitGroup
		wg.Add(2)
		go func() { defer wg.Done(); cancel() }()
		go func() { defer wg.Done(); first() }()
		wg.Wait()
		<-done
		q.mu.Lock()
		active, waiting := q.active, len(q.waiters)
		q.mu.Unlock()
		if active != 0 || waiting != 0 {
			t.Fatal(fmt.Sprintf("slot leak: active=%d waiting=%d", active, waiting))
		}
	}
}

func TestLeasedMCPClientReleasesExactlyOnce(t *testing.T) {
	base := &lifecycleFakeClient{}
	var released atomic.Int32
	cl := &leasedMCPClient{mcpClient: base, release: func() { released.Add(1) }}
	var wg sync.WaitGroup
	for range 10 {
		wg.Add(1)
		go func() { defer wg.Done(); _ = cl.Close() }()
	}
	wg.Wait()
	if base.closed.Load() != 1 || released.Load() != 1 {
		t.Fatal("duplicate cleanup")
	}
}
