package server

import (
	"context"
	"encoding/json"
	"io"
	"iter"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Autumn-27/artex/agent"
	"github.com/Autumn-27/norma/llm"
	"github.com/Autumn-27/norma/tool"
	"github.com/Autumn-27/norma/transcript"
)

type recoveryEngineProvider struct {
	calls   atomic.Int64
	resumed atomic.Bool
}

func (p *recoveryEngineProvider) Complete(context.Context, llm.CompletionRequest) (llm.Message, string, llm.Usage, error) {
	return llm.Message{}, "", llm.Usage{}, io.ErrUnexpectedEOF
}
func (p *recoveryEngineProvider) Stream(ctx context.Context, req llm.CompletionRequest) iter.Seq2[llm.StreamEvent, error] {
	return func(yield func(llm.StreamEvent, error) bool) {
		call := p.calls.Add(1)
		if call == 2 {
			// Reproduce Norma's unfinished tool input at a mid-stream task-router EOF.
			if !yield(llm.StreamEvent{Type: llm.SEToolUseStart, ToolID: "partial", ToolName: "Bash"}, nil) {
				return
			}
			if !yield(llm.StreamEvent{Type: llm.SEToolInputJSON, Text: `{"command":`}, nil) {
				return
			}
			yield(llm.StreamEvent{}, &taskLLMError{taskID: "fixture", cause: io.ErrUnexpectedEOF})
			return
		}
		events := []llm.StreamEvent{{Type: llm.SEToolUseStart, ToolID: "read-once", ToolName: "RecoveryProbe"}, {Type: llm.SEToolInputJSON, Text: `{}`}, {Type: llm.SEMessageDelta, StopReason: "tool_use"}}
		if call >= 3 {
			for _, msg := range req.Messages {
				for _, block := range msg.Content {
					if block.Type == llm.BlockToolResult && block.ToolUseID == "read-once" {
						p.resumed.Store(true)
					}
				}
			}
			events = []llm.StreamEvent{{Type: llm.SETextDelta, Text: "Recovered from saved fixture result."}, {Type: llm.SEMessageDelta, StopReason: "end_turn"}}
		}
		for _, e := range events {
			if !yield(e, nil) {
				return
			}
		}
	}
}

func TestAutomaticRecoveryEngineResumesSameTranscriptWithoutRepeatingTool(t *testing.T) {
	_ = taskToolRegistryPG(t)
	m := proxyTestManager(t)
	task, err := m.CreateTask("automatic recovery engine", "fixture", nil, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = m.DeleteTask(task.ID, DeleteTaskOptions{}) })
	id, err := task.Store.AddIntent(map[string]any{"summary": "fixture"}, 5, nil, "fixture")
	if err != nil {
		t.Fatal(err)
	}
	if ok, err := task.Store.ClaimIntent(id, "fixture"); err != nil || !ok {
		t.Fatal(ok, err)
	}
	intent, err := task.Store.GetNode(id)
	if err != nil {
		t.Fatal(err)
	}
	var executions atomic.Int64
	probe := tool.Build(tool.Spec{Name: "RecoveryProbe", Schema: map[string]any{"type": "object"}, Run: func(context.Context, json.RawMessage, *tool.ToolContext) (tool.Result, error) {
		executions.Add(1)
		return tool.Text("fixture-once-result"), nil
	}})
	provider := &recoveryEngineProvider{}
	worker := agent.NewWorker(provider, "fixture", t.TempDir(), transcript.NewStore(t.TempDir()), 0, 4, probe)
	engine := NewEngine(m)
	t.Cleanup(func() { engine.StopTask(task.ID) })
	engine.runIntent(t.Context(), task, "fixture", worker, intent, "", "")
	node, err := task.Store.GetNode(id)
	if err != nil {
		t.Fatal(err)
	}
	cp := taskNodeDTO(node).ExecutionCheckpoint
	if node.State != "blocked" || provider.calls.Load() != 2 || executions.Load() != 1 || cp == nil || cp.ToolCalls != 1 || cp.ToolResults != 1 || len(cp.Pending) != 0 {
		t.Fatalf("initial stream corrupted checkpoint: state=%s calls=%d executions=%d cp=%+v", node.State, provider.calls.Load(), executions.Load(), cp)
	}
	engine.reconcileWorkerOutcomesAt(t.Context(), task, time.Now().Add(time.Minute))
	node, err = task.Store.GetNode(id)
	if err != nil || node.State != "open" {
		t.Fatalf("automatic recovery did not queue original intent: %v %+v", err, node)
	}
	if !engine.runWorkerStep(t.Context(), task, "fixture", worker) {
		t.Fatal("recovered intent was not claimed")
	}
	node, err = task.Store.GetNode(id)
	if err != nil {
		t.Fatal(err)
	}
	if node.State != "done" || executions.Load() != 1 || provider.calls.Load() != 3 || !provider.resumed.Load() {
		t.Fatalf("recovery restarted work: state=%s executions=%d calls=%d transcript=%v", node.State, executions.Load(), provider.calls.Load(), provider.resumed.Load())
	}
	if taskNodeDTO(node).ExecutionAttempt != 2 {
		t.Fatal("original intent did not acquire a new execution generation")
	}
}
