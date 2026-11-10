package db

import (
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/Autumn-27/artex/knowledge"
)

func TestKnowledgeScopeVersionsAndRetrieval(t *testing.T) {
	d, err := Open(testDSN(t))
	if err != nil {
		t.Skip(err)
	}
	t.Cleanup(func() { _ = d.Close() })
	a, err := d.CreateTask("kb A", "fixture", nil, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer d.DeleteTask(a.ID)
	b, err := d.CreateTask("kb B", "fixture", nil, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer d.DeleteTask(b.ID)
	makeDoc := func(task int64, title, body string) *knowledge.Document {
		t.Helper()
		doc, e := d.SaveKnowledgeDocument(t.Context(), knowledge.Document{TaskID: task, Title: title, Content: body})
		if e != nil {
			t.Fatal(e)
		}
		t.Cleanup(func() { d.Exec(`DELETE FROM knowledge_docs WHERE id=$1`, doc.ID) })
		return doc
	}
	global := makeDoc(0, "shared", strings.Repeat("padding ", 200)+"KB_SCOPE_SENTINEL")
	owned := makeDoc(a.ID, "owned", "KB_SCOPE_SENTINEL owned")
	foreign := makeDoc(b.ID, "other", "KB_SCOPE_SENTINEL secret")
	for _, task := range []int64{0, a.ID} {
		hits, e := d.SearchKnowledge(t.Context(), task, "KB_SCOPE_SENTINEL", 8, knowledge.Config{}, nil)
		if e != nil {
			t.Fatal(e)
		}
		ids := map[int64]bool{}
		for _, h := range hits {
			ids[h.DocID] = true
			if !strings.Contains(h.Content, "KB_SCOPE_SENTINEL") {
				t.Fatal("returned irrelevant leading excerpt")
			}
		}
		if !ids[global.ID] || ids[foreign.ID] || (task == 0 && ids[owned.ID]) || (task == a.ID && !ids[owned.ID]) {
			t.Fatalf("task scope leak %d %v", task, ids)
		}
	}
	stale := *owned
	owned.Content = "changed"
	saved, e := d.SaveKnowledgeDocument(t.Context(), *owned)
	if e != nil || saved.Version != owned.Version+1 {
		t.Fatal(saved, e)
	}
	if _, e = d.SaveKnowledgeDocument(t.Context(), stale); !errors.Is(e, knowledge.ErrConflict) {
		t.Fatal("stale edit allowed", e)
	}
	if e = d.DeleteKnowledgeDocument(t.Context(), saved.ID, stale.Version); !errors.Is(e, knowledge.ErrConflict) {
		t.Fatal("stale deletion allowed", e)
	}
	_, e = d.Exec(`UPDATE tasks SET archived_at=now() WHERE id=$1`, a.ID)
	if e != nil {
		t.Fatal(e)
	}
	if _, e = d.SearchKnowledge(t.Context(), a.ID, "KB_SCOPE_SENTINEL", 5, knowledge.Config{}, nil); e == nil {
		t.Fatal("archived task readable by agent")
	}
}

func TestKnowledgeVectorPublicationRejectsStaleDocumentsAndSettings(t *testing.T) {
	d, err := Open(testDSN(t))
	if err != nil {
		t.Skip(err)
	}
	t.Cleanup(func() { _ = d.Close() })
	old, err := d.KnowledgeConfig(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if !old.VectorAvailable {
		if os.Getenv("ARTEX_REQUIRE_PGVECTOR") == "1" {
			t.Fatal("pgvector fixture was not initialized")
		}
		t.Skip("optional pgvector not installed")
	}
	t.Cleanup(func() {
		d.Exec(`UPDATE knowledge_config SET enabled=$1,base_url=$2,model=$3,dimension=$4,api_key=$5,epoch=$6`, old.Enabled, old.BaseURL, old.Model, old.Dimension, old.APIKey, old.Epoch)
	})
	c := old
	c.Enabled = true
	c.BaseURL = "http://127.0.0.1:1"
	c.Model = "fixture"
	c.Dimension = 2
	c, err = d.SaveKnowledgeConfig(t.Context(), c, nil)
	if err != nil {
		t.Fatal(err)
	}
	doc, err := d.SaveKnowledgeDocument(t.Context(), knowledge.Document{Title: "vector fixture", Content: "vector fixture"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Exec(`DELETE FROM knowledge_docs WHERE id=$1`, doc.ID) })
	if err = d.PublishKnowledgeVectors(t.Context(), *doc, c, [][]float32{{1, 0}}); err != nil {
		t.Fatal(err)
	}
	hits, err := d.SearchKnowledge(t.Context(), 0, "unrelated", 5, c, []float32{1, 0})
	if err != nil || len(hits) == 0 || hits[0].DocID != doc.ID {
		t.Fatal(hits, err)
	}
	changed := *doc
	changed.Content = "new content"
	updated, err := d.SaveKnowledgeDocument(t.Context(), changed)
	if err != nil {
		t.Fatal(err)
	}
	if err = d.PublishKnowledgeVectors(t.Context(), *doc, c, [][]float32{{1, 0}}); !errors.Is(err, knowledge.ErrConflict) {
		t.Fatal("stale document index published", err)
	}
	hits, err = d.SearchKnowledge(t.Context(), 0, "x", 5, c, []float32{1, 0})
	if err != nil || len(hits) > 0 {
		t.Fatal("stale vectors remained visible", hits, err)
	}
	_, err = d.SaveKnowledgeConfig(t.Context(), c, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err = d.PublishKnowledgeVectors(t.Context(), *updated, c, [][]float32{{1, 0}}); !errors.Is(err, knowledge.ErrConflict) {
		t.Fatal("stale settings published", err)
	}
}
