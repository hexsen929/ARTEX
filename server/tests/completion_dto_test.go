//go:build ignore

package server

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/Autumn-27/artex/db"
)

func TestCompletionDTOAndReportPreserveUnfinishedReason(t *testing.T) {
	input := db.CompletionInput{Data: json.RawMessage(`{"private_large_snapshot":"must not leak into task DTO"}`), Review: "目标证据已经复核", Work: []db.CompletionWork{{IntentID: 12, State: "running", Summary: "remaining check", CompletionDisposition: db.CompletionDisposition{Kind: "irrelevant", Reason: "outside original goal"}}}}
	raw, _ := json.Marshal(input)
	input.GoalGaps = []db.CompletionGoalGap{{GoalID: 4, ExpectedVersion: 2, Reason: "未取得 flag"}}
	raw, _ = json.Marshal(input)
	c := &db.TaskCompletion{AttemptID: 3, Phase: "completed", Reason: "validated", Snapshot: raw}
	dto := completionDTO(c)
	if dto == nil || len(dto.RemainingWork) != 1 || dto.RemainingWork[0].Reason != "outside original goal" {
		t.Fatalf("missing disposition: %+v", dto)
	}
	data, err := json.Marshal(dto)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "private_large_snapshot") {
		t.Fatal("task DTO exposes full raw evidence snapshot")
	}
	if len(dto.GoalGaps) != 1 || dto.GoalGaps[0].Reason != "未取得 flag" {
		t.Fatal("task DTO lost structured goal gap")
	}
	report := completionReport(c)
	for _, text := range []string{"停止决定不代表检查验证通过", "#12", "outside original goal", "目标证据已经复核", "目标 #4", "未取得 flag"} {
		if !strings.Contains(report, text) {
			t.Fatalf("report misses %q: %s", text, report)
		}
	}
	if completionDTO(nil) != nil || completionReport(nil) != "" {
		t.Fatal("legacy task completion is not optional")
	}
}
