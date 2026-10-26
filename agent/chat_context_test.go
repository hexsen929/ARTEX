package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Autumn-27/artex/db"
	"github.com/Autumn-27/artex/llmguard"
	"github.com/Autumn-27/norma/llm"
	actool "github.com/Autumn-27/norma/tool"
)

func TestChatProxyOverrideDistinguishesAbsentAndExplicitDirect(t *testing.T) {
	chat := NewChatAgent(nil, "fixture", t.TempDir(), nil, 0)
	chat.SetProxy("http://global.invalid:8788", "global-ca")
	chat.SetWebSearch(WebSearchOpts{Enabled: true, Proxy: "http://search.invalid:1"})
	for _, tc := range []struct {
		name     string
		ctx      context.Context
		addr, ca string
	}{
		{"ordinary", t.Context(), "http://global.invalid:8788", "global-ca"},
		{"task", WithChatProxyOverride(t.Context(), "http://task.invalid:21100", "task-ca"), "http://task.invalid:21100", "task-ca"},
		{"explicit direct", WithChatProxyOverride(t.Context(), "", ""), "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			addr, ca := chat.proxyForChat(tc.ctx)
			if addr != tc.addr || ca != tc.ca {
				t.Fatalf("proxy=(%q,%q), want (%q,%q)", addr, ca, tc.addr, tc.ca)
			}
		})
	}
	if chat.proxyAddr != "http://global.invalid:8788" || chat.proxyCACert != "global-ca" || chat.webSearch.Proxy != "http://search.invalid:1" {
		t.Fatal("turn proxy mutated shared agent or search proxy")
	}
}

func TestChatPreservesParentAttributionValuesAndHTTPBudget(t *testing.T) {
	type parentKey struct{}
	oldAugment, oldResolve := ToolAugment, ToolResolve
	t.Cleanup(func() { ToolAugment, ToolResolve = oldAugment, oldResolve })
	ToolResolve = nil
	want := RunInfo{TaskID: 31, ExplorationID: 41, IntentID: 51, SessionID: "new-conversation"}
	var assemblyInfo, providerInfo RunInfo
	ToolAugment = func(ctx context.Context, _ string) ([]actool.CoreTool, DeferredInfo, func()) {
		assemblyInfo = RunInfoFrom(ctx)
		if ctx.Value(parentKey{}) != "parent-value" {
			t.Error("parent context value lost during assembly")
		}
		return nil, DeferredInfo{}, func() {}
	}
	provider := captureUsageProvider{stream: func(ctx context.Context, yield func(llm.StreamEvent, error) bool) {
		providerInfo = RunInfoFrom(ctx)
		if ctx.Value(parentKey{}) != "parent-value" {
			t.Error("parent context value lost before model call")
		}
		if err := llmguard.TakeHTTP(ctx); err != nil {
			yield(llm.StreamEvent{}, err)
			return
		}
		if !yield(llm.StreamEvent{Type: llm.SETextDelta, Text: "fixture complete"}, nil) {
			return
		}
		yield(llm.StreamEvent{Type: llm.SEMessageDelta, StopReason: "end_turn"}, nil)
	}}
	chat := NewChatAgent(provider, "fixture", t.TempDir(), nil, 0)
	ctx := context.WithValue(t.Context(), parentKey{}, "parent-value")
	ctx = WithRunInfo(ctx, RunInfo{TaskID: want.TaskID, ExplorationID: want.ExplorationID, IntentID: want.IntentID, SessionID: "parent-session"})
	ctx = llmguard.WithBudget(ctx, 1)
	ctx = WithChatProxyOverride(ctx, "", "")
	if _, err := chat.Chat(ctx, "verifier", want.SessionID, "fixture", 3, 0, false, nil); err != nil {
		t.Fatal(err)
	}
	if assemblyInfo != want || providerInfo != want {
		t.Fatalf("attribution lost: assembly=%+v provider=%+v want=%+v", assemblyInfo, providerInfo, want)
	}
	if got := RunInfoFrom(ctx); got.SessionID != "parent-session" || got.TaskID != want.TaskID {
		t.Fatalf("parent context was mutated: %+v", got)
	}
	if err := llmguard.TakeHTTP(ctx); !errors.Is(err, llmguard.ErrRetryBudget) {
		t.Fatalf("parent HTTP budget was replaced/reset: %v", err)
	}
}

func TestConcurrentTaskChatsKeepPrivateProxyAndAttribution(t *testing.T) {
	type parentKey struct{}
	type observation struct {
		info  RunInfo
		env   []string
		value any
	}
	oldAugment, oldResolve := ToolAugment, ToolResolve
	t.Cleanup(func() { ToolAugment, ToolResolve = oldAugment, oldResolve })
	ToolResolve = nil
	assembled := make(chan observation, 2)
	called := make(chan observation, 2)
	release := make(chan struct{})
	ToolAugment = func(ctx context.Context, _ string) ([]actool.CoreTool, DeferredInfo, func()) {
		assembled <- observation{info: RunInfoFrom(ctx), value: ctx.Value(parentKey{})}
		probe := actool.Build(actool.Spec{Name: "ChatScopeProxyProbe", Schema: map[string]any{"type": "object"}, Run: func(ctx context.Context, _ json.RawMessage, tc *actool.ToolContext) (actool.Result, error) {
			called <- observation{info: RunInfoFrom(ctx), env: append([]string(nil), tc.Env...), value: ctx.Value(parentKey{})}
			select {
			case <-release:
				return actool.Text("fixture probe complete"), nil
			case <-ctx.Done():
				return actool.Result{}, ctx.Err()
			}
		}})
		return []actool.CoreTool{probe}, DeferredInfo{}, func() {}
	}
	var mu sync.Mutex
	turns := map[string]int{}
	provider := captureUsageProvider{stream: func(ctx context.Context, yield func(llm.StreamEvent, error) bool) {
		ri := RunInfoFrom(ctx)
		mu.Lock()
		turns[ri.SessionID]++
		turn := turns[ri.SessionID]
		mu.Unlock()
		events := []llm.StreamEvent{{Type: llm.SETextDelta, Text: "fixture complete"}, {Type: llm.SEMessageDelta, StopReason: "end_turn"}}
		if turn == 1 {
			events = []llm.StreamEvent{{Type: llm.SEToolUseStart, ToolID: "fixture-probe", ToolName: "ChatScopeProxyProbe"}, {Type: llm.SEToolInputJSON, Text: `{}`}, {Type: llm.SEMessageDelta, StopReason: "tool_use"}}
		}
		for _, event := range events {
			if !yield(event, nil) {
				return
			}
		}
	}}
	chat := NewChatAgent(provider, "fixture", t.TempDir(), nil, 0)
	chat.SetProxy("http://global.invalid:8788", "global-ca")
	chat.SetWebSearch(WebSearchOpts{Enabled: true, Proxy: "http://search.invalid:1"})
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	var wg sync.WaitGroup
	defer func() { cancel(); wg.Wait() }()
	errs := make(chan error, 2)
	for _, id := range []int64{1, 2} {
		wg.Add(1)
		go func(id int64) {
			defer wg.Done()
			turnCtx := context.WithValue(ctx, parentKey{}, fmt.Sprintf("parent-%d", id))
			turnCtx = WithRunInfo(turnCtx, RunInfo{TaskID: id, ExplorationID: id * 10, IntentID: id * 100, SessionID: "parent"})
			turnCtx = WithChatProxyOverride(turnCtx, fmt.Sprintf("http://task-%d.invalid:21100", id), fmt.Sprintf("task-%d-ca", id))
			_, err := chat.Chat(turnCtx, "verifier", fmt.Sprintf("task-%d-chat", id), "fixture", 4, 0, false, nil)
			errs <- err
		}(id)
	}
	checkInfo := func(o observation) {
		t.Helper()
		id := o.info.TaskID
		if id != 1 && id != 2 || o.info.ExplorationID != id*10 || o.info.IntentID != id*100 || o.info.SessionID != fmt.Sprintf("task-%d-chat", id) || o.value != fmt.Sprintf("parent-%d", id) {
			t.Fatalf("cross-task attribution: %+v", o)
		}
	}
	for range 2 {
		select {
		case o := <-assembled:
			checkInfo(o)
		case <-ctx.Done():
			t.Fatal("chat assembly timed out")
		}
	}
	seen := map[int64]bool{}
	for range 2 {
		select {
		case o := <-called:
			checkInfo(o)
			seen[o.info.TaskID] = true
			joined := strings.Join(o.env, "\n")
			wantProxy := fmt.Sprintf("HTTP_PROXY=http://task-%d.invalid:21100", o.info.TaskID)
			wantCA := fmt.Sprintf("SSL_CERT_FILE=task-%d-ca", o.info.TaskID)
			if !strings.Contains(joined, wantProxy) || !strings.Contains(joined, wantCA) || strings.Contains(joined, "global.invalid") || strings.Contains(joined, "search.invalid") {
				t.Fatalf("cross-task/default proxy leak: task=%d env=%q", o.info.TaskID, joined)
			}
		case <-ctx.Done():
			t.Fatal("chat probe timed out")
		}
	}
	if len(seen) != 2 {
		t.Fatal("both task contexts did not reach the tools")
	}
	close(release)
	for range 2 {
		select {
		case err := <-errs:
			if err != nil {
				t.Fatal(err)
			}
		case <-ctx.Done():
			t.Fatal("chat completion timed out")
		}
	}
	if chat.proxyAddr != "http://global.invalid:8788" || chat.proxyCACert != "global-ca" || chat.webSearch.Proxy != "http://search.invalid:1" {
		t.Fatal("concurrent turn changed shared proxy settings")
	}
}
