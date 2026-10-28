package agent

import (
	"strings"
	"testing"
	"unicode/utf8"
)

func TestAgentWorkGuidanceIsRoleScopedAndBounded(t *testing.T) {
	for _, key := range []string{"goals", "planner", "mainagent", "auto", "worker", "worker.intranet", "pentest", "verifier", "retester", "reporter", "credential_processor"} {
		guidance := AgentWorkGuidance(key)
		if guidance == "" || !utf8.ValidString(guidance) || len(guidance) > 2048 {
			t.Fatalf("role %s has missing, invalid, or unbounded guidance (%d bytes)", key, len(guidance))
		}
		if !strings.Contains(guidance, "【角色工作规范 · "+key+" · "+AgentWorkGuidanceVersion+"】") {
			t.Fatalf("role %s lost its versioned identity", key)
		}
		if !strings.Contains(guidance, "不替换已有提示词、输出格式、任务范围、权限、审批或停止条件") {
			t.Fatalf("role %s lost the existing controls and output-format contract", key)
		}
	}
	for _, key := range []string{"", "custom_agent", "worker.custom", "Worker", "worker.intranet.extra"} {
		if AgentWorkGuidance(key) != "" || withAgentWorkGuidance("CUSTOM BODY", key) != "CUSTOM BODY" {
			t.Fatalf("unknown role %q inherited a built-in persona", key)
		}
	}
	if AgentWorkGuidance("worker") == AgentWorkGuidance("worker.intranet") {
		t.Fatal("intranet and ordinary worker guidance must be distinct")
	}
}

func TestAgentWorkGuidanceAppendIsIdempotentAndPreservesBody(t *testing.T) {
	const body = "用户自定义正文 {{.UnrenderedTemplate}}\n保持原始输出格式。"
	for _, key := range []string{"worker", "reporter", "credential_processor"} {
		got := withAgentWorkGuidance(body, key)
		if !strings.HasPrefix(got, body) || strings.Count(got, AgentWorkGuidance(key)) != 1 {
			t.Fatalf("role %s replaced the original body or duplicated its supplement", key)
		}
		if twice := withAgentWorkGuidance(got, key); twice != got {
			t.Fatalf("role %s re-appended an identical supplement", key)
		}
	}
}

func TestAgentWorkGuidanceUsesActualRuntimeAssemblyWithoutChangingRenderer(t *testing.T) {
	old := PromptOverride
	t.Cleanup(func() { PromptOverride = old })
	current := "CUSTOM BODY A"
	PromptOverride = func(key string) (string, bool) { return current + " {{.DataDir}}", true }
	if got := renderSystem("planner", "DEFAULT", PlannerVars{DataDir: "/fixture/data"}); got != "CUSTOM BODY A /fixture/data" {
		t.Fatal("renderSystem no longer returns only the editable rendered body")
	}
	checks := []struct {
		key    string
		prompt string
	}{
		{"planner", plannerSystem("fixture", "/fixture/data", "/fixture/work")},
		{"mainagent", mainAgentSystem("fixture", "/fixture/data", "/fixture/work")},
		{"worker", workerSystem("", "", "/fixture/data", "/fixture/work", false)},
		{"worker.intranet", workerSystem("", "", "/fixture/data", "/fixture/work", true)},
	}
	for _, key := range []string{"auto", "pentest", "verifier", "retester", "reporter", "credential_processor"} {
		checks = append(checks, struct {
			key    string
			prompt string
		}{key, chatSystem(key, "/fixture/data", "/fixture/work")})
	}
	for _, check := range checks {
		if !strings.HasPrefix(check.prompt, "CUSTOM BODY A /fixture/data") || strings.Count(check.prompt, AgentWorkGuidance(check.key)) != 1 {
			t.Fatalf("runtime role %s lost custom body or exact-once guidance", check.key)
		}
	}
	if strings.Contains(checks[3].prompt, AgentWorkGuidance("worker")) || !strings.Contains(checks[3].prompt, "内网红线") || !strings.Contains(checks[3].prompt, "反 AI 蜜罐红线") {
		t.Fatal("intranet role duplicated ordinary guidance or dropped fixed rule tails")
	}
	if custom := chatSystem("custom_agent", "/fixture/data", "/fixture/work"); strings.Contains(custom, "角色工作规范") || !strings.Contains(custom, "CUSTOM BODY A /fixture/data") {
		t.Fatal("custom agent was overridden or given a built-in supplement")
	}
	// Constructed runs are immutable snapshots; only a later construction uses
	// the new stored body. No prompt version or database mutation is involved.
	first := plannerSystem("fixture", "/fixture/data", "/fixture/work")
	current = "CUSTOM BODY B"
	second := plannerSystem("fixture", "/fixture/data", "/fixture/work")
	if !strings.Contains(first, "CUSTOM BODY A") || strings.Contains(first, "CUSTOM BODY B") || !strings.Contains(second, "CUSTOM BODY B") {
		t.Fatal("new construction did not preserve the existing run's prompt snapshot")
	}
}

func TestAgentWorkGuidanceGoalsRequestAndCredentialJSONContract(t *testing.T) {
	old := PromptOverride
	t.Cleanup(func() { PromptOverride = old })
	PromptOverride = func(key string) (string, bool) {
		if key == "goals" {
			return "CUSTOM GOAL BODY {{.DataDir}}", true
		}
		return "", false
	}
	goals := &credentialFixtureProvider{reply: "fixture complete", stop: "end_turn"}
	DecomposeGoalsWithProvider(t.Context(), goals, t.TempDir(), "fixture goal", "fixture description", nil, nil, 0, true, 100, nil)
	if goals.calls != 1 || len(goals.request.System) == 0 || !strings.Contains(strings.Join(goals.request.System, "\n"), AgentWorkGuidance("goals")) || !strings.Contains(strings.Join(goals.request.System, "\n"), "CUSTOM GOAL BODY") {
		t.Fatal("goals request lost editable body or runtime work guidance")
	}

	const custom = `CUSTOM CREDENTIAL BODY: 输出原字段的纯 JSON。`
	processor := &credentialFixtureProvider{reply: `{"accounts":[]}`, stop: "end_turn"}
	if _, _, err := ExtractCredentials(t.Context(), processor, custom, "fixture content with no credentials", 500); err != nil {
		t.Fatal(err)
	}
	request := processor.request
	if processor.calls != 1 || len(request.System) != 1 || !strings.HasPrefix(request.System[0], custom) || strings.Count(request.System[0], AgentWorkGuidance("credential_processor")) != 1 {
		t.Fatal("credential processor lost custom body, duplicated guidance, or added model calls")
	}
	if len(request.Tools) != 0 || len(request.Messages) != 1 || request.Thinking != "disabled" || !strings.Contains(request.System[0], "保持本角色要求的纯 JSON 结构") || strings.Contains(request.System[0], "按适用性列出目标与范围") {
		t.Fatal("credential supplement changed the bounded no-tools JSON-only contract")
	}
}
