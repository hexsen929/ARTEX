package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"testing"

	"github.com/Autumn-27/artex/db"
)

func TestMemoryManagementSearchMatchesTagsLiterally(t *testing.T) {
	f := newMemoryHTTPFixture(t)
	tag := "onlytag_%_fixture"
	entry := f.create(t, "-tag", map[string]any{"tags": []string{tag}})
	f.create(t, "-decoy", map[string]any{"tags": []string{"onlytag-xx-fixture"}})
	w := f.request(t, http.MethodGet, "/api/memories?q="+url.QueryEscape(tag), nil)
	var result struct {
		Items []db.MemoryEntry `json:"items"`
		Total int              `json:"total"`
	}
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &result) != nil || result.Total != 1 || len(result.Items) != 1 || result.Items[0].ID != entry.ID {
		t.Fatalf("tag-only literal search: %d %s", w.Code, w.Body)
	}
}

func TestMemoryHistoryHTTPContinuesAfterEmptyPage(t *testing.T) {
	f := newMemoryHTTPFixture(t)
	task, err := f.pg.CreateTask("history fixture", "synthetic stages", nil, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		f.pg.DeleteTask(task.ID)
		f.pg.Exec(`DELETE FROM memory_jobs WHERE source_ref IN (SELECT id FROM memory_sources WHERE origin_task_id=$1)`, task.ID)
		f.pg.Exec(`DELETE FROM memory_sources WHERE origin_task_id=$1`, task.ID)
	})
	store := f.pg.Exploration(task.ExplorationID)
	for i := 0; i < 51; i++ {
		intent, err := store.AddNode(db.KindIntent, map[string]any{"summary": "fixture"}, 0, "running", "planner", nil)
		if err != nil {
			t.Fatal(err)
		}
		if i == 50 {
			fact, err := store.AddNode(db.KindFact, map[string]any{"summary": "fixture tool supports bounded structured output", "confidence": "observed"}, 0, "confirmed", "worker", nil)
			if err != nil {
				t.Fatal(err)
			}
			if err = store.Link(intent, db.RelYields, fact); err != nil {
				t.Fatal(err)
			}
		}
		if err = store.SetIntentState(intent, "done"); err != nil {
			t.Fatal(err)
		}
		if _, err = f.pg.Exec(`UPDATE exploration_nodes SET completed_at=now()-($2*interval '1 second') WHERE id=$1`, intent, 100-i); err != nil {
			t.Fatal(err)
		}
	}
	w := f.request(t, http.MethodPost, "/api/memories/extract", map[string]any{"task_id": task.ID})
	var first db.MemoryHistoryProgress
	if w.Code != 202 || json.Unmarshal(w.Body.Bytes(), &first) != nil || first.Scanned != 50 || first.Queued != 0 || !first.HasMore {
		t.Fatalf("first page: %d %s", w.Code, w.Body)
	}
	w = f.request(t, http.MethodPost, "/api/memories/extract", map[string]any{"task_id": task.ID, "cursor": first.NextCursor})
	var next db.MemoryHistoryProgress
	if w.Code != 202 || json.Unmarshal(w.Body.Bytes(), &next) != nil || next.Scanned != 1 || next.Queued != 1 || next.HasMore {
		t.Fatalf("later page: %d %s", w.Code, w.Body)
	}
	for _, body := range []map[string]any{{"task_id": task.ID, "cursor": "bad-cursor"}, {"task_id": task.ID + 1, "cursor": first.NextCursor}} {
		if w = f.request(t, http.MethodPost, "/api/memories/extract", body); w.Code != 400 {
			t.Fatalf("invalid cursor accepted: %d %s", w.Code, w.Body)
		}
	}
	if f.calls != 0 {
		t.Fatal(fmt.Sprintf("history API invoked model %d times", f.calls))
	}
}
