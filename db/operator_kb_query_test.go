package db

import (
	"fmt"
	"testing"
	"time"

	"github.com/Autumn-27/artex/kb"
)

func TestOperatorKnowledgeOriginalAnchorFallbackAndContentRules(t *testing.T) {
	d, err := Open(testDSN(t))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	prefix := fmt.Sprintf("opkb%d", time.Now().UnixNano())
	put := func(suffix, title, content string) {
		e := kb.Entry{Code: prefix + "-" + suffix, Title: title, Category: "protocol", Content: content, Source: "fixture", Language: "fixture", EngagementID: prefix, EngagementName: "fixture"}
		if _, _, err := d.KBPut(t.Context(), e); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _, _ = d.Exec("DELETE FROM kb_entries WHERE code=$1", e.Code) })
	}
	put("a", prefix+" alpha", "bodyonlyfixture")
	put("b", prefix+" beta", "")
	put("c", prefix, "")
	search := func(q kb.Query) []kb.Entry {
		es, e := d.KBSearchEntries(t.Context(), q)
		if e != nil {
			t.Fatal(e)
		}
		return es
	}
	if got := search(kb.Query{Q: prefix + " alpha"}); len(got) != 1 || got[0].Code != prefix+"-a" {
		t.Fatal("strong-anchor first pass", got)
	}
	if got := search(kb.Query{Q: prefix + " alpha beta"}); len(got) != 2 {
		t.Fatal("OR fallback / minimum two term matches", got)
	}
	q := kb.Query{Q: "bodyonlyfixture", Engagement: prefix}
	if got := search(q); len(got) != 0 {
		t.Fatal("body searched despite content=false", got)
	}
	q.SearchContent = true
	if got := search(q); len(got) != 1 || got[0].Content != "" {
		t.Fatal("content search / metadata-only materialization", got)
	}
	q.Source = "not-fixture"
	if got := search(q); len(got) != 0 {
		t.Fatal("source filter", got)
	}
}
