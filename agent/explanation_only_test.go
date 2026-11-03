package agent

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/Autumn-27/artex/db"
	"github.com/Autumn-27/norma/llm"
	actool "github.com/Autumn-27/norma/tool"
	"github.com/Autumn-27/norma/transcript"
)

func explanationFixtureHooks(t *testing.T) {
	t.Helper()
	oldAugment, oldResolve, oldRecall := ToolAugment, ToolResolve, MemoryRecall
	t.Cleanup(func() { ToolAugment, ToolResolve, MemoryRecall = oldAugment, oldResolve, oldRecall })
	ToolAugment = func(context.Context, string) ([]actool.CoreTool, DeferredInfo, func()) {
		t.Fatal("historical explanation initialized execution dependencies")
		return nil, DeferredInfo{}, nil
	}
	ToolResolve = func(context.Context, string, []actool.CoreTool) []actool.CoreTool {
		t.Fatal("historical explanation resolved execution bindings")
		return nil
	}
	MemoryRecall = func(context.Context, RunInfo, string, llm.CompletionRequest) (MemoryRecallResult, error) {
		t.Fatal("historical explanation recalled unrelated shared memory")
		return MemoryRecallResult{}, nil
	}
}

func TestExplanationOnlyChatHasNoExecutionOrAuxiliaryCalls(t *testing.T) {
	explanationFixtureHooks(t)
	for _, role := range []string{"verifier", "retester", "worker"} {
		ctx, _, _ := taskRegistryFixture("worker")
		provider := &credentialFixtureProvider{reply: "Only the saved observation is explained.", stop: "end_turn"}
		chat := NewChatAgent(provider, "fixture", t.TempDir(), nil, 1)
		chat.SetNonStreaming(func() bool { return true })
		chat.SetProxyProvider(func() string { t.Fatal("explanation consulted target routing"); return "" })
		chat.SetNoaEnabled(func() bool { t.Fatal("explanation initialized compaction"); return true })
		chat.SetWebSearch(WebSearchOpts{Enabled: true, Backend: "deepseek", DeepSeekAPIKey: "fixture-search-key"})
		if _, err := chat.Chat(WithExplanationOnly(ctx), role, "historical-"+role, "Explain the saved synthetic record.", 0, 0, true, nil); err != nil {
			t.Fatal(err)
		}
		if provider.calls != 1 || len(provider.request.Tools) != 0 {
			t.Fatalf("explanation gained tools or extra provider calls: calls=%d tools=%d", provider.calls, len(provider.request.Tools))
		}
		system := strings.Join(provider.request.System, "\n")
		if !strings.Contains(system, "历史解释模式") || strings.Contains(system, "文件输出规约") {
			t.Fatal("explanation retained execution/workdir guidance")
		}
	}
}

func TestExplanationOnlyDoesNotRecoverToolLoopsOrTruncation(t *testing.T) {
	explanationFixtureHooks(t)
	for _, truncate := range []bool{false, true} {
		provider := &restrictedLoopFixtureProvider{truncate: truncate}
		chat := NewChatAgent(provider, "fixture", t.TempDir(), nil, 1)
		chat.SetNonStreaming(func() bool { return true })
		_, _ = chat.Chat(WithExplanationOnly(t.Context()), "verifier", "historical-loop", "Explain only.", 0, 0, true, nil)
		if provider.calls != 1 || provider.badTools {
			t.Fatal("historical explanation made auxiliary/recovery calls or received tools")
		}
	}
}

func TestExplanationOnlyPreservesSavedHistoryWithoutReplayingPendingTools(t *testing.T) {
	explanationFixtureHooks(t)
	store := transcript.NewStore(t.TempDir())
	w := store.NewWriter("historical-transcript", "")
	w.RecordMessage(llm.UserText("saved synthetic observation"), llm.Usage{})
	w.RecordMessage(llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{{Type: llm.BlockToolUse, ID: "old-pending", Name: "Bash", Input: json.RawMessage(`{"command":"must-not-run"}`)}}}, llm.Usage{})
	provider := &credentialFixtureProvider{reply: "Saved history was explained without testing.", stop: "end_turn"}
	chat := NewChatAgent(provider, "fixture", t.TempDir(), store, 1)
	chat.SetNonStreaming(func() bool { return true })
	var toolEvents int
	if _, err := chat.Chat(WithExplanationOnly(t.Context()), "retester", "historical-transcript", "Explain this saved history.", 0, 0, true, func(record db.Activity) {
		if record.Kind == "tool_use" || record.Kind == "tool_result" {
			toolEvents++
		}
	}); err != nil {
		t.Fatal(err)
	}
	wire, err := json.Marshal(provider.request.Messages)
	if err != nil {
		t.Fatal(err)
	}
	if provider.calls != 1 || toolEvents != 0 || !strings.Contains(string(wire), "saved synthetic observation") || strings.Contains(string(wire), "must-not-run") {
		t.Fatal("history explanation lost saved context or replayed an unfinished action")
	}
}
