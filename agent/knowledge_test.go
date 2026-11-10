package agent

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/Autumn-27/artex/knowledge"
)

func TestKnowledgeToolUsesHostTaskIdentity(t *testing.T) {
	old := KnowledgeSearch
	t.Cleanup(func() { KnowledgeSearch = old })
	seen := int64(-1)
	KnowledgeSearch = func(_ context.Context, task int64, q string, k int) (knowledge.Result, error) {
		seen = task
		return knowledge.Result{Mode: "keyword", Hits: []knowledge.Hit{}}, nil
	}
	tools := NewToolSet(nil, "fixture")
	tool := tools.searchKnowledge()
	ctx := WithRunInfo(t.Context(), RunInfo{TaskID: 12})
	res, err := tool.Call(ctx, json.RawMessage(`{"query":"fixture","task_id":99}`), nil)
	if err != nil || res.IsError || seen != 12 {
		t.Fatal("model supplied task identity used", seen, err)
	}
	tools.SetTaskID(18)
	res, err = tool.Call(ctx, json.RawMessage(`{"query":"fixture"}`), nil)
	if err != nil || !res.IsError {
		t.Fatal("conflicting host contexts allowed")
	}
	tools.SetTaskID(0)
	res, err = tool.Call(t.Context(), json.RawMessage(`{"query":"fixture","task_id":99}`), nil)
	if err != nil || res.IsError || seen != 0 {
		t.Fatal("taskless tool expanded scope", seen, err)
	}
}
