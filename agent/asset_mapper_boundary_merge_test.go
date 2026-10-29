package agent

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	actool "github.com/Autumn-27/norma/tool"
)

func mapperReadFixture(name string) actool.CoreTool {
	return actool.Build(actool.Spec{
		Name: name, Schema: map[string]any{"type": "object"},
		ReadOnly: func(json.RawMessage) bool { return true },
		Run: func(context.Context, json.RawMessage, *actool.ToolContext) (actool.Result, error) {
			return actool.Text("registered asset fixture"), nil
		},
	})
}

func TestAssetMapperBoundarySkipsAugmentationAndKeepsOnlyCatalogReads(t *testing.T) {
	oldAugment, oldResolve := ToolAugment, ToolResolve
	t.Cleanup(func() { ToolAugment, ToolResolve = oldAugment, oldResolve })
	ToolAugment = func(context.Context, string) ([]actool.CoreTool, DeferredInfo, func()) {
		t.Fatal("asset mapper initialized Skill/MCP/host dependencies")
		return nil, DeferredInfo{}, nil
	}
	ToolResolve = func(_ context.Context, role string, tools []actool.CoreTool) []actool.CoreTool {
		if role != "asset_mapper" || tools == nil || len(tools) != 0 {
			t.Fatal("asset mapper supplied implicit SDK defaults")
		}
		return []actool.CoreTool{mapperReadFixture("list_assets"), mapperReadFixture("list_companies")}
	}
	tools, def, cleanup := AugmentTools(t.Context(), "asset_mapper", []actool.CoreTool{})
	defer cleanup()
	if def.AssemblyError != nil || len(tools) != 2 || len(def.SkillDirs) != 0 || len(def.Deferred) != 0 || def.Unlock != nil || def.UnlockSkill != nil || def.CaseGuidance != "" || def.IntelligenceGuidance != "" {
		t.Fatal("mapper exceeded its dependency/guidance boundary", def.AssemblyError)
	}
	for _, tool := range tools {
		if !IsAssetMapperToolAllowed(tool.Name()) || !tool.IsReadOnly(nil) {
			t.Fatalf("mapper received unsupported tool %s", tool.Name())
		}
		result, err := tool.Call(t.Context(), json.RawMessage(`{}`), nil)
		if err != nil || result.IsError {
			t.Fatal("permitted read is not callable", err)
		}
	}
}

func TestAssetMapperBoundaryRejectsSDKAndResolverEscalation(t *testing.T) {
	oldAugment, oldResolve := ToolAugment, ToolResolve
	t.Cleanup(func() { ToolAugment, ToolResolve = oldAugment, oldResolve })
	ToolAugment = func(context.Context, string) ([]actool.CoreTool, DeferredInfo, func()) {
		t.Fatal("mapper consulted augmentation despite its hard ceiling")
		return nil, DeferredInfo{}, nil
	}
	for _, name := range []string{"Bash", "Read", "Write", "Edit", "WebFetch", "WebSearch", "Browser", "Skill", "read_skill_reference", "mcp__fixture__read", "shell_open", "record_fact", "session_exec"} {
		t.Run(name, func(t *testing.T) {
			for _, injectBase := range []bool{false, true} {
				base := []actool.CoreTool{}
				unsafe := mapperReadFixture(name)
				ToolResolve = func(context.Context, string, []actool.CoreTool) []actool.CoreTool {
					if injectBase {
						t.Fatal("unsafe caller base reached metadata resolution")
					}
					return []actool.CoreTool{mapperReadFixture("list_assets"), unsafe}
				}
				if injectBase {
					base = append(base, unsafe)
				}
				tools, def, cleanup := AugmentTools(t.Context(), "asset_mapper", base)
				cleanup()
				if tools == nil || len(tools) != 0 || def.AssemblyError == nil {
					t.Fatalf("unsupported tool %s escaped mapper boundary", name)
				}
			}
		})
	}
	ToolResolve = func(context.Context, string, []actool.CoreTool) []actool.CoreTool {
		return []actool.CoreTool{actool.Build(actool.Spec{Name: "list_assets", Schema: map[string]any{"type": "object"}})}
	}
	tools, def, cleanup := AugmentTools(t.Context(), "asset_mapper", nil)
	cleanup()
	if len(tools) != 0 || def.AssemblyError == nil {
		t.Fatal("a writable handler borrowed a read-only tool name")
	}
}

func TestAssetMapperBoundaryChatHasNoImplicitSDKCapabilities(t *testing.T) {
	oldAugment, oldResolve := ToolAugment, ToolResolve
	t.Cleanup(func() { ToolAugment, ToolResolve = oldAugment, oldResolve })
	ToolAugment = func(context.Context, string) ([]actool.CoreTool, DeferredInfo, func()) {
		t.Fatal("mapper chat consulted Skill/MCP dependencies")
		return nil, DeferredInfo{}, nil
	}
	ToolResolve = func(_ context.Context, role string, base []actool.CoreTool) []actool.CoreTool {
		if role != "asset_mapper" || base == nil || len(base) != 0 {
			t.Fatal("mapper chat retained generic chat defaults")
		}
		return []actool.CoreTool{mapperReadFixture("list_assets"), mapperReadFixture("list_companies")}
	}
	provider := &credentialFixtureProvider{reply: "registered assets only", stop: "end_turn"}
	chat := NewChatAgent(provider, "fixture", t.TempDir(), nil, 0)
	chat.SetNonStreaming(func() bool { return true })
	chat.SetNoaEnabled(func() bool { return true })
	chat.SetWebSearch(WebSearchOpts{Enabled: true, Backend: "ddgs"})
	if _, err := chat.Chat(t.Context(), "asset_mapper", "mapper-fixture", "explain registered assets", 0, 0, true, nil); err != nil {
		t.Fatal(err)
	}
	if provider.calls != 1 || len(provider.request.Tools) != 2 {
		t.Fatalf("mapper received SDK defaults or implicit network tools: %+v", provider.request.Tools)
	}
	for _, tool := range provider.request.Tools {
		if !IsAssetMapperToolAllowed(tool.Name) {
			t.Fatalf("implicit capability %s reached the model", tool.Name)
		}
	}
	system := strings.Join(provider.request.System, "\n")
	if !strings.Contains(system, "只读资产梳理边界") || strings.Contains(system, "文件输出规约") {
		t.Fatal("read-only mapper was instructed to write files")
	}
}

func TestAssetMapperBoundaryEmptyCatalogAndFailureStayClosed(t *testing.T) {
	oldAugment, oldResolve := ToolAugment, ToolResolve
	t.Cleanup(func() { ToolAugment, ToolResolve = oldAugment, oldResolve })
	ToolAugment = nil
	ToolResolve = func(context.Context, string, []actool.CoreTool) []actool.CoreTool { return nil }
	tools, def, cleanup := AugmentTools(t.Context(), "asset_mapper", nil)
	cleanup()
	if tools == nil || len(tools) != 0 || def.AssemblyError != nil {
		t.Fatal("empty mapper catalog restored SDK default tools")
	}
	problem := errors.New("fixture catalog unavailable")
	ToolResolve = func(context.Context, string, []actool.CoreTool) []actool.CoreTool {
		return ToolAssemblyFailure(problem)
	}
	chat := NewChatAgent(nil, "fixture", t.TempDir(), nil, 0)
	if _, err := chat.Chat(t.Context(), "asset_mapper", "mapper-failure", "fixture", 8, 0, true, nil); !errors.Is(err, problem) {
		t.Fatal("mapper catalog failure did not stop before the nil model", err)
	}
}

func TestAssetMapperBoundaryKeepsFixedCallBudget(t *testing.T) {
	oldAugment, oldResolve := ToolAugment, ToolResolve
	t.Cleanup(func() { ToolAugment, ToolResolve = oldAugment, oldResolve })
	ToolAugment, ToolResolve = nil, nil
	provider := &restrictedLoopFixtureProvider{}
	chat := NewChatAgent(provider, "fixture", t.TempDir(), nil, 0)
	chat.SetNonStreaming(func() bool { return true })
	_, _ = chat.Chat(t.Context(), "asset_mapper", "mapper-loop", "fixture", 0, 0, true, nil)
	if provider.calls != 8 || provider.badTools {
		t.Fatalf("mapper unbounded loop or implicit capabilities: calls=%d tools=%v", provider.calls, provider.badTools)
	}
}
