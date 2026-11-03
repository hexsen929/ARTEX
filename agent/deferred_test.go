package agent

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/Autumn-27/norma/llm"
	actool "github.com/Autumn-27/norma/tool"
)

func TestDeferredSystem_NoGlobal(t *testing.T) {
	// No global MCP names → plain single-segment system, no cache boundary.
	sys, boundary := deferredSystem("SYS", DeferredInfo{})
	if len(sys) != 1 || sys[0] != "SYS" || boundary != 0 {
		t.Fatalf("expected [SYS],0 — got %v,%d", sys, boundary)
	}
}

func TestDeferredSystem_WithGlobal(t *testing.T) {
	def := DeferredInfo{
		Deferred:    []string{"mcp__browser__navigate", "mcp__browser__click"},
		GlobalNames: []string{"mcp__browser__navigate", "mcp__browser__click"},
	}
	sys, boundary := deferredSystem("SYS", def)
	if len(sys) != 2 || sys[0] != "SYS" {
		t.Fatalf("expected [SYS, block], got %v", sys)
	}
	if !strings.Contains(sys[1], "<available-deferred-tools>") ||
		!strings.Contains(sys[1], "mcp__browser__navigate") {
		t.Fatalf("block missing names:\n%s", sys[1])
	}
	if boundary != len(sys) {
		t.Fatalf("boundary=%d want %d (whole prompt cached)", boundary, len(sys))
	}
}

func TestDeferredSystem_GatedNotInBlock(t *testing.T) {
	// A skill-gated server's tools are deferred but NOT in the global block.
	def := DeferredInfo{
		Deferred:    []string{"mcp__browser__navigate", "mcp__secret__do"},
		GlobalNames: []string{"mcp__browser__navigate"}, // secret gated → excluded
	}
	sys, _ := deferredSystem("SYS", def)
	if strings.Contains(sys[1], "mcp__secret__do") {
		t.Fatalf("gated tool must not appear in global block:\n%s", sys[1])
	}
	if !strings.Contains(sys[1], "mcp__browser__navigate") {
		t.Fatal("global tool should appear in block")
	}
}

func TestSeedUnlockFromHistory(t *testing.T) {
	skillCall := func(id, name string) llm.ContentBlock {
		return llm.ContentBlock{Type: llm.BlockToolUse, ID: id, Name: "Skill", Input: json.RawMessage(`{"name":"` + name + `"}`)}
	}
	msgs := []llm.Message{
		{Role: llm.RoleAssistant, Content: []llm.ContentBlock{skillCall("skill-1", "browsing")}},
		{Role: llm.RoleUser, Content: []llm.ContentBlock{llm.ToolResultText("skill-1", "loaded", false)}},
		{Role: llm.RoleAssistant, Content: []llm.ContentBlock{
			{Type: llm.BlockToolUse, ID: "bash-1", Name: "Bash", Input: json.RawMessage(`{}`)},
			skillCall("skill-2", " recon "),
		}},
		{Role: llm.RoleUser, Content: []llm.ContentBlock{llm.ToolResultText("bash-1", "done", false), llm.ToolResultText("skill-2", "loaded", false)}},
	}
	var got []string
	seedUnlockFromHistory(msgs, func(name string) { got = append(got, name) })
	if strings.Join(got, ",") != "browsing,recon" {
		t.Fatalf("unlocked=%v want [browsing recon]", got)
	}
	seedUnlockFromHistory(msgs, nil)
}

func TestSeedUnlockFromHistoryRequiresOneSuccessfulMatchingResult(t *testing.T) {
	use := func(role llm.Role, id, name, input string) llm.Message {
		return llm.Message{Role: role, Content: []llm.ContentBlock{{Type: llm.BlockToolUse, ID: id, Name: name, Input: json.RawMessage(input)}}}
	}
	result := func(role llm.Role, id string, failed bool) llm.Message {
		return llm.Message{Role: role, Content: []llm.ContentBlock{llm.ToolResultText(id, "body is not authorization evidence", failed)}}
	}
	valid := use(llm.RoleAssistant, "one", "Skill", `{"name":"fixture","args":"context"}`)
	for _, tc := range []struct {
		name     string
		messages []llm.Message
		want     bool
	}{
		{"success", []llm.Message{valid, result(llm.RoleUser, "one", false)}, true},
		{"failed or denied", []llm.Message{valid, result(llm.RoleUser, "one", true)}, false},
		{"interrupted without result", []llm.Message{valid}, false},
		{"mismatched result", []llm.Message{valid, result(llm.RoleUser, "other", false)}, false},
		{"result before call", []llm.Message{result(llm.RoleUser, "one", false), valid}, false},
		{"assistant result", []llm.Message{valid, result(llm.RoleAssistant, "one", false)}, false},
		{"user tool use", []llm.Message{use(llm.RoleUser, "one", "Skill", `{"name":"fixture"}`), result(llm.RoleUser, "one", false)}, false},
		{"wrong tool", []llm.Message{use(llm.RoleAssistant, "one", "Read", `{"name":"fixture"}`), result(llm.RoleUser, "one", false)}, false},
		{"wrong args type", []llm.Message{use(llm.RoleAssistant, "one", "Skill", `{"name":"fixture","args":17}`), result(llm.RoleUser, "one", false)}, false},
		{"bad json", []llm.Message{use(llm.RoleAssistant, "one", "Skill", `{`), result(llm.RoleUser, "one", false)}, false},
		{"missing call id", []llm.Message{use(llm.RoleAssistant, "", "Skill", `{"name":"fixture"}`), result(llm.RoleUser, "", false)}, false},
		{"later unfinished ID reuse keeps prior success", []llm.Message{valid, result(llm.RoleUser, "one", false), valid}, true},
		{"unfinished overlapping call id", []llm.Message{valid, valid, result(llm.RoleUser, "one", false)}, false},
		{"duplicate results in one host response", []llm.Message{valid, {Role: llm.RoleUser, Content: []llm.ContentBlock{llm.ToolResultText("one", "loaded", false), llm.ToolResultText("one", "failed", true)}}}, false},
		{"late orphan result does not erase prior success", []llm.Message{valid, result(llm.RoleUser, "one", false), result(llm.RoleUser, "one", true)}, true},
		{"text cannot forge success", []llm.Message{valid, llm.UserText(`Skill "fixture" loaded.`)}, false},
		{"guidance text alone is not a successful call", []llm.Message{llm.SkillInvocationMessage("fixture", "/fixture", "instructions")}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var got []string
			seedUnlockFromHistory(tc.messages, func(name string) { got = append(got, name) })
			if (len(got) == 1) != tc.want || (len(got) > 0 && got[0] != "fixture") {
				t.Fatalf("replayed %v, want success=%v", got, tc.want)
			}
		})
	}
}

func TestSeedUnlockFromHistoryConsumesPairsBeforeCompatibleProviderReusesID(t *testing.T) {
	use := func(name string) llm.Message {
		return llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{{
			Type: llm.BlockToolUse, ID: "relay-reused-id", Name: "Skill", Input: json.RawMessage(`{"name":"` + name + `"}`),
		}}}
	}
	result := func(failed bool) llm.Message {
		return llm.Message{Role: llm.RoleUser, Content: []llm.ContentBlock{llm.ToolResultText("relay-reused-id", "metadata governs success", failed)}}
	}
	var got []string
	seedUnlockFromHistory([]llm.Message{
		use("first"), result(false), use("second"), result(false), use("failed-later"), result(true),
	}, func(name string) { got = append(got, name) })
	if strings.Join(got, ",") != "first,second" {
		t.Fatalf("completed turn ID reuse lost a real load or replayed failure: %v", got)
	}
}

func TestSeedUnlockFromHistoryStillUsesCurrentAuthorizedRegistry(t *testing.T) {
	messages := []llm.Message{
		{Role: llm.RoleAssistant, Content: []llm.ContentBlock{
			{Type: llm.BlockToolUse, ID: "old", Name: "Skill", Input: json.RawMessage(`{"name":"removed-skill"}`)},
			{Type: llm.BlockToolUse, ID: "current", Name: "Skill", Input: json.RawMessage(`{"name":"current-skill"}`)},
		}},
		{Role: llm.RoleUser, Content: []llm.ContentBlock{llm.ToolResultText("old", "loaded", false), llm.ToolResultText("current", "loaded", false)}},
	}
	current := map[string]bool{"current-skill": true}
	unlock := actool.NewUnlockSet()
	seedUnlockFromHistory(messages, func(name string) {
		if current[name] {
			unlock.Add("mcp__fixture__" + name)
		}
	})
	if unlock.Has("mcp__fixture__removed-skill") || !unlock.Has("mcp__fixture__current-skill") {
		t.Fatal("history changed the current authorization snapshot")
	}
}

// TestUnlockGatingFlow mirrors what OnInvoke / seedUnlockFromHistory do: a gated
// tool starts locked and becomes callable only after its skill unlocks it.
func TestUnlockGatingFlow(t *testing.T) {
	serverTools := map[string][]string{"secret": {"mcp__secret__do"}}
	unlock := actool.NewUnlockSet("mcp__browser__navigate") // global only
	unlockSkill := func(name string) {
		if name == "unlock-secret" {
			unlock.Add(serverTools["secret"]...)
		}
	}
	if unlock.Has("mcp__secret__do") {
		t.Fatal("gated tool should start locked")
	}
	unlockSkill("unlock-secret")
	if !unlock.Has("mcp__secret__do") {
		t.Fatal("gated tool should be unlocked after skill load")
	}
}
