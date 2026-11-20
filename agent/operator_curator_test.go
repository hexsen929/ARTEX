package agent

import (
	"context"
	actool "github.com/Autumn-27/norma/tool"
	"testing"
)

func TestOperatorCuratorRetainsOriginalAssistantTools(t *testing.T) {
	augment, resolve, recall, supplement := ToolAugment, ToolResolve, MemoryRecall, OperatorSupplement
	t.Cleanup(func() {
		ToolAugment, ToolResolve, MemoryRecall, OperatorSupplement = augment, resolve, recall, supplement
	})
	ToolAugment = nil
	MemoryRecall = nil
	OperatorSupplement = nil
	seen := map[string]bool{}
	ToolResolve = func(ctx context.Context, role string, tools []actool.CoreTool) []actool.CoreTool {
		if role != "kb_curator" {
			t.Fatal(role)
		}
		for _, tool := range tools {
			seen[tool.Name()] = true
		}
		return tools
	}
	provider := &credentialFixtureProvider{reply: "fixture complete", stop: "end_turn"}
	chat := NewChatAgent(provider, "fixture", t.TempDir(), nil, 1)
	chat.SetNonStreaming(func() bool { return true })
	if _, err := chat.Chat(t.Context(), "kb_curator", "curator-fixture", "fixture", 1, 0, false, nil); err != nil {
		t.Fatal(err)
	}
	if !seen["Bash"] || !seen["Read"] {
		t.Fatal("curator was silently converted to a data-only role", seen)
	}
}
