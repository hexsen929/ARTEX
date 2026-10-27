package agent

import (
	"context"
	"errors"
	"iter"
	"strings"
	"testing"

	"github.com/Autumn-27/artex/db"
	"github.com/Autumn-27/norma/harness"
	"github.com/Autumn-27/norma/llm"
	actool "github.com/Autumn-27/norma/tool"
)

func TestSkillVisibilityOverrideDoesNotChangeAnotherExecutionRole(t *testing.T) {
	parent := WithRunInfo(t.Context(), RunInfo{TaskID: 17, IntentID: 23})
	ctx := WithSkillVisibilityAgent(parent, "worker", "worker.intranet")
	if SkillVisibilityAgent(ctx, "worker") != "worker.intranet" || SkillVisibilityAgent(ctx, "reporter") != "reporter" {
		t.Fatal("Skill selection changed the wrong execution role")
	}
	if SkillVisibilityAgent(parent, "worker") != "worker" || RunInfoFrom(ctx) != RunInfoFrom(parent) {
		t.Fatal("Skill selection mutated the parent or run attribution")
	}
	oldAugment, oldResolve := ToolAugment, ToolResolve
	t.Cleanup(func() { ToolAugment, ToolResolve = oldAugment, oldResolve })
	ToolAugment = func(ctx context.Context, executionKey string) ([]actool.CoreTool, DeferredInfo, func()) {
		if executionKey != "worker" || SkillVisibilityAgent(ctx, executionKey) != "worker.intranet" {
			t.Fatal("assembly lost the separate execution and Skill roles")
		}
		return nil, DeferredInfo{}, nil
	}
	resolved := false
	ToolResolve = func(ctx context.Context, executionKey string, tools []actool.CoreTool) []actool.CoreTool {
		resolved = true
		if executionKey != "worker" {
			t.Fatal("domain tool permissions switched to the prompt variant")
		}
		return tools
	}
	_, _, cleanup := AugmentTools(ctx, "worker", nil)
	cleanup()
	if !resolved {
		t.Fatal("ordinary domain tool resolution was skipped")
	}
}

func TestIntranetSkillMetadataFailureStopsWorkerBeforeModel(t *testing.T) {
	oldAugment, oldResolve := ToolAugment, ToolResolve
	t.Cleanup(func() { ToolAugment, ToolResolve = oldAugment, oldResolve })
	problem := errors.New("fixture Skill permissions unavailable")
	cleaned := false
	ToolAugment = func(ctx context.Context, executionKey string) ([]actool.CoreTool, DeferredInfo, func()) {
		if executionKey != "worker" || SkillVisibilityAgent(ctx, executionKey) != "worker.intranet" {
			t.Fatal("intranet Skill selection was not attached before assembly")
		}
		return nil, DeferredInfo{AssemblyError: problem}, func() { cleaned = true }
	}
	ToolResolve = func(context.Context, string, []actool.CoreTool) []actool.CoreTool {
		t.Fatal("failed authorization metadata must stop before further assembly")
		return nil
	}
	w := NewWorker(nil, "fixture", t.TempDir(), nil, 0, 0)
	w.SetIntranetResolver(func(int64) bool { return true })
	reason, writes, err := w.Execute(t.Context(), "fixture", 17, nil, nil, &db.Node{ID: 23}, nil, nil, nil, nil)
	if reason != harness.ReasonHookStopped || !errors.Is(err, problem) || writes.Total() != 0 || !cleaned {
		t.Fatalf("reason=%s writes=%+v err=%v cleaned=%v", reason, writes, err, cleaned)
	}
}

type skillSnapshotProvider struct {
	systems []string
	roles   []string
}

func (p *skillSnapshotProvider) Stream(ctx context.Context, req llm.CompletionRequest) iter.Seq2[llm.StreamEvent, error] {
	return func(yield func(llm.StreamEvent, error) bool) {
		p.systems = append(p.systems, strings.Join(req.System, "\n"))
		p.roles = append(p.roles, SkillVisibilityAgent(ctx, "worker"))
		if !yield(llm.StreamEvent{Type: llm.SETextDelta, Text: "fixture complete"}, nil) {
			return
		}
		yield(llm.StreamEvent{Type: llm.SEMessageDelta, StopReason: "end_turn"}, nil)
	}
}

func (p *skillSnapshotProvider) Complete(ctx context.Context, req llm.CompletionRequest) (llm.Message, string, llm.Usage, error) {
	acc := llm.NewAccumulator()
	for event, err := range p.Stream(ctx, req) {
		if err != nil {
			return llm.Message{}, "", llm.Usage{}, err
		}
		acc.Add(event)
	}
	return acc.Message(), acc.StopReason, acc.Usage, nil
}

func TestWorkerSkillAndPromptUseOnePhaseSnapshotPerRun(t *testing.T) {
	d := testDB(t)
	t.Cleanup(func() { _ = d.Close() })
	task, err := d.CreateTask("Skill phase snapshot fixture", "fixture", nil, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.DeleteTask(task.ID) })
	es := d.Exploration(task.ExplorationID)
	id, err := es.AddNode("intent", map[string]any{"summary": "fixture"}, 0, "running", "planner", nil)
	if err != nil {
		t.Fatal(err)
	}
	oldAugment, oldResolve, oldPrompt := ToolAugment, ToolResolve, PromptOverride
	t.Cleanup(func() { ToolAugment, ToolResolve, PromptOverride = oldAugment, oldResolve, oldPrompt })
	ToolResolve = nil
	PromptOverride = func(key string) (string, bool) {
		switch key {
		case "worker.intranet":
			return "INTRANET_SNAPSHOT={{.Intranet}}", true
		case "worker":
			return "EXTERNAL_SNAPSHOT={{.Intranet}}", true
		}
		return "", false
	}
	phase, reads := true, 0
	var assembled []string
	ToolAugment = func(ctx context.Context, executionKey string) ([]actool.CoreTool, DeferredInfo, func()) {
		if executionKey != "worker" || RunInfoFrom(ctx).TaskID != task.ID {
			t.Fatal("execution identity or task attribution changed")
		}
		assembled = append(assembled, SkillVisibilityAgent(ctx, executionKey))
		// Simulate a session/tunnel transition during assembly. It must not
		// change the prompt in the middle of the already prepared run.
		phase = !phase
		return nil, DeferredInfo{}, nil
	}
	provider := &skillSnapshotProvider{}
	w := NewWorker(provider, "fixture", t.TempDir(), nil, 0, 3)
	w.SetIntranetResolver(func(int64) bool { reads++; return phase })
	intent := &db.Node{ID: id, Payload: []byte(`{"summary":"fixture"}`)}
	for _, expected := range []struct{ role, marker string }{
		{"worker.intranet", "INTRANET_SNAPSHOT=true"},
		{"worker", "EXTERNAL_SNAPSHOT=false"},
	} {
		reads = 0
		start := len(provider.systems)
		// A stale inherited selection must be overwritten for the next run.
		ctx := WithSkillVisibilityAgent(t.Context(), "worker", "worker.intranet")
		if _, _, err := w.Execute(ctx, "fixture", task.ID, nil, es, intent, nil, nil, nil, nil); err != nil {
			t.Fatal(err)
		}
		if reads != 1 || assembled[len(assembled)-1] != expected.role || len(provider.systems) == start {
			t.Fatalf("phase reads=%d roles=%v model calls=%d", reads, assembled, len(provider.systems)-start)
		}
		for index := start; index < len(provider.systems); index++ {
			if provider.roles[index] != expected.role || !strings.Contains(provider.systems[index], expected.marker) {
				t.Fatal("prompt and Skill visibility described different phases")
			}
		}
	}
}
