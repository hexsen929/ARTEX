//go:build ignore

package server

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"

	"github.com/Autumn-27/artex/agent"
	"github.com/Autumn-27/norma/llm"
)

func TestGoalsDecompositionUsesRuntimeMeterAndTaskAttribution(t *testing.T) {
	s, f, _ := trafficEvidenceServer(t)
	saved, _ := s.m.pg.GetFinding(f.FindingID)
	task := s.m.ResolveTask(fmt.Sprint(*saved.TaskID))
	calls := 0
	provider := retestProvider{complete: func(context.Context, llm.CompletionRequest) (llm.Message, string, llm.Usage, error) {
		calls++
		if calls == 1 {
			return llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{{Type: llm.BlockToolUse, ID: "scope", Name: "add_task_scope", Input: json.RawMessage(`{"kind":"ip","value":"127.0.0.1","reason":"explicit fixture"}`)}}}, "tool_use", llm.Usage{}, nil
		}
		if calls == 2 {
			return llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{{Type: llm.BlockToolUse, ID: "goals", Name: "set_goals", Input: json.RawMessage(`{"goals":[{"text":"check local fixture","goal_type":"inspection","required_checks":["one local fixture response"]}]}`)}}}, "tool_use", llm.Usage{}, nil
		}
		return llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{llm.TextBlock("saved")}}, "end_turn", llm.Usage{}, nil
	}}
	goals := agent.DecomposeGoalsWithProvider(t.Context(), provider, t.TempDir(), "check local fixture", "127.0.0.1", s.m.Assets(), task.Store, *saved.TaskID, true, 0, nil)
	if len(goals) != 1 {
		t.Fatal("decomposition did not persist", goals)
	}
	for _, tool := range []string{"set_goals", "add_task_scope"} {
		var count int
		if err := s.m.pg.QueryRow(`SELECT count(*) FROM tool_usage WHERE task_id=$1 AND tool_key=$2 AND agent_key='goals' AND outcome='success' AND session_id=$3`, *saved.TaskID, tool, fmt.Sprintf("exp%d-goals", task.ExpID)).Scan(&count); err != nil || count != 1 {
			t.Fatal("decomposer call missing or wrongly attributed", tool, count, err)
		}
	}
}
