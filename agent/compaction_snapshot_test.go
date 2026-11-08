package agent

import (
	"context"
	"errors"
	"testing"

	"github.com/Autumn-27/artex/db"
	"github.com/Autumn-27/norma/llm"
)

func TestExecutionCompactionRejectsChangedSourceBeforePublication(t *testing.T) {
	d := testDB(t)
	t.Cleanup(func() { _ = d.Close() })
	exp, err := d.CreateExploration("compaction version fixture", "fixture")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = d.Exec(`DELETE FROM explorations WHERE id=$1`, exp) })
	s := d.Exploration(exp)
	a, err := s.AddNode("fact", map[string]any{"summary": "old observation"}, 5, "confirmed", "test", nil)
	if err != nil {
		t.Fatal(err)
	}
	b, err := s.AddNode("fact", map[string]any{"summary": "unchanged observation"}, 5, "confirmed", "test", nil)
	if err != nil {
		t.Fatal(err)
	}
	g, nodes, err := loadColdGraph(s)
	if err != nil {
		t.Fatal(err)
	}
	versions, err := s.ContentVersions()
	if err != nil {
		t.Fatal(err)
	}
	provider := captureUsageProvider{stream: func(ctx context.Context, yield func(llm.StreamEvent, error) bool) {
		if _, err := d.Exec(`UPDATE exploration_nodes SET payload=jsonb_set(payload,'{summary}','"new corrected observation"'),content_version=content_version+1 WHERE id=$1`, a); err != nil {
			yield(llm.StreamEvent{}, err)
			return
		}
		yield(llm.StreamEvent{Type: llm.SETextDelta, Text: "old stale summary"}, nil)
		yield(llm.StreamEvent{Type: llm.SEMessageDelta, StopReason: "end_turn"}, nil)
	}}
	c := NewCompactor(provider, "fixture")
	if err := c.foldBlock(t.Context(), s, g, block{Members: []int64{a, b}}, nodes, versions, 1); !errors.Is(err, db.ErrDigestSnapshotChanged) {
		t.Fatal("stale model output was published", err)
	}
	if active, err := s.ActiveDigests(); err != nil || len(active) != 0 {
		t.Fatal("failed snapshot created digest", active, err)
	}
	g, nodes, err = loadColdGraph(s)
	if err != nil {
		t.Fatal(err)
	}
	versions, err = s.ContentVersions()
	if err != nil {
		t.Fatal(err)
	}
	c.prov = captureUsageProvider{stream: func(ctx context.Context, yield func(llm.StreamEvent, error) bool) {
		yield(llm.StreamEvent{Type: llm.SETextDelta, Text: "fresh summary"}, nil)
		yield(llm.StreamEvent{Type: llm.SEMessageDelta, StopReason: "end_turn"}, nil)
	}}
	if err := c.foldBlock(t.Context(), s, g, block{Members: []int64{a, b}}, nodes, versions, 1); err != nil {
		t.Fatal("unchanged snapshot not published", err)
	}
}
