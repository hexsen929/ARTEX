package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"sync"
	"testing"

	"github.com/Autumn-27/artex/db"
	actool "github.com/Autumn-27/norma/tool"
)

func taskRegistryFixture(role string) (context.Context, *ToolSet, RunInfo) {
	// Metadata-only handles deliberately have no database. Negative invocation
	// tests therefore prove rejection occurs before any exploration read.
	ts := NewToolSet((&db.DB{}).Exploration(901), "fixture-worker")
	ts.SetTaskID(117)
	info := RunInfo{TaskID: 117, ExplorationID: 901}
	if role == "worker" {
		info.IntentID = 43
	}
	ctx := WithRunInfo(context.Background(), info)
	return WithTaskToolSet(ctx, role, ts), ts, info
}

func TestTaskToolRegistryResolvesOnlyExistingNativeReaders(t *testing.T) {
	for _, role := range []string{"worker", "planner", "mainagent"} {
		ctx, _, _ := taskRegistryFixture(role)
		for _, name := range []string{"graph_overview", "list_findings", "list_facts", "node_detail", "list_goals", "get_worker_output", "get_worker_trace", "list_worker_traces", "search_all_worker_traces", "list_assets", "list_companies"} {
			tool, managed, err := TaskDomainTool(ctx, role, name)
			if err != nil || !managed || tool == nil || tool.Name() != name || !tool.IsReadOnly(nil) {
				t.Fatalf("%s/%s did not resolve its native reader: managed=%v err=%v", role, name, managed, err)
			}
		}
		for _, name := range []string{"add_intent", "record_fact", "set_goals", "expand_digest", "Bash", "unknown_fixture"} {
			tool, managed, err := TaskDomainTool(ctx, role, name)
			if err != nil || managed || tool != nil {
				t.Fatalf("%s gained a non-reader %s", role, name)
			}
		}
	}
}

func TestTaskToolRegistryAssetReadersRetainExactNativeRun(t *testing.T) {
	ctx, ts, info := taskRegistryFixture("worker")
	for _, name := range []string{"list_assets", "list_companies"} {
		tool, managed, err := TaskDomainTool(ctx, "worker", name)
		if err != nil || !managed || tool == nil || !tool.IsReadOnly(nil) {
			t.Fatalf("native asset reader %s not resolved: managed=%v err=%v", name, managed, err)
		}
		newRun := WithTaskToolSet(WithRunInfo(context.Background(), info), "worker", ts)
		result, err := tool.Call(newRun, json.RawMessage(`{}`), nil)
		if err != nil || !result.IsError {
			t.Fatalf("old native asset reader %s was accepted in a new run", name)
		}
		changed := info
		changed.TaskID++
		result, err = tool.Call(WithRunInfo(ctx, changed), json.RawMessage(`{}`), nil)
		if err != nil || !result.IsError {
			t.Fatalf("native asset reader %s accepted a different task", name)
		}
	}
}

func TestTaskDomainAssetFactoryHasNoExplorationCapabilities(t *testing.T) {
	ts := NewToolSet(nil, "associated-chat")
	ts.SetTaskID(117)
	tools := ts.AssetDomainTools()
	var names []string
	for _, tool := range tools {
		names = append(names, tool.Name())
		if tool.Name() != "insert_assets" && !tool.IsReadOnly(nil) {
			t.Fatalf("asset factory returned a non-reader %s", tool.Name())
		}
	}
	if !reflect.DeepEqual(names, []string{"insert_assets", "list_assets", "list_companies"}) || ts.ts != nil {
		t.Fatal("asset factory granted exploration capabilities")
	}
}

func TestTaskToolRegistryRejectsChangedOwnershipAndOldRunToken(t *testing.T) {
	ctx, ts, info := taskRegistryFixture("worker")
	tool, _, err := TaskDomainTool(ctx, "worker", "node_detail")
	if err != nil || tool == nil {
		t.Fatal(err)
	}
	for _, changed := range []RunInfo{
		{}, {TaskID: 118, ExplorationID: info.ExplorationID, IntentID: info.IntentID},
		{TaskID: info.TaskID, ExplorationID: 902, IntentID: info.IntentID},
		{TaskID: info.TaskID, ExplorationID: info.ExplorationID, IntentID: 44},
		{TaskID: info.TaskID, ExplorationID: info.ExplorationID, IntentID: info.IntentID, SessionID: "associated-chat"},
	} {
		changedCtx := WithRunInfo(ctx, changed)
		if _, _, err := TaskDomainTool(changedCtx, "worker", "node_detail"); err == nil {
			t.Fatal("changed attribution resolved an old run's reader")
		}
		res, callErr := tool.Call(changedCtx, json.RawMessage(`{"id":1}`), nil)
		if callErr != nil || !res.IsError {
			t.Fatal("changed attribution reached the exploration store")
		}
	}
	if _, _, err := TaskDomainTool(ctx, "planner", "node_detail"); err == nil {
		t.Fatal("different assembly role reused the Worker registry")
	}
	// All numeric IDs are identical, but a new native run has a new capability
	// token. This also catches a tool accidentally retained across run restarts.
	newRun := WithTaskToolSet(WithRunInfo(context.Background(), info), "worker", ts)
	res, err := tool.Call(newRun, json.RawMessage(`{"id":1}`), nil)
	if err != nil || !res.IsError {
		t.Fatal("old run's tool was callable in a new run with identical IDs")
	}
	ctx, cancel := context.WithCancel(ctx)
	cancel()
	res, err = tool.Call(ctx, json.RawMessage(`{"id":1}`), nil)
	if err != nil || !res.IsError {
		t.Fatal("cancelled read reached the exploration store")
	}
}

func TestTaskToolRegistryDoesNotTurnChatAttributionIntoAStore(t *testing.T) {
	ctx, ts, info := taskRegistryFixture("worker")
	for _, role := range []string{"worker", "auto", "pentest", "reporter", "goals", db.CredentialAgentKey, "asset_mapper"} {
		chatCtx := WithoutTaskToolSet(WithRunInfo(ctx, info))
		tool, managed, err := TaskDomainTool(chatCtx, role, "node_detail")
		if err != nil || !managed || tool != nil {
			t.Fatalf("task-associated Chat %s gained a native reader", role)
		}
		if !nativeTaskToolRole(role) {
			tool, _, err = TaskDomainTool(WithTaskToolSet(ctx, role, ts), role, "node_detail")
			if err != nil || tool != nil {
				t.Fatalf("restricted/non-native role %s published a native registry", role)
			}
		}
	}
	bad := NewToolSet((&db.DB{}).Exploration(info.ExplorationID+1), "fixture")
	bad.SetTaskID(info.TaskID)
	if _, _, err := TaskDomainTool(WithTaskToolSet(ctx, "worker", bad), "worker", "node_detail"); err == nil {
		t.Fatal("ToolSet exploration ID mismatch was accepted")
	}
}

func TestTaskToolRegistryDoesNotChangeWorkerDefaults(t *testing.T) {
	_, ts, _ := taskRegistryFixture("worker")
	names := func(tools []actool.CoreTool) []string {
		out := []string{}
		for _, tool := range tools {
			out = append(out, tool.Name())
		}
		return out
	}
	before := names(ts.WorkerTools())
	WithTaskToolSet(WithRunInfo(context.Background(), RunInfo{TaskID: 117, ExplorationID: 901, IntentID: 43}), "worker", ts)
	if !reflect.DeepEqual(before, names(ts.WorkerTools())) {
		t.Fatal("publishing optional readers changed the Worker base/default bindings")
	}
	for _, name := range before {
		if name == "node_detail" {
			t.Fatal("node_detail was automatically added to the default Worker base")
		}
	}
}

func TestTaskAssemblyWarningsAreBoundedAndRunLocal(t *testing.T) {
	oldAugment, oldResolve := ToolAugment, ToolResolve
	t.Cleanup(func() { ToolAugment, ToolResolve = oldAugment, oldResolve })
	ToolAugment = nil
	ToolResolve = func(ctx context.Context, _ string, base []actool.CoreTool) []actool.CoreTool {
		message := fmt.Sprintf("fixture task %d omitted a task reader", RunInfoFrom(ctx).TaskID)
		AddToolAssemblyWarning(ctx, message)
		AddToolAssemblyWarning(ctx, message)
		return base
	}
	var wg sync.WaitGroup
	for i := int64(1); i <= 12; i++ {
		wg.Add(1)
		go func(id int64) {
			defer wg.Done()
			ctx := WithRunInfo(context.Background(), RunInfo{TaskID: id})
			_, def, cleanup := AugmentTools(ctx, "auto", []actool.CoreTool{})
			cleanup()
			want := fmt.Sprintf("fixture task %d omitted a task reader", id)
			if def.AssemblyError != nil || len(def.AssemblyWarnings) != 1 || def.AssemblyWarnings[0] != want {
				t.Errorf("assembly warning crossed run boundaries: %+v", def)
			}
		}(i)
	}
	wg.Wait()
}
