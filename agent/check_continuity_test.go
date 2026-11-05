package agent

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/Autumn-27/artex/db"
)

func TestExecutionCheckContinuationToolKeepsOriginalSession(t *testing.T) {
	ts := factTestRig(t)
	ts.worker = "planner"
	missing, err := ts.addIntent().Call(t.Context(), json.RawMessage(`{"summary":"new wording without identity"}`), nil)
	if err != nil || !missing.IsError {
		t.Fatal("planner bypassed check identity", missing, err)
	}
	raw := json.RawMessage(`{"summary":"fixture","check_identity":{"resource":"fixture/object","operation":"read","identity_ref":"account-A","question":"fixture-question"}}`)
	res, err := ts.addIntent().Call(t.Context(), raw, nil)
	if err != nil || res.IsError {
		t.Fatal(res, err)
	}
	var dispatched struct {
		ID    int64  `json:"intent_id"`
		State string `json:"state"`
	}
	if err := json.Unmarshal([]byte(res.Flatten()), &dispatched); err != nil || dispatched.ID == 0 || dispatched.State != "open" {
		t.Fatal(res.Flatten(), err)
	}
	id := dispatched.ID
	session := WorkerSessionID(ts.ts.ID(), id)
	if ok, err := ts.ts.ClaimIntent(id, "fixture"); err != nil || !ok {
		t.Fatal(ok, err)
	}
	token, err := ts.ts.BindIntentRun(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	if err := ts.ts.SetIntentOutcome(id, "exhausted", "turn_budget", "fixture", token); err != nil {
		t.Fatal(err)
	}
	input, _ := json.Marshal(map[string]any{"continue_intent_id": id, "continuation_note": "reuse saved fixture; finish next step"})
	res, err = ts.addIntent().Call(t.Context(), input, nil)
	if err != nil || res.IsError {
		t.Fatal(res, err)
	}
	if err := json.Unmarshal([]byte(res.Flatten()), &dispatched); err != nil || dispatched.ID != id || WorkerSessionID(ts.ts.ID(), dispatched.ID) != session {
		t.Fatal("continuation forked transcript", res.Flatten(), err)
	}
	node, err := ts.ts.GetNode(id)
	if err != nil {
		t.Fatal(err)
	}
	context, requires := workerContinuationContext(node)
	if !strings.Contains(context, "reuse saved fixture") || requires {
		t.Fatal("wrong no-tool checkpoint", context, requires)
	}
	// A missing transcript cannot silently restart a check with prior tool calls.
	payload, _ := json.Marshal(map[string]any{"execution_checkpoint": db.ExecutionCheckpoint{ToolCalls: 1, Pending: map[string]string{"u": "fixture"}}})
	context, requires = workerContinuationContext(&db.Node{Payload: payload})
	if !requires || !strings.Contains(context, "pending") {
		t.Fatal("unknown tool result lost", context, requires)
	}
	payload, _ = json.Marshal(map[string]any{"execution_history_required": true, "execution_checkpoint": db.ExecutionCheckpoint{ToolCalls: 0}})
	_, requires = workerContinuationContext(&db.Node{Payload: payload})
	if !requires {
		t.Fatal("later zero-tool failure erased history requirement")
	}
	// Duplicate check creation returns the same blocked state, never an open clone.
	if err := ts.ts.SetIntentState(id, "blocked"); err != nil {
		t.Fatal(err)
	}
	res, err = ts.addIntent().Call(t.Context(), raw, nil)
	if err != nil || res.IsError {
		t.Fatal(res, err)
	}
	if err := json.Unmarshal([]byte(res.Flatten()), &dispatched); err != nil || dispatched.ID != id || dispatched.State != "blocked" {
		t.Fatal("blocked reuse reported queued", res.Flatten(), err)
	}
}
