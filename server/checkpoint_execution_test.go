package server

import (
	"context"
	"encoding/json"
	"strconv"
	"sync/atomic"
	"testing"

	"github.com/Autumn-27/artex/agent"
	"github.com/Autumn-27/artex/db"
	"github.com/Autumn-27/norma/llm"
)

func TestExecutionEnginePersistsActualToolCheckpoint(t *testing.T) {
	pg := taskToolRegistryPG(t)
	m := proxyTestManager(t)
	task, err := m.CreateTask("checkpoint real worker fixture", "read fixture only", nil, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = m.DeleteTask(task.ID, DeleteTaskOptions{}) })
	fact, err := task.Store.AddNode(db.KindFact, map[string]any{"summary": "CHECKPOINT_FIXTURE"}, 1, "confirmed", "fixture", nil)
	if err != nil {
		t.Fatal(err)
	}
	id, err := task.Store.AddIntent(map[string]any{"summary": "read synthetic fact"}, 5, nil, "fixture")
	if err != nil {
		t.Fatal(err)
	}
	if ok, err := task.Store.ClaimIntent(id, "fixture-worker"); err != nil || !ok {
		t.Fatal(ok, err)
	}
	intent, err := task.Store.GetNode(id)
	if err != nil {
		t.Fatal(err)
	}
	var calls atomic.Int64
	provider := retestProvider{complete: func(_ context.Context, req llm.CompletionRequest) (llm.Message, string, llm.Usage, error) {
		if calls.Add(1) == 1 {
			return llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{{Type: llm.BlockToolUse, ID: "checkpoint-read", Name: "node_detail", Input: json.RawMessage(`{"id":` + strconv.FormatInt(fact, 10) + `}`)}}}, "tool_use", llm.Usage{}, nil
		}
		return llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{llm.TextBlock("Fixture read completed.")}}, "end_turn", llm.Usage{}, nil
	}}
	worker := agent.NewWorker(provider, "fixture", t.TempDir(), nil, 0, 4)
	worker.SetNonStreaming(func() bool { return true })
	engine := NewEngine(m)
	t.Cleanup(func() { engine.StopTask(task.ID) })
	if !engine.runIntent(t.Context(), task, "fixture-worker", worker, intent, "", "") {
		t.Fatal("worker did not run")
	}
	node, err := task.Store.GetNode(id)
	if err != nil {
		t.Fatal(err)
	}
	dto := taskNodeDTO(node)
	cp := dto.ExecutionCheckpoint
	if calls.Load() != 2 || node.State != "done" || cp == nil || cp.ToolCalls != 1 || cp.ToolResults != 1 || len(cp.Pending) != 0 || cp.Incomplete || !cp.Settled || dto.ExecutionAttempt != 1 || cp.LastResultID == 0 {
		t.Fatalf("actual engine checkpoint missing: calls=%d node=%+v checkpoint=%+v", calls.Load(), node, cp)
	}
	var generation string
	if err := pg.QueryRow(`SELECT metadata->>'worker_run_token' FROM activity WHERE id=$1`, cp.LastResultID).Scan(&generation); err != nil || generation != cp.RunToken {
		t.Fatal("event generation was not wired", generation, err)
	}
}
