package sidequestion

import (
	"context"
	"encoding/json"
	"iter"
	"strings"
	"testing"

	"github.com/Autumn-27/artex/memcontext"
	"github.com/Autumn-27/norma/harness"
	"github.com/Autumn-27/norma/llm"
)

type memoryCheckpointProvider struct{}

func (memoryCheckpointProvider) Complete(context.Context, llm.CompletionRequest) (llm.Message, string, llm.Usage, error) {
	return llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{llm.TextBlock("business answer")}}, "stop", llm.Usage{}, nil
}
func (memoryCheckpointProvider) Stream(context.Context, llm.CompletionRequest) iter.Seq2[llm.StreamEvent, error] {
	return func(yield func(llm.StreamEvent, error) bool) { yield(llm.StreamEvent{Type: llm.SEMessageStop}, nil) }
}

func TestMemoryTemporaryBlockDoesNotEnterSideQuestionCheckpoint(t *testing.T) {
	const canary = "transient-memory-checkpoint-canary"
	var snapshots []Snapshot
	ctx := WithPublisher(t.Context(), func(s Snapshot) { snapshots = append(snapshots, s) })
	provider := Bind(memoryCheckpointProvider{}, Model{Model: "fixture"})
	ctx, deps := Attach(ctx, Parent{TaskID: 1, ExplorationID: 2, IntentID: 3}, harness.QueryDeps{}, provider)
	memoryCtx, release := memcontext.WithRecall(ctx, []memcontext.Reference{{ID: 4, Version: 5}}, func(context.Context) error { return nil })
	defer release()
	block := memcontext.FormatBlock(canary, memcontext.References(memoryCtx))
	req := llm.CompletionRequest{System: []string{"host rules", block}, Messages: []llm.Message{llm.UserText("business question")}}
	msg, _, _, err := deps.CallModelSync(memoryCtx, req)
	if err != nil {
		t.Fatal(err)
	}
	Finish(ctx, append(req.Messages, msg))
	if len(snapshots) < 3 {
		t.Fatalf("missing before/after/final checkpoints: %d", len(snapshots))
	}
	for _, snapshot := range snapshots {
		data, err := json.Marshal(snapshot)
		if err != nil || strings.Contains(string(data), canary) || len(snapshot.Request.System) != 1 || snapshot.Request.System[0] != "host rules" {
			t.Fatalf("transient memory entered persisted checkpoint: %s error=%v", data, err)
		}
	}
	if len(req.System) != 2 || req.System[1] != block {
		t.Fatal("checkpoint sanitization mutated actual request")
	}
}
