package server

import (
	"context"
	"encoding/json"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/Autumn-27/artex/agent"
	"github.com/Autumn-27/artex/db"
	"github.com/Autumn-27/norma/llm"
	actool "github.com/Autumn-27/norma/tool"
)

func taskToolRegistryPG(t *testing.T) *db.DB {
	t.Helper()
	dsn := os.Getenv("ARTEX_PG_DSN")
	if dsn == "" {
		t.Skip("isolated CI PostgreSQL is not configured")
	}
	pg, err := db.Open(dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = pg.Close() })
	oldResolve, oldAugment, oldRecall, oldPrompt := agent.ToolResolve, agent.ToolAugment, agent.MemoryRecall, agent.PromptOverride
	oldTraffic, oldIntelligence, oldIntelligenceRecord := agent.FindingTrafficBindingEnabled, agent.IntelligenceEnabled, agent.IntelligenceRecord
	t.Cleanup(func() {
		agent.ToolResolve, agent.ToolAugment, agent.MemoryRecall, agent.PromptOverride = oldResolve, oldAugment, oldRecall, oldPrompt
		agent.FindingTrafficBindingEnabled, agent.IntelligenceEnabled, agent.IntelligenceRecord = oldTraffic, oldIntelligence, oldIntelligenceRecord
	})
	agent.ToolAugment, agent.MemoryRecall, agent.PromptOverride = nil, nil, nil
	t.Setenv("AGENT_CORE_DISABLE_INTERACTIVE_SHELL", "1")
	wireTools(pg, buildDomainReg(pg.Assets()))
	old, err := pg.GetTool("node_detail")
	if err != nil || old == nil {
		t.Fatalf("native catalog reader was not seeded: %v", err)
	}
	t.Cleanup(func() {
		agents, _ := json.Marshal(old.Agents)
		_, err := pg.Exec(`UPDATE tools SET system=$2,description=$3,schema=$4,agents=$5,enabled=$6,kind=$7,exec=$8,deferred=$9 WHERE key=$1`,
			old.Key, old.System, old.Description, old.Schema, agents, old.Enabled, old.Kind, old.Exec, old.Deferred)
		if err != nil {
			t.Errorf("restore task tool catalog row: %v", err)
		}
	})
	if _, err := pg.Exec(`UPDATE tools SET system=true,kind='builtin',enabled=true,agents='["worker"]' WHERE key='node_detail'`); err != nil {
		t.Fatal(err)
	}
	return pg
}

func taskToolRegistryTask(t *testing.T, pg *db.DB, label string, sources ...int64) (*db.Task, *db.ExplorationStore, int64) {
	t.Helper()
	task, err := pg.CreateTaskWithOptions("synthetic tool registry "+label, "Read registered synthetic records only", db.TaskCreateOptions{SourceTaskIDs: sources})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = pg.Exec(`DELETE FROM tool_usage WHERE task_id=$1`, task.ID)
		_, _ = pg.DeleteTaskCascade(task.ID, false, false)
	})
	store := pg.Exploration(task.ExplorationID)
	node, err := store.AddNode(db.KindFact, map[string]any{"summary": "registry-fixture-" + label}, 0, "confirmed", "fixture", nil)
	if err != nil {
		t.Fatal(err)
	}
	return task, store, node
}

func taskToolRegistryContext(task *db.Task, store *db.ExplorationStore) context.Context {
	ts := agent.NewToolSet(store, "fixture-worker")
	ts.SetTaskID(task.ID)
	ctx := agent.WithRunInfo(context.Background(), agent.RunInfo{TaskID: task.ID, ExplorationID: task.ExplorationID})
	return agent.WithTaskToolSet(ctx, "worker", ts)
}

func runTaskToolRegistryWorker(t *testing.T, pg *db.DB, task *db.Task, store *db.ExplorationStore, node int64, marker string) {
	t.Helper()
	intentID, err := store.AddIntent(map[string]any{"summary": "Read one synthetic registered fact"}, 1, nil, "fixture")
	if err != nil {
		t.Fatal(err)
	}
	if claimed, err := store.ClaimIntent(intentID, "fixture-worker"); err != nil || !claimed {
		t.Fatalf("claim synthetic native intent: %v %v", claimed, err)
	}
	intent, err := store.GetNode(intentID)
	if err != nil || intent == nil {
		t.Fatal("native fixture intent unavailable", err)
	}
	var calls atomic.Int64
	var readResult atomic.Bool
	provider := retestProvider{complete: func(_ context.Context, req llm.CompletionRequest) (llm.Message, string, llm.Usage, error) {
		if calls.Add(1) == 1 {
			found := false
			for _, tool := range req.Tools {
				found = found || tool.Name == "node_detail"
			}
			if !found {
				t.Error("DB-authorized optional reader was absent from the real Worker request")
			}
			return llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{{Type: llm.BlockToolUse, ID: "fixture-native-read", Name: "node_detail", Input: json.RawMessage(`{"id":` + strconv.FormatInt(node, 10) + `}`)}}}, "tool_use", llm.Usage{}, nil
		}
		return llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{llm.TextBlock("Synthetic registered record read completed.")}}, "end_turn", llm.Usage{}, nil
	}}
	worker := agent.NewWorker(provider, "fixture", t.TempDir(), nil, 0, 4)
	worker.SetNonStreaming(func() bool { return true })
	_, writes, err := worker.Execute(t.Context(), "fixture-worker", task.ID, pg.Assets(), store, intent, nil, func(record db.Activity) {
		if record.Kind == "tool_result" && record.Tool == "node_detail" && !record.IsError && strings.Contains(record.Detail, marker) {
			readResult.Store(true)
		}
	}, nil, nil)
	if err != nil || calls.Load() != 2 || !readResult.Load() {
		t.Fatalf("native Worker did not call the per-task reader: err=%v calls=%d read=%v", err, calls.Load(), readResult.Load())
	}
	if writes != (agent.WriteCounts{}) {
		t.Fatal("read-only fixture wrote exploration findings/facts/assets")
	}
}

func TestTaskToolRegistryWorkerExecuteUsesTaskLocalReader(t *testing.T) {
	pg := taskToolRegistryPG(t)
	task, store, node := taskToolRegistryTask(t, pg, "alpha")
	for _, tool := range agent.NewToolSet(store, "fixture").WorkerTools() {
		if tool.Name() == "node_detail" {
			t.Fatal("fixture no longer exercises the missing-base injection path")
		}
	}
	runTaskToolRegistryWorker(t, pg, task, store, node, "registry-fixture-alpha")
}

func TestTaskToolRegistryConcurrentWorkersStayIsolated(t *testing.T) {
	pg := taskToolRegistryPG(t)
	a, as, an := taskToolRegistryTask(t, pg, "concurrent-alpha")
	b, bs, bn := taskToolRegistryTask(t, pg, "concurrent-bravo")
	var wg sync.WaitGroup
	for _, f := range []struct {
		task   *db.Task
		store  *db.ExplorationStore
		node   int64
		marker string
	}{{a, as, an, "registry-fixture-concurrent-alpha"}, {b, bs, bn, "registry-fixture-concurrent-bravo"}} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			runTaskToolRegistryWorker(t, pg, f.task, f.store, f.node, f.marker)
		}()
	}
	wg.Wait()
	for _, f := range []struct {
		task  *db.Task
		store *db.ExplorationStore
		other int64
	}{{a, as, bn}, {b, bs, an}} {
		ctx := taskToolRegistryContext(f.task, f.store)
		tools, def, cleanup := agent.AugmentTools(ctx, "worker", agent.NewToolSet(f.store, "fixture").WorkerTools())
		cleanup()
		if def.AssemblyError != nil {
			t.Fatal(def.AssemblyError)
		}
		res, err := names(tools)["node_detail"].Call(ctx, json.RawMessage(`{"id":`+strconv.FormatInt(f.other, 10)+`}`), nil)
		if err != nil || !res.IsError || strings.Contains(res.Flatten(), "registry-fixture-concurrent-") {
			t.Fatal("unrelated task data escaped the per-run exploration store")
		}
	}
}

func TestTaskToolRegistryBindingsAndTasklessWarnings(t *testing.T) {
	pg := taskToolRegistryPG(t)
	task, store, _ := taskToolRegistryTask(t, pg, "binding")
	ctx := taskToolRegistryContext(task, store)
	for _, update := range []string{`enabled=false,agents='["worker"]'`, `enabled=true,agents='["planner"]'`} {
		if _, err := pg.Exec(`UPDATE tools SET ` + update + ` WHERE key='node_detail'`); err != nil {
			t.Fatal(err)
		}
		tools, def, cleanup := agent.AugmentTools(ctx, "worker", agent.NewToolSet(store, "fixture").WorkerTools())
		cleanup()
		if def.AssemblyError != nil || names(tools)["node_detail"] != nil {
			t.Fatal("optional registry ignored a disabled or absent role grant", def.AssemblyError)
		}
	}
	if _, err := pg.Exec(`UPDATE tools SET enabled=true,agents='["worker"]' WHERE key='node_detail'`); err != nil {
		t.Fatal(err)
	}
	var calls int
	chat := agent.NewChatAgent(retestProvider{complete: func(callCtx context.Context, req llm.CompletionRequest) (llm.Message, string, llm.Usage, error) {
		calls++
		if tool, _, err := agent.TaskDomainTool(callCtx, "worker", "node_detail"); err != nil || tool != nil {
			t.Error("task-associated Chat inherited a native registry")
		}
		hasBash := false
		for _, tool := range req.Tools {
			if tool.Name == "node_detail" {
				t.Error("taskless Chat advertised an unavailable graph reader")
			}
			hasBash = hasBash || tool.Name == "Bash"
		}
		if !hasBash || !strings.Contains(strings.Join(req.System, "\n"), "node_detail") {
			t.Error("taskless omission broke normal Chat or hid its assembly warning")
		}
		return llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{llm.TextBlock("No task reader was executed.")}}, "end_turn", llm.Usage{}, nil
	}}, "fixture", t.TempDir(), nil, 0)
	chat.SetNonStreaming(func() bool { return true })
	if _, err := chat.Chat(ctx, "worker", "task-associated-fixture", "Explain without native context.", 2, 0, false, nil); err != nil || calls != 1 {
		t.Fatal("normal Chat failed rather than omitting an unsupported task reader", err)
	}
}

func TestTaskToolRegistryRelatedSourcesStayReadOnly(t *testing.T) {
	pg := taskToolRegistryPG(t)
	source, sourceStore, node := taskToolRegistryTask(t, pg, "related-source")
	child, childStore, _ := taskToolRegistryTask(t, pg, "related-child", source.ID)
	ctx := taskToolRegistryContext(child, childStore)
	tools, def, cleanup := agent.AugmentTools(ctx, "worker", agent.NewToolSet(childStore, "fixture").WorkerTools())
	cleanup()
	if def.AssemblyError != nil {
		t.Fatal(def.AssemblyError)
	}
	res, err := names(tools)["node_detail"].Call(ctx, json.RawMessage(`{"id":`+strconv.FormatInt(node, 10)+`}`), nil)
	if err != nil || res.IsError || !strings.Contains(res.Flatten(), "registry-fixture-related-source") || !strings.Contains(res.Flatten(), `"inherited":true`) {
		t.Fatal("directly related source provenance was lost", err, res.Flatten())
	}
	if _, managed, err := agent.TaskDomainTool(ctx, "worker", "record_fact"); managed || err != nil {
		t.Fatal("read-context repair introduced an optional write capability")
	}
	if original, err := sourceStore.GetNode(node); err != nil || original == nil || !strings.Contains(string(original.Payload), "registry-fixture-related-source") {
		t.Fatal("related source record changed during a read", err)
	}
}
