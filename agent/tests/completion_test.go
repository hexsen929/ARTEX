//go:build ignore

package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"iter"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/Autumn-27/artex/db"
	"github.com/Autumn-27/norma/harness"
	"github.com/Autumn-27/norma/llm"
	"github.com/Autumn-27/norma/tool"
)

type completionReviewProvider struct {
	stream func(context.Context, llm.CompletionRequest, func(llm.StreamEvent, error) bool)
}

func (p completionReviewProvider) Stream(ctx context.Context, req llm.CompletionRequest) iter.Seq2[llm.StreamEvent, error] {
	return func(yield func(llm.StreamEvent, error) bool) { p.stream(ctx, req, yield) }
}

func (p completionReviewProvider) Complete(ctx context.Context, req llm.CompletionRequest) (llm.Message, string, llm.Usage, error) {
	acc := llm.NewAccumulator()
	for event, err := range p.Stream(ctx, req) {
		if err != nil {
			return llm.Message{}, "", llm.Usage{}, err
		}
		acc.Add(event)
	}
	return acc.Message(), acc.StopReason, acc.Usage, nil
}

func completionReviewEvents(yield func(llm.StreamEvent, error) bool, events ...llm.StreamEvent) {
	for _, event := range events {
		if !yield(event, nil) {
			return
		}
	}
}

func completionReviewFixture() json.RawMessage {
	return json.RawMessage(`{"goals_met":true,"eligible":true,"remaining_work":[],"data":{"tasks":[{"id":1,"goal":"original goal","description":"original description"}],"nodes":[],"edges":[],"constraints":[]}}`)
}

func TestReviewCompletionGoalGapsRequireSnapshotAuthority(t *testing.T) {
	snapshot := json.RawMessage(`{"goals_met":true,"eligible":true,"remaining_work":[],"data":{"tasks":[{"id":1,"goal":"obtain flag","description":"saved work"}],"nodes":[{"id":4,"exploration_id":7,"kind":"goal","state":"met","content_version":2,"payload":{"text":"obtain flag"}},{"id":8,"exploration_id":99,"kind":"goal","state":"met","content_version":2,"payload":{"text":"foreign goal"}}],"edges":[],"constraints":[]}}`)
	for _, tc := range []struct {
		name, input string
		valid       bool
	}{
		{"local", `{"complete":false,"reason":"flag absent","goal_gaps":[{"goal_id":4,"expected_version":2,"reason":"no saved flag"}]}`, true},
		{"foreign", `{"complete":false,"reason":"flag absent","goal_gaps":[{"goal_id":8,"expected_version":2,"reason":"foreign"}]}`, false},
		{"stale", `{"complete":false,"reason":"flag absent","goal_gaps":[{"goal_id":4,"expected_version":1,"reason":"old version"}]}`, false},
		{"blank", `{"complete":false,"reason":"flag absent","goal_gaps":[{"goal_id":4,"expected_version":2,"reason":" "}]}`, false},
		{"contradictory", `{"complete":true,"reason":"all met","goal_gaps":[{"goal_id":4,"expected_version":2,"reason":"no flag"}]}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			turns := 0
			provider := completionReviewProvider{stream: func(_ context.Context, _ llm.CompletionRequest, yield func(llm.StreamEvent, error) bool) {
				turns++
				if turns == 1 {
					completionReviewEvents(yield, llm.StreamEvent{Type: llm.SEToolUseStart, ToolID: "v", ToolName: "completion_verdict"}, llm.StreamEvent{Type: llm.SEToolInputJSON, Text: tc.input}, llm.StreamEvent{Type: llm.SEMessageDelta, StopReason: "tool_use"})
					return
				}
				completionReviewEvents(yield, llm.StreamEvent{Type: llm.SETextDelta, Text: "finished"}, llm.StreamEvent{Type: llm.SEMessageDelta, StopReason: "end_turn"})
			}}
			out, err := NewPlanner(provider, "", "", nil, 0, 0).ReviewCompletionVerdict(t.Context(), 1, new(db.DB).Exploration(7), snapshot, nil)
			if err != nil || out.Complete || (len(out.GoalGaps) == 1) != tc.valid {
				t.Fatalf("invalid verdict: %+v err=%v", out, err)
			}
		})
	}
}

func TestReviewCompletionUsesReadOnlyToolsAndFreshController(t *testing.T) {
	for _, nonStreaming := range []bool{false, true} {
		name := "streaming"
		if nonStreaming {
			name = "non_streaming"
		}
		t.Run(name, func(t *testing.T) {
			parentControl := harness.NewSettlementController()
			parentControl.Request()
			ctx := harness.WithSettlementController(context.Background(), parentControl)
			turns := 0
			provider := completionReviewProvider{stream: func(ctx context.Context, req llm.CompletionRequest, yield func(llm.StreamEvent, error) bool) {
				turns++
				var names []string
				for _, schema := range req.Tools {
					names = append(names, schema.Name)
				}
				slices.Sort(names)
				want := []string{"completion_verdict", "get_worker_output", "get_worker_trace", "list_facts", "list_findings", "list_goals", "node_detail"}
				if !slices.Equal(names, want) {
					t.Errorf("review tool registry=%v, want %v", names, want)
				}
				for _, forbidden := range []string{"Bash", "Monitor", "ExecuteExtraTool", "Agent", "Write", "record_fact", "prove_goal", "create_intent"} {
					if tool.CheckExecution(ctx, forbidden) == nil {
						t.Errorf("review dispatch guard permits %s", forbidden)
					}
				}
				if err := tool.CheckExecution(ctx, "completion_verdict"); err != nil {
					t.Errorf("verdict blocked: %v", err)
				}
				if turns == 1 {
					completionReviewEvents(yield,
						llm.StreamEvent{Type: llm.SEToolUseStart, ToolID: "forbidden", ToolName: "Monitor"},
						llm.StreamEvent{Type: llm.SEToolInputJSON, Text: `{}`},
						llm.StreamEvent{Type: llm.SEToolUseStart, ToolID: "verdict", ToolName: "completion_verdict"},
						llm.StreamEvent{Type: llm.SEToolInputJSON, Text: `{"complete":true,"reason":"saved evidence is complete"}`},
						llm.StreamEvent{Type: llm.SEMessageDelta, StopReason: "tool_use"})
					return
				}
				completionReviewEvents(yield, llm.StreamEvent{Type: llm.SETextDelta, Text: "review finished"}, llm.StreamEvent{Type: llm.SEMessageDelta, StopReason: "end_turn"})
			}}
			planner := NewPlanner(provider, "", "", nil, 0, 0)
			planner.SetNonStreaming(func() bool { return nonStreaming })
			var activities []db.Activity
			accepted, reason, err := planner.ReviewCompletion(ctx, 1, new(db.DB).Exploration(7), completionReviewFixture(), func(a db.Activity) { activities = append(activities, a) })
			if err != nil || !accepted || reason != "saved evidence is complete" || turns != 2 {
				t.Fatalf("review accepted=%v reason=%q err=%v turns=%d", accepted, reason, err, turns)
			}
			blocked := false
			for _, a := range activities {
				if a.Kind == "tool_result" && a.ToolUseID == "forbidden" {
					blocked = a.IsError
				}
			}
			if !blocked {
				t.Fatal("unexpected Monitor request did not produce a denied tool result")
			}
		})
	}
}

func TestReviewCompletionRequiresExplicitVerdict(t *testing.T) {
	for _, tc := range []struct{ name, verdict string }{
		{name: "no_verdict"},
		{name: "negative_verdict", verdict: `{"complete":false,"reason":"necessary work remains"}`},
		{name: "malformed_verdict", verdict: `{"complete":true,"reason":""}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			turns := 0
			provider := completionReviewProvider{stream: func(_ context.Context, _ llm.CompletionRequest, yield func(llm.StreamEvent, error) bool) {
				turns++
				if tc.verdict != "" && turns == 1 {
					completionReviewEvents(yield, llm.StreamEvent{Type: llm.SEToolUseStart, ToolID: "verdict", ToolName: "completion_verdict"}, llm.StreamEvent{Type: llm.SEToolInputJSON, Text: tc.verdict}, llm.StreamEvent{Type: llm.SEMessageDelta, StopReason: "tool_use"})
					return
				}
				completionReviewEvents(yield, llm.StreamEvent{Type: llm.SETextDelta, Text: "finished"}, llm.StreamEvent{Type: llm.SEMessageDelta, StopReason: "end_turn"})
			}}
			accepted, reason, err := NewPlanner(provider, "", "", nil, 0, 0).ReviewCompletion(context.Background(), 1, new(db.DB).Exploration(7), completionReviewFixture(), nil)
			if accepted || reason == "" || err != nil {
				t.Fatalf("review accepted=%v reason=%q err=%v", accepted, reason, err)
			}
		})
	}
}

func TestReviewCompletionCancellationInvalidatesEarlierVerdict(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	turns := 0
	provider := completionReviewProvider{stream: func(ctx context.Context, _ llm.CompletionRequest, yield func(llm.StreamEvent, error) bool) {
		turns++
		if turns == 1 {
			completionReviewEvents(yield, llm.StreamEvent{Type: llm.SEToolUseStart, ToolID: "verdict", ToolName: "completion_verdict"}, llm.StreamEvent{Type: llm.SEToolInputJSON, Text: `{"complete":true,"reason":"provisional"}`}, llm.StreamEvent{Type: llm.SEMessageDelta, StopReason: "tool_use"})
			return
		}
		cancel()
		<-ctx.Done()
		yield(llm.StreamEvent{}, ctx.Err())
	}}
	accepted, _, err := NewPlanner(provider, "", "", nil, 0, 0).ReviewCompletion(ctx, 1, new(db.DB).Exploration(7), completionReviewFixture(), nil)
	if accepted || !errors.Is(err, context.Canceled) || turns != 2 {
		t.Fatalf("canceled review accepted=%v err=%v turns=%d", accepted, err, turns)
	}
}

func TestBuildReviewInputPreservesCriticalDataAndBoundsEvidence(t *testing.T) {
	var nodes, edges, work []map[string]any
	for id := 1; id <= 150; id++ {
		nodes = append(nodes, map[string]any{"id": id, "exploration_id": 7, "kind": "goal", "state": "met", "payload": map[string]any{"text": fmt.Sprintf("完整目标 %d，不得截断", id)}})
		edges = append(edges, map[string]any{"src_id": 5000, "dst_id": id, "rel": "proves"})
	}
	for id := 5000; id < 5100; id++ {
		nodes = append(nodes, map[string]any{"id": id, "exploration_id": 7, "kind": "fact", "state": "done", "payload": map[string]any{"text": strings.Repeat("证据详情", 5000)}})
	}
	nodes = append(nodes, map[string]any{"id": 9000, "exploration_id": 99, "kind": "goal", "state": "open", "payload": map[string]any{"text": "source task goal"}})
	for id := 200; id < 220; id++ {
		work = append(work, map[string]any{"intent_id": id, "state": "blocked", "summary": fmt.Sprintf("完整剩余检查 %d", id), "kind": "duplicate", "reason": "已引用同范围成果", "reference_id": 5001})
	}
	constraints := []map[string]any{{"exploration_id": 7, "text": "本任务操作边界：只读"}, {"exploration_id": 99, "text": "关联任务操作边界：不执行写入"}}
	snapshot, err := json.Marshal(map[string]any{
		"goals_met": true, "eligible": true, "remaining_work": work,
		"data": map[string]any{"tasks": []map[string]any{{"id": 1, "goal": "原始任务目标", "description": "原始完整描述"}}, "nodes": nodes, "edges": edges, "constraints": constraints, "assets": []map[string]any{{"extra": strings.Repeat("DO_NOT_SEND_ASSET_BLOB", 100000)}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	before := bytes.Clone(snapshot)
	input, err := buildReviewInput(snapshot, 1, 7, completionReviewByteLimit)
	if err != nil {
		t.Fatal(err)
	}
	if len(input) > completionReviewByteLimit || bytes.Contains(input, []byte("DO_NOT_SEND_ASSET_BLOB")) {
		t.Fatalf("unbounded review input: %d bytes", len(input))
	}
	if !bytes.Equal(snapshot, before) {
		t.Fatal("builder mutated the durable snapshot")
	}
	var got completionReviewInput
	if err := json.Unmarshal(input, &got); err != nil {
		t.Fatal(err)
	}
	if got.Task.Goal != "原始任务目标" || got.Task.Description != "原始完整描述" || len(got.Goals) != 150 || len(got.EvidenceReferences) != 170 {
		t.Fatalf("critical inputs lost: goals=%d refs=%d task=%+v", len(got.Goals), len(got.EvidenceReferences), got.Task)
	}
	for key, raw := range map[string]json.RawMessage{"remaining_work": got.RemainingWork, "constraints": got.Constraints} {
		var want any
		if key == "remaining_work" {
			want = work
		} else {
			want = constraints
		}
		encoded, _ := json.Marshal(want)
		var wantValue, actualValue any
		_ = json.Unmarshal(encoded, &wantValue)
		_ = json.Unmarshal(raw, &actualValue)
		if !reflect.DeepEqual(actualValue, wantValue) {
			t.Fatalf("%s was truncated or changed", key)
		}
	}
	if len(got.EvidenceSummaries) > 12 || got.EvidenceOmitted != 100-len(got.EvidenceSummaries) || !strings.Contains(got.Notice, "采样") {
		t.Fatalf("sampling is not explicit: %+v", got)
	}
}

func TestReviewCompletionOversizedCriticalInputStopsBeforeModel(t *testing.T) {
	for _, field := range []string{"goal", "constraints", "remaining_work"} {
		t.Run(field, func(t *testing.T) {
			var snapshot map[string]any
			_ = json.Unmarshal(completionReviewFixture(), &snapshot)
			data := snapshot["data"].(map[string]any)
			long := strings.Repeat("不可截断", 3000)
			switch field {
			case "goal":
				data["tasks"].([]any)[0].(map[string]any)["goal"] = long
			case "constraints":
				data["constraints"] = []map[string]any{{"exploration_id": 7, "text": long}}
			case "remaining_work":
				snapshot["remaining_work"] = []map[string]any{{"intent_id": 2, "reason": long}}
			}
			encoded, _ := json.Marshal(snapshot)
			calls := 0
			provider := completionReviewProvider{stream: func(context.Context, llm.CompletionRequest, func(llm.StreamEvent, error) bool) { calls++ }}
			planner := NewPlanner(provider, "", "", nil, 8192, 0)
			if planner.completionReviewLimit() != 4096 {
				t.Fatalf("small window budget=%d", planner.completionReviewLimit())
			}
			accepted, _, err := planner.ReviewCompletion(context.Background(), 1, new(db.DB).Exploration(7), encoded, nil)
			if accepted || err == nil || calls != 0 || !strings.Contains(err.Error(), "未被截断") {
				t.Fatalf("oversized critical input reached model: accepted=%v calls=%d err=%v", accepted, calls, err)
			}
		})
	}
}

func TestBuildReviewInputRejectsMissingTask(t *testing.T) {
	if _, err := buildReviewInput(completionReviewFixture(), 999, 7, completionReviewByteLimit); err == nil {
		t.Fatal("missing original task accepted")
	}
}
