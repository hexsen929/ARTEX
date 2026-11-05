package server

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Autumn-27/artex/agent"
	"github.com/Autumn-27/artex/db"
	"github.com/Autumn-27/norma/llm"
)

func TestExecutionEnginePersistsActualToolCheckpoint(t *testing.T) {
	t.Run("complete", func(t *testing.T) { testEngineToolCheckpoint(t, false) })
	t.Run("missing_activity", func(t *testing.T) { testEngineToolCheckpoint(t, true) })
}

func testEngineToolCheckpoint(t *testing.T, loseActivity bool) {
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
	if loseActivity {
		name := fmt.Sprintf("activity_checkpoint_fail_%d", time.Now().UnixNano())
		if _, err := pg.Exec(`CREATE FUNCTION ` + name + `() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN IF NEW.node_id=` + fmt.Sprint(id) + ` AND NEW.kind IN ('tool_use','tool_result') THEN RAISE EXCEPTION 'fixture activity write failed'; END IF; RETURN NEW; END $$`); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _, _ = pg.Exec(`DROP FUNCTION IF EXISTS ` + name + `()`) })
		if _, err := pg.Exec(`CREATE TRIGGER ` + name + ` BEFORE INSERT ON activity FOR EACH ROW EXECUTE FUNCTION ` + name + `()`); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _, _ = pg.Exec(`DROP TRIGGER IF EXISTS ` + name + ` ON activity`) })
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
	if loseActivity {
		if calls.Load() != 2 || cp == nil || !cp.Incomplete || !cp.Settled || cp.ToolCalls != 0 || cp.ToolResults != 0 {
			t.Fatalf("lost activity was treated as complete: calls=%d checkpoint=%+v", calls.Load(), cp)
		}
		return
	}
	if calls.Load() != 2 || node.State != "done" || cp == nil || cp.ToolCalls != 1 || cp.ToolResults != 1 || len(cp.Pending) != 0 || cp.Incomplete || !cp.Settled || dto.ExecutionAttempt != 1 || cp.LastResultID == 0 {
		t.Fatalf("actual engine checkpoint missing: calls=%d node=%+v checkpoint=%+v", calls.Load(), node, cp)
	}
	var generation string
	if err := pg.QueryRow(`SELECT metadata->>'worker_run_token' FROM activity WHERE id=$1`, cp.LastResultID).Scan(&generation); err != nil || generation != cp.RunToken {
		t.Fatal("event generation was not wired", generation, err)
	}
}
