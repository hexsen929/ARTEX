//go:build ignore

package agent

import (
	"encoding/json"
	"fmt"
	"slices"
	"testing"

	"github.com/Autumn-27/artex/db"
)

func TestRemainingWorkToolIsReadableAndPaged(t *testing.T) {
	d := testDB(t)
	t.Cleanup(func() { _ = d.Close() })
	task, err := d.CreateTask("remaining work tool", "verify paging", nil, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.DeleteTask(task.ID) })
	es := d.Exploration(task.ExplorationID)
	for _, state := range []string{"paused", "deleted", "stopped", "done"} {
		if _, err := es.AddNode(db.KindIntent, map[string]any{"summary": state}, 0, state, "human", nil); err != nil {
			t.Fatal(err)
		}
	}
	set := NewToolSet(es, "planner")
	tool := set.listRemainingWork()
	if !tool.IsReadOnly(json.RawMessage(`{}`)) {
		t.Fatal("remaining work reads should not require write permissions")
	}
	seeded := false
	for _, seed := range BuiltinToolSeeds() {
		if seed.Key == tool.Name() {
			seeded = slices.Contains(seed.Agents, "planner")
		}
	}
	if !seeded {
		t.Fatal("planner cannot discover list_remaining_work")
	}
	var after int64
	got := []string{}
	for {
		result, err := tool.Call(t.Context(), json.RawMessage(fmt.Sprintf(`{"after":%d,"limit":1}`, after)), nil)
		if err != nil || result.IsError {
			t.Fatalf("tool result=%+v error=%v", result, err)
		}
		var page db.RemainingWorkPage
		if err := json.Unmarshal([]byte(result.Flatten()), &page); err != nil {
			t.Fatal(err)
		}
		if page.Total != 3 || len(page.Items) != 1 {
			t.Fatalf("page=%+v", page)
		}
		got = append(got, page.Items[0].State)
		if !page.HasMore {
			break
		}
		after = page.NextAfter
	}
	if !slices.Equal(got, []string{"paused", "deleted", "stopped"}) {
		t.Fatalf("states=%v", got)
	}
	for _, raw := range []string{`{"after":-1}`, `{"after":"bad"}`} {
		result, err := tool.Call(t.Context(), json.RawMessage(raw), nil)
		if err != nil || !result.IsError {
			t.Fatalf("invalid cursor was not rejected: %+v %v", result, err)
		}
	}
}
