package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/Autumn-27/artex/db"
	"github.com/Autumn-27/norma/llm"
	actool "github.com/Autumn-27/norma/tool"
)

func TestToolAssemblyResolverFailureReturnsNoCapabilities(t *testing.T) {
	oldAugment, oldResolve := ToolAugment, ToolResolve
	t.Cleanup(func() { ToolAugment, ToolResolve = oldAugment, oldResolve })
	problem := errors.New("fixture catalog permissions unavailable")
	cleaned := false
	ToolAugment = func(context.Context, string) ([]actool.CoreTool, DeferredInfo, func()) {
		return []actool.CoreTool{actool.NewBash()}, DeferredInfo{}, func() { cleaned = true }
	}
	ToolResolve = func(context.Context, string, []actool.CoreTool) []actool.CoreTool {
		return ToolAssemblyFailure(problem)
	}
	tools, def, cleanup := AugmentTools(t.Context(), "pentest", actool.DefaultTools())
	cleanup()
	if tools == nil || len(tools) != 0 || !errors.Is(def.AssemblyError, problem) || !cleaned {
		t.Fatal("failed catalog lookup retained tools or lost its cleanup/error")
	}
	chat := NewChatAgent(nil, "fixture", t.TempDir(), nil, 0)
	if _, err := chat.Chat(t.Context(), "pentest", "fixture", "do not call a model", 1, 0, false, nil); !errors.Is(err, problem) {
		t.Fatal("chat did not stop before the nil model", err)
	}
	main := NewMainAgent(nil, "fixture", t.TempDir(), nil, 0, 1)
	if _, err := main.Chat(t.Context(), 17, 0, nil, nil, "fixture", "do not call a model", nil, nil, nil, nil, nil); !errors.Is(err, problem) {
		t.Fatal("main agent did not stop before the nil model", err)
	}
}

func TestGoalsLoadGuidanceWithoutExecutionCapabilities(t *testing.T) {
	oldAugment, oldResolve := ToolAugment, ToolResolve
	t.Cleanup(func() { ToolAugment, ToolResolve = oldAugment, oldResolve })
	ToolResolve = nil
	called := false
	ToolAugment = func(ctx context.Context, key string) ([]actool.CoreTool, DeferredInfo, func()) {
		called = true
		if key != "goals" || RunInfoFrom(ctx).TaskID != 29 {
			t.Fatal("goal run omitted its role or task attribution")
		}
		return []actool.CoreTool{
			actool.Build(actool.Spec{Name: "Skill", Schema: map[string]any{"type": "object"}, ReadOnly: func(json.RawMessage) bool { return true }, Run: func(context.Context, json.RawMessage, *actool.ToolContext) (actool.Result, error) {
				return actool.Text("fixture guidance"), nil
			}}),
			actool.Build(actool.Spec{Name: "read_skill_reference", Schema: map[string]any{"type": "object"}, ReadOnly: func(json.RawMessage) bool { return true }}),
			actool.Build(actool.Spec{Name: "Read", Schema: map[string]any{"type": "object"}, ReadOnly: func(json.RawMessage) bool { return true }}),
			actool.NewBash(),
			actool.Build(actool.Spec{Name: "mcp__fixture__network", Schema: map[string]any{"type": "object"}}),
		}, DeferredInfo{Deferred: []string{"mcp__fixture__network"}, GlobalNames: []string{"mcp__fixture__network"}, Unlock: actool.NewUnlockSet("mcp__fixture__network")}, nil
	}
	provider := &credentialFixtureProvider{reply: "fixture complete", stop: "end_turn"}
	DecomposeGoalsWithProvider(t.Context(), provider, t.TempDir(), "fixture", "fixture", nil, nil, 29, true, 57, nil)
	if !called || provider.calls != 1 || provider.request.MaxTokens != 57 {
		t.Fatal("goals did not use the shared assembler or preserved provider settings")
	}
	seen := map[string]bool{}
	for _, tool := range provider.request.Tools {
		seen[tool.Name] = true
		if !IsGoalsToolAllowed(tool.Name) {
			t.Fatalf("goals received an execution capability: %s", tool.Name)
		}
	}
	if !seen["Skill"] || !seen["read_skill_reference"] || seen["Read"] || !seen["set_goals"] || !seen["set_constraints"] || len(seen) != 4 {
		t.Fatal("goals lost its limited write tools or authorized guidance")
	}
	if strings.Contains(strings.Join(provider.request.System, "\n"), "mcp__fixture__network") {
		t.Fatal("goals advertised an execution dependency")
	}
}

func TestAugmentRejectsHostFailureBeforeResolverCanDropMarker(t *testing.T) {
	oldAugment, oldResolve := ToolAugment, ToolResolve
	t.Cleanup(func() { ToolAugment, ToolResolve = oldAugment, oldResolve })
	problem := errors.New("fixture host metadata unavailable")
	ToolAugment = func(context.Context, string) ([]actool.CoreTool, DeferredInfo, func()) {
		extra := []actool.CoreTool{actool.NewBash()}
		return append(extra, ToolAssemblyFailure(problem)...), DeferredInfo{}, nil
	}
	ToolResolve = func(context.Context, string, []actool.CoreTool) []actool.CoreTool {
		t.Fatal("the resolver must not hide a prior host metadata failure")
		return nil
	}
	tools, def, cleanup := AugmentTools(t.Context(), "worker", actool.DefaultTools())
	defer cleanup()
	if tools == nil || len(tools) != 0 || !errors.Is(def.AssemblyError, problem) {
		t.Fatal("mixed host failure retained capabilities or lost its reason")
	}
}

func TestGoalsAssemblyFailureStopsBeforeModel(t *testing.T) {
	oldAugment, oldResolve := ToolAugment, ToolResolve
	t.Cleanup(func() { ToolAugment, ToolResolve = oldAugment, oldResolve })
	ToolResolve = nil
	cleaned, reported := false, false
	ToolAugment = func(context.Context, string) ([]actool.CoreTool, DeferredInfo, func()) {
		return nil, DeferredInfo{AssemblyError: errors.New("fixture unavailable")}, func() { cleaned = true }
	}
	provider := &credentialFixtureProvider{}
	DecomposeGoalsWithProvider(t.Context(), provider, t.TempDir(), "fixture", "fixture", nil, nil, 29, true, 57, func(a db.Activity) {
		if a.IsError && a.Kind == "result" {
			reported = true
		}
	})
	if provider.calls != 0 || !cleaned || !reported {
		t.Fatal("goals model was requested or assembly failure was hidden")
	}
}

func TestIntentionallyEmptyResolvedSetDoesNotRestoreSDKDefaults(t *testing.T) {
	oldAugment, oldResolve := ToolAugment, ToolResolve
	t.Cleanup(func() { ToolAugment, ToolResolve = oldAugment, oldResolve })
	ToolAugment = nil
	ToolResolve = func(context.Context, string, []actool.CoreTool) []actool.CoreTool { return nil }
	tools, def, cleanup := AugmentTools(t.Context(), "goals", actool.DefaultTools())
	defer cleanup()
	if def.AssemblyError != nil || tools == nil || len(tools) != 0 {
		t.Fatal("an empty permitted set became a nil SDK default-tools request")
	}
	provider := &credentialFixtureProvider{reply: "fixture complete", stop: "end_turn"}
	DecomposeGoalsWithProvider(t.Context(), provider, t.TempDir(), "fixture", "fixture", nil, nil, 29, true, 57, nil)
	if provider.calls != 1 || len(provider.request.Tools) != 0 {
		t.Fatal("SDK default execution tools reappeared after all goal tools were removed")
	}
}

func TestRestrictedChatsDoNotGainImplicitSDKTools(t *testing.T) {
	oldAugment, oldResolve, oldPrompt := ToolAugment, ToolResolve, PromptOverride
	t.Cleanup(func() { ToolAugment, ToolResolve, PromptOverride = oldAugment, oldResolve, oldPrompt })
	PromptOverride = func(key string) (string, bool) {
		if key == db.CredentialAgentKey {
			return CredentialProcessorPrompt, true
		}
		return "", false
	}
	for _, key := range []string{db.CredentialAgentKey, "goals"} {
		ToolAugment, ToolResolve = nil, nil
		if key == db.CredentialAgentKey {
			ToolAugment = func(context.Context, string) ([]actool.CoreTool, DeferredInfo, func()) {
				t.Fatal("data-only processor initialized Skill/MCP/host dependencies")
				return nil, DeferredInfo{}, nil
			}
			ToolResolve = func(context.Context, string, []actool.CoreTool) []actool.CoreTool {
				t.Fatal("data-only processor consulted executable tool grants")
				return nil
			}
		}
		provider := &credentialFixtureProvider{reply: `{"accounts":[]}`, stop: "end_turn"}
		chat := NewChatAgent(provider, "fixture", t.TempDir(), nil, 0)
		chat.SetNonStreaming(func() bool { return true })
		chat.SetWebSearch(WebSearchOpts{Enabled: true, Backend: "ddgs"})
		if _, err := chat.Chat(t.Context(), key, "fixture-"+key, "fixture content", 3, 0, true, nil); err != nil {
			t.Fatalf("restricted %s chat failed: %v", key, err)
		}
		if provider.calls != 1 || len(provider.request.Tools) != 0 {
			t.Fatalf("restricted %s chat gained SDK fetch/search/todo/background/default tools", key)
		}
		if key == db.CredentialAgentKey && !strings.Contains(strings.Join(provider.request.System, "\n"), "输出纯 JSON") {
			t.Fatal("processor chat lost its structured-output instruction")
		}
	}
}

func TestIntranetVariantCannotRunIndependentChat(t *testing.T) {
	provider := &credentialFixtureProvider{}
	chat := NewChatAgent(provider, "fixture", t.TempDir(), nil, 0)
	if _, err := chat.Chat(t.Context(), "worker.intranet", "fixture-variant", "fixture", 3, 0, true, nil); err == nil {
		t.Fatal("prompt variant accepted an independent execution identity")
	}
	if provider.calls != 0 {
		t.Fatal("independent prompt variant requested the model")
	}
}

type restrictedLoopFixtureProvider struct {
	credentialFixtureProvider
	truncate bool
	badTools bool
}

func (p *restrictedLoopFixtureProvider) Complete(_ context.Context, request llm.CompletionRequest) (llm.Message, string, llm.Usage, error) {
	p.calls++
	if len(request.Tools) != 0 {
		p.badTools = true
	}
	if p.truncate {
		return llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{llm.TextBlock("fixture partial")}}, "max_tokens", llm.Usage{}, nil
	}
	return llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{{
		Type: llm.BlockToolUse, ID: fmt.Sprintf("missing-%d", p.calls), Name: "fixture_missing_tool", Input: json.RawMessage(`{}`),
	}}}, "tool_use", llm.Usage{}, nil
}

func TestRestrictedChatsBoundUnknownToolAndTokenRecoveryCalls(t *testing.T) {
	oldAugment, oldResolve := ToolAugment, ToolResolve
	t.Cleanup(func() { ToolAugment, ToolResolve = oldAugment, oldResolve })
	ToolAugment, ToolResolve = nil, nil
	for _, tc := range []struct {
		key      string
		turns    int
		want     int
		truncate bool
	}{
		{db.CredentialAgentKey, 0, 1, false},
		{"goals", 0, 8, false},
		{"goals", 3, 3, false},
		{db.CredentialAgentKey, 0, 1, true},
		{"goals", 0, 8, true},
	} {
		t.Run(fmt.Sprintf("%s-%d-truncate-%v", tc.key, tc.turns, tc.truncate), func(t *testing.T) {
			provider := &restrictedLoopFixtureProvider{truncate: tc.truncate}
			chat := NewChatAgent(provider, "fixture", t.TempDir(), nil, 0)
			chat.SetNonStreaming(func() bool { return true })
			chat.SetNoaEnabled(func() bool { return true })
			_, _ = chat.Chat(t.Context(), tc.key, "fixture-bounded", "fixture", tc.turns, 0, true, nil)
			// SDK recovery itself is bounded by three retries, so the goals
			// truncation case may stop before its total-call ceiling of eight.
			if provider.calls <= 0 || provider.calls > tc.want || provider.badTools {
				t.Fatalf("restricted chat calls=%d ceiling=%d exposed_tools=%v", provider.calls, tc.want, provider.badTools)
			}
			if !tc.truncate && provider.calls != tc.want {
				t.Fatalf("unknown-tool loop used %d calls instead of its fixed budget %d", provider.calls, tc.want)
			}
		})
	}
}

func TestDeferredAvailabilityWarningsDoNotAdvertiseUnavailableServices(t *testing.T) {
	def := DeferredInfo{
		AssemblyWarnings: []string{`MCP "optional-fixture" 当前不可用（连接未就绪），本轮未装配该服务工具。`},
		GlobalNames:      []string{"mcp__ready-fixture__inspect"},
	}
	system, _ := deferredSystem("CUSTOM BODY", def)
	if !strings.HasPrefix(system[0], "CUSTOM BODY") || !strings.Contains(system[0], "本轮工具可用性诊断") || !strings.Contains(system[0], "步骤记录为受阻") {
		t.Fatal("optional service diagnostics were not visible while preserving the custom prompt")
	}
	if len(system) != 2 || strings.Contains(system[1], "optional-fixture") || !strings.Contains(system[1], "mcp__ready-fixture__inspect") {
		t.Fatal("unavailable service was advertised or an available service was hidden")
	}
}
