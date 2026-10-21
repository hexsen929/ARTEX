//go:build ignore

package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/Autumn-27/artex/db"
	"github.com/Autumn-27/norma/llm"
)

func TestGoalReviewRequiresStructuredToolFreeVerdict(t *testing.T) {
	for _, tc := range []struct {
		name, output string
		tool         bool
		valid        bool
	}{
		{"outcome", `{"supported":true,"goal_type":"outcome","reason":"saved flag"}`, false, true},
		{"inspection", `{"supported":false,"goal_type":"inspection","reason":"scope incomplete"}`, false, true},
		{"missing_supported", `{"goal_type":"outcome","reason":"no result"}`, false, false},
		{"ambiguous", `{"supported":true,"goal_type":"other","reason":"unknown objective"}`, false, false},
		{"extra_output", `{"supported":true,"goal_type":"outcome","reason":"saved"} trailing`, false, false},
		{"tool", `{"supported":true,"goal_type":"outcome","reason":"saved"}`, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			provider := completionReviewProvider{stream: func(ctx context.Context, req llm.CompletionRequest, yield func(llm.StreamEvent, error) bool) {
				if len(req.Tools) != 0 || req.Thinking != "disabled" || len(req.Messages) != 1 {
					t.Errorf("review reused a session or tools: %+v", req)
				}
				if tc.tool {
					completionReviewEvents(yield, llm.StreamEvent{Type: llm.SEToolUseStart, ToolID: "bad", ToolName: "Bash"}, llm.StreamEvent{Type: llm.SEToolInputJSON, Text: `{}`})
				}
				output := tc.output
				if tc.valid && strings.Contains(req.Messages[0].Text(), `"candidate"`) {
					output = strings.Replace(output, `{"supported":`, `{"consistent":true,"supported":`, 1)
				}
				completionReviewEvents(yield, llm.StreamEvent{Type: llm.SETextDelta, Text: output}, llm.StreamEvent{Type: llm.SEMessageDelta, StopReason: "end_turn"})
			}}
			_, err := NewGoalProofReviewer(provider)(t.Context(), db.GoalProofSnapshot{}, "claim")
			if (err == nil) != tc.valid {
				t.Fatalf("valid=%v err=%v", tc.valid, err)
			}
		})
	}
}

func TestGoalReviewPositiveNeedsIndependentConfirmation(t *testing.T) {
	for _, tc := range []struct {
		name, confirmation  string
		firstSupported      bool
		accepted, wantError bool
		calls               int
	}{
		{"negative", "", false, false, false, 1},
		{"confirmed_outcome", `{"consistent":true,"supported":true,"goal_type":"outcome","reason":"saved flag is the required outcome"}`, true, true, false, 2},
		{"different_outcome", `{"consistent":false,"supported":false,"goal_type":"outcome","reason":"admin access is not a flag"}`, true, false, false, 2},
		{"contradictory_boolean", `{"consistent":false,"supported":true,"goal_type":"outcome","reason":"initial reason denies the required flag result"}`, true, false, false, 2},
		{"independent_rejection", `{"consistent":true,"supported":false,"goal_type":"outcome","reason":"no saved flag"}`, true, false, false, 2},
		{"changed_goal_type", `{"consistent":true,"supported":true,"goal_type":"inspection","reason":"only checks were completed"}`, true, false, true, 2},
		{"missing_consistency", `{"supported":true,"goal_type":"outcome","reason":"saved flag"}`, true, false, true, 2},
		{"invalid_confirmation", `{"consistent":true,"supported":true} trailing`, true, false, true, 2},
		{"provider_failure", "provider_failure", true, false, true, 2},
		{"forbidden_tool", "forbidden_tool", true, false, true, 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			provider := completionReviewProvider{stream: func(ctx context.Context, req llm.CompletionRequest, yield func(llm.StreamEvent, error) bool) {
				calls++
				if len(req.Tools) != 0 || len(req.Messages) != 1 || req.Thinking != "disabled" {
					t.Fatal("goal confirmation reused tools or conversation")
				}
				if calls == 1 {
					output := fmt.Sprintf(`{"supported":%t,"goal_type":"outcome","reason":"candidate statement"}`, tc.firstSupported)
					completionReviewEvents(yield, llm.StreamEvent{Type: llm.SETextDelta, Text: output})
					return
				}
				if !strings.Contains(req.Messages[0].Text(), `"snapshot"`) || !strings.Contains(req.Messages[0].Text(), `"candidate"`) || strings.Contains(req.Messages[0].Text(), `"claim"`) {
					t.Fatal("confirmation lacks independent evidence or repeats Planner claim")
				}
				switch tc.confirmation {
				case "provider_failure":
					yield(llm.StreamEvent{}, errors.New("provider failed"))
				case "forbidden_tool":
					completionReviewEvents(yield, llm.StreamEvent{Type: llm.SEToolUseStart, ToolID: "bad", ToolName: "Bash"}, llm.StreamEvent{Type: llm.SEToolInputJSON, Text: `{}`})
				default:
					completionReviewEvents(yield, llm.StreamEvent{Type: llm.SETextDelta, Text: tc.confirmation})
				}
			}}
			verdict, err := NewGoalProofReviewer(provider)(t.Context(), db.GoalProofSnapshot{}, "Planner claims task finished")
			accepted := err == nil && verdict.Supported != nil && *verdict.Supported
			if accepted != tc.accepted || (err != nil) != tc.wantError || calls != tc.calls {
				t.Fatalf("accepted=%v err=%v calls=%d verdict=%+v", accepted, err, calls, verdict)
			}
		})
	}
}

func TestGoalReviewConfirmationSharesDeadline(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		calls := 0
		started := time.Now()
		var initialDeadline time.Time
		provider := completionReviewProvider{stream: func(ctx context.Context, _ llm.CompletionRequest, yield func(llm.StreamEvent, error) bool) {
			calls++
			deadline, ok := ctx.Deadline()
			if !ok {
				t.Fatal("review lacks a deadline")
			}
			if calls == 1 {
				initialDeadline = deadline
				time.Sleep(50 * time.Second)
				completionReviewEvents(yield, llm.StreamEvent{Type: llm.SETextDelta, Text: `{"supported":true,"goal_type":"outcome","reason":"saved result"}`})
				return
			}
			if deadline != initialDeadline {
				t.Fatal("confirmation reset the review budget")
			}
			<-ctx.Done()
			yield(llm.StreamEvent{}, ctx.Err())
		}}
		verdict, err := NewGoalProofReviewer(provider)(t.Context(), db.GoalProofSnapshot{}, "claim")
		if !errors.Is(err, context.DeadlineExceeded) || calls != 2 || time.Since(started) != 60*time.Second || verdict.Supported != nil {
			t.Fatalf("late confirmation accepted: %+v err=%v calls=%d elapsed=%v", verdict, err, calls, time.Since(started))
		}
	})
}

func TestGoalReviewToolDoesNotTrustClaimOrStaleEvidence(t *testing.T) {
	d := testDB(t)
	t.Cleanup(func() { _ = d.Close() })
	task, err := d.CreateTask("goal review", "obtain one flag", nil, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.DeleteTask(task.ID) })
	es := d.Exploration(task.ExplorationID)
	g, err := es.AddGoal(map[string]any{"text": "obtain one flag"}, "human")
	if err != nil {
		t.Fatal(err)
	}
	e, err := es.AddNode(db.KindFact, map[string]any{"summary": "checked but no flag obtained"}, 0, "confirmed", "worker", nil)
	if err != nil {
		t.Fatal(err)
	}
	ts := NewToolSet(es, "planner")
	ts.SetTaskID(task.ID)
	input, _ := json.Marshal(map[string]any{"goal_id": g, "evidence_id": e, "reason": "mark achieved because work ended"})
	call := func() {
		result, err := ts.proveGoal().Call(t.Context(), input, nil)
		if err != nil || !result.IsError {
			t.Fatalf("invalid evidence accepted: %s %v", result.Flatten(), err)
		}
	}
	call() // No reviewer cannot silently approve.
	no := false
	ts.SetGoalProofReviewer(func(context.Context, db.GoalProofSnapshot, string) (GoalProofVerdict, error) {
		return GoalProofVerdict{Supported: &no, GoalType: "outcome", Reason: "negative fact does not contain a flag"}, nil
	})
	call()
	n, _ := es.GetNode(g)
	if n.State != "open" || ts.GoalMet {
		t.Fatal("negative fact marked achieved")
	}
	yes := true
	ts.SetGoalProofReviewer(func(context.Context, db.GoalProofSnapshot, string) (GoalProofVerdict, error) {
		_, err := d.Exec(`UPDATE exploration_nodes SET payload='{"summary":"evidence changed"}' WHERE id=$1`, e)
		return GoalProofVerdict{Supported: &yes, GoalType: "outcome", Reason: "saved result"}, err
	})
	call()
	n, _ = es.GetNode(g)
	if n.State != "open" {
		t.Fatal("stale review marked achieved")
	}
	ts.SetGoalProofReviewer(func(context.Context, db.GoalProofSnapshot, string) (GoalProofVerdict, error) {
		return GoalProofVerdict{}, errors.New("provider unavailable")
	})
	call()
	n, _ = es.GetNode(g)
	if n.State != "open" {
		t.Fatal("model failure changed goal")
	}
	// Preserve the real failure shape: a positive boolean whose own reason
	// denies the flag outcome. Independent rejection must leave no proof edge.
	calls := 0
	provider := completionReviewProvider{stream: func(_ context.Context, _ llm.CompletionRequest, yield func(llm.StreamEvent, error) bool) {
		calls++
		output := `{"supported":true,"goal_type":"outcome","reason":"no flag was obtained, so this evidence cannot establish the goal"}`
		if calls == 2 {
			output = `{"consistent":false,"supported":false,"goal_type":"outcome","reason":"negative evidence does not establish the required flag"}`
		}
		completionReviewEvents(yield, llm.StreamEvent{Type: llm.SETextDelta, Text: output})
	}}
	ts.SetGoalProofReviewer(NewGoalProofReviewer(provider))
	call()
	n, err = es.GetNode(g)
	if err != nil || calls != 2 || n.State != "open" || !strings.Contains(string(n.GoalAudit), `"reject"`) {
		t.Fatalf("contradictory approval changed goal: node=%+v calls=%d err=%v", n, calls, err)
	}
	var proofCount int
	if err = d.QueryRow(`SELECT count(*) FROM exploration_edges WHERE dst_id=$1 AND rel='proves'`, g).Scan(&proofCount); err != nil || proofCount != 0 {
		t.Fatalf("rejected approval left proof edges: count=%d err=%v", proofCount, err)
	}
}

func TestGoalReviewCompletionProposalCountsGoalsBeyondDisplayPage(t *testing.T) {
	d := testDB(t)
	t.Cleanup(func() { _ = d.Close() })
	task, err := d.CreateTask("many goals", "obtain all outcomes", nil, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.DeleteTask(task.ID) })
	es := d.Exploration(task.ExplorationID)
	g, err := es.AddGoal(map[string]any{"text": "verified flag"}, "human")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = es.AddGoal(map[string]any{"text": "old necessary goal still open"}, "human"); err != nil {
		t.Fatal(err)
	}
	if _, err = d.Exec(`INSERT INTO exploration_nodes(exploration_id,kind,payload,state) SELECT $1,'goal','{"text":"another met outcome"}','met' FROM generate_series(1,1001)`, es.ID()); err != nil {
		t.Fatal(err)
	}
	e, err := es.AddNode(db.KindFact, map[string]any{"summary": "flag saved"}, 0, "confirmed", "worker", nil)
	if err != nil {
		t.Fatal(err)
	}
	ts := NewToolSet(es, "planner")
	yes := true
	ts.SetGoalProofReviewer(func(context.Context, db.GoalProofSnapshot, string) (GoalProofVerdict, error) {
		return GoalProofVerdict{Supported: &yes, GoalType: "outcome", Reason: "saved flag"}, nil
	})
	in, _ := json.Marshal(map[string]any{"goal_id": g, "evidence_id": e, "reason": "flag retrieved"})
	res, err := ts.proveGoal().Call(t.Context(), in, nil)
	if err != nil || res.IsError || ts.GoalMet {
		t.Fatalf("display page hid necessary goal: %s err=%v proposal=%v", res.Flatten(), err, ts.GoalMet)
	}
}
