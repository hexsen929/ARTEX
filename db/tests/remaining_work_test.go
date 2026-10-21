//go:build ignore

package db

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"testing"
)

func TestRemainingWorkPagesAllStatesBeyondRecentWindow(t *testing.T) {
	d, task, es := completionFixture(t)
	states := []string{"open", "running", "paused", "blocked", "exhausted", "stopped", "deleted"}
	wantIDs := []int64{}
	add := func(state string) int64 {
		t.Helper()
		id, err := es.AddNode(KindIntent, map[string]any{"summary": fmt.Sprintf("pending %d", len(wantIDs))}, 0, state, "human", nil)
		if err != nil {
			t.Fatal(err)
		}
		wantIDs = append(wantIDs, id)
		return id
	}
	for _, state := range states {
		add(state)
	}
	if _, err := d.Exec(`INSERT INTO exploration_nodes(exploration_id,kind,payload,state)
SELECT $1,'intent','{"summary":"done"}'::jsonb,'done' FROM generate_series(1,320)`, task.ExplorationID); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 205; i++ {
		add(states[i%len(states)])
	}
	if _, err := d.Exec(`UPDATE exploration_nodes SET blocked_reason='waiting on operator',delete_reason='operator removed this check' WHERE id=$1`, wantIDs[0]); err != nil {
		t.Fatal(err)
	}
	foreign, err := d.CreateTask("unrelated", "unrelated", nil, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.DeleteTask(foreign.ID) })
	if _, err := d.Exploration(foreign.ExplorationID).AddNode(KindIntent, map[string]any{"summary": "foreign"}, 0, "open", "human", nil); err != nil {
		t.Fatal(err)
	}
	var after int64
	gotIDs := []int64{}
	seenStates := map[string]bool{}
	for {
		page, err := es.ListRemainingWork(t.Context(), after, 31)
		if err != nil {
			t.Fatal(err)
		}
		if page.Total != len(wantIDs) || len(page.Items) > 31 {
			t.Fatalf("invalid page: %+v", page)
		}
		for _, item := range page.Items {
			gotIDs = append(gotIDs, item.IntentID)
			seenStates[item.State] = true
		}
		if after == 0 && (page.Items[0].BlockedReason != "waiting on operator" || page.Items[0].DeleteReason != "operator removed this check") {
			t.Fatalf("missing existing stop reasons: %+v", page.Items[0])
		}
		if !page.HasMore {
			break
		}
		if page.NextAfter <= after {
			t.Fatalf("cursor failed to advance: %+v", page)
		}
		after = page.NextAfter
	}
	if !reflect.DeepEqual(gotIDs, wantIDs) {
		t.Fatalf("page traversal got %d IDs, expected %d without omissions or duplicates", len(gotIDs), len(wantIDs))
	}
	for _, state := range states {
		if !seenStates[state] {
			t.Fatalf("missing state %s", state)
		}
	}
	page, err := es.ListRemainingWork(t.Context(), 0, 10000)
	if err != nil || len(page.Items) != 200 || !page.HasMore {
		t.Fatalf("page cap=%+v error=%v", page, err)
	}
	page, err = es.ListRemainingWork(t.Context(), wantIDs[len(wantIDs)-1], 100)
	if err != nil || len(page.Items) != 0 || page.HasMore || page.Total != len(wantIDs) {
		t.Fatalf("last page=%+v error=%v", page, err)
	}
}

func TestRemainingWorkEmptyAndCanceled(t *testing.T) {
	_, _, es := completionFixture(t)
	page, err := es.ListRemainingWork(t.Context(), 0, 0)
	if err != nil || page.Items == nil || page.Total != 0 || page.HasMore {
		t.Fatalf("empty page=%+v error=%v", page, err)
	}
	if _, err := es.ListRemainingWork(t.Context(), -1, 10); err == nil {
		t.Fatal("negative cursor accepted")
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := es.ListRemainingWork(ctx, 0, 10); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancel ignored: %v", err)
	}
}
