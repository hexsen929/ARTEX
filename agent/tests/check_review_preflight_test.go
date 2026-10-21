//go:build ignore

package agent

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/Autumn-27/artex/db"
)

func TestGoalReviewRejectsKnownIncompleteBeforeModel(t *testing.T) {
	checkpoint := db.Checkpoint{Result: "blocked", Scope: []string{"two candidates"}, Evidence: "second request 429", Gaps: []string{"second candidate not checked"}, Barrier: "rate limit"}
	payload, _ := json.Marshal(map[string]any{"checkpoint": checkpoint})
	snapshot := db.GoalProofSnapshot{Goal: &db.Node{Payload: json.RawMessage(`{"goal_type":"inspection"}`)}, Evidence: &db.Node{Payload: payload}}
	verdict, err := NewGoalProofReviewer(nil)(t.Context(), snapshot, "saved interruption is not complete")
	if err != nil || verdict.Supported == nil || *verdict.Supported || !strings.Contains(verdict.Reason, "缺口") {
		t.Fatal(verdict, err)
	}
}

func TestNewGoalRequiresFixedTypeAndInspectionCoverage(t *testing.T) {
	ts := NewToolSet(nil, "goals")
	for _, item := range []goalItem{{Text: "inspect"}, {Text: "inspect", GoalType: "inspection"}, {Text: "inspect", GoalType: "inspection", RequiredChecks: []string{" "}}} {
		if _, err := ts.addOneGoal(item); err == nil {
			t.Fatal("incomplete contract admitted", item)
		}
	}
}
