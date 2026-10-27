package agent

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"github.com/Autumn-27/artex/db"
)

func TestPlannerRendersStrategicHintTriggers(t *testing.T) {
	for _, tc := range []struct {
		name   string
		hints  []string
		marker string
	}{
		{"single", []string{"fixture strategic hint alpha"}, "新增了一条战略提示"},
		{"batch", []string{"fixture strategic hint alpha", "fixture strategic hint beta"}, "新增了 2 条战略提示"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// Hint rendering needs no DB lookups. A non-nil store exercises the
			// actual renderer without a fixture database for these pure cases.
			got := renderTriggers(&db.ExplorationStore{}, []TriggerEvent{{Kind: "hint", Hints: tc.hints, Detail: "DETAIL_FIELD_IS_NOT_HINTS"}})
			if !strings.Contains(got, tc.marker) {
				t.Fatalf("missing strategic hint event: %q", got)
			}
			last := -1
			for _, hint := range tc.hints {
				at := strings.Index(got, hint)
				if at <= last {
					t.Fatalf("missing/reordered hint %q in %q", hint, got)
				}
				last = at
			}
			if strings.Contains(got, "DETAIL_FIELD_IS_NOT_HINTS") || strings.Contains(got, "意图 #0") {
				t.Fatalf("hint event degraded to another trigger shape: %q", got)
			}
		})
	}
}

func TestAddHintBatchPreservesPlannerNotificationPayload(t *testing.T) {
	d := testDB(t)
	t.Cleanup(func() { _ = d.Close() })
	task, err := d.CreateTask("planner hint callback fixture", "fixture", nil, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.DeleteTask(task.ID) })
	tools := NewToolSet(d.Exploration(task.ExplorationID), "human")
	tools.SetTaskID(task.ID)
	var events []TriggerEvent
	fallbackWakes := 0
	tools.SetNotify(func() { fallbackWakes++ })
	tools.SetNotifyHint(func(texts []string) {
		events = append(events, TriggerEvent{Kind: "hint", Hints: append([]string(nil), texts...)})
	})
	result, err := tools.addHint().Call(t.Context(), json.RawMessage(`{"hints":[{"text":"fixture strategic hint alpha"},{"text":"fixture strategic hint beta"}]}`), nil)
	if err != nil || result.IsError {
		t.Fatalf("add_hint: %v %s", err, result.Flatten())
	}
	want := []string{"fixture strategic hint alpha", "fixture strategic hint beta"}
	if len(events) != 1 || events[0].Kind != "hint" || !slices.Equal(events[0].Hints, want) || fallbackWakes != 0 {
		t.Fatalf("batch notification lost payload or duplicated wake: events=%+v fallback=%d", events, fallbackWakes)
	}
	nodes, err := tools.ts.ListByKind(db.KindHint, 10)
	if err != nil || len(nodes) != 2 {
		t.Fatalf("hint persistence: nodes=%d err=%v", len(nodes), err)
	}
	got := renderTriggers(tools.ts, events)
	if !strings.Contains(got, "新增了 2 条战略提示") || !strings.Contains(got, strings.Join(want, "；")) {
		t.Fatalf("batch hints did not reach planner trigger text: %q", got)
	}
}
