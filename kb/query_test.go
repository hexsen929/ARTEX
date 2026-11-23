package kb

import (
	"reflect"
	"testing"
)

func TestOperatorOriginalMixedChineseQueryTerms(t *testing.T) {
	got := ExtractTerms("HTTPS 测试 Spring_Boot 反序列化漏洞的验证 alpha.alpha --ab--")
	want := []Term{{"spring_boot", true}, {"alpha.alpha", true}, {"ab", false}}
	// A >8-rune phrase is not an anchor; its overlapping non-stop bigrams are.
	want = append(want, Term{"反序", false}, Term{"序列", false}, Term{"列化", false}, Term{"化漏", false}, Term{"漏洞", false}, Term{"验证", false})
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %#v; want %#v", got, want)
	}
	got = ExtractTerms("1Panel 验证码绕过")
	want = []Term{{"1panel", true}, {"验证码绕过", true}, {"验证", false}, {"证码", false}, {"码绕", false}, {"绕过", false}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %#v; want %#v", got, want)
	}
	if len(ExtractTerms("http https www 测试 使用 如何")) != 0 {
		t.Fatal("original stopwords were retained")
	}
}

func TestOperatorOriginalSDKSearchBucketsAndBrief(t *testing.T) {
	r := Rank([]Entry{{ID: 1, Code: "fixture", Title: "needle", Polarity: "context", Verified: true, Usage: "usage", HasContent: true}}, Query{Q: "needle"})
	d := r.ToolResponse()
	b := d["buckets"].(map[string]any)
	items := b["ammo"].([]map[string]any)
	if len(items) != 1 || items[0]["has_content"] != true || items[0]["usage"] != "usage" {
		t.Fatal(d)
	}
	for _, k := range []string{"content", "score", "path", "source_url"} {
		if _, ok := items[0][k]; ok {
			t.Fatal("not original compact metadata", k)
		}
	}
	if _, ok := d["counts"]; !ok {
		t.Fatal("SDK counts missing")
	}
}

func TestOperatorOriginalKnowledgeScoreGroupsAndTies(t *testing.T) {
	e := Entry{Title: "spring", Code: "spring", Component: "spring", Tags: "spring", CVE: "spring", Versions: "spring", Description: "spring", Usage: "spring", Content: "spring"}
	if score, matches := ScoreTerms(e, []Term{{"spring", true}}, false); score != 12 || matches != 1 {
		t.Fatal(score, matches)
	}
	if score, matches := ScoreTerms(e, []Term{{"spring", true}}, true); score != 14 || matches != 1 {
		t.Fatal(score, matches)
	}
	if score, matches := ScoreTerms(e, []Term{{"spring", false}}, true); score != 7 || matches != 1 {
		t.Fatal(score, matches)
	}
	a := Entry{ID: 1, Code: "a", Title: "needle", Polarity: "context", HitCount: 99}
	b := Entry{ID: 2, Code: "b", Title: "needle", Polarity: "context", Verified: true}
	r := Rank([]Entry{a, b}, Query{Q: "needle", Limit: 1})
	if len(r.Ammo) != 1 || r.Ammo[0].Code != "b" {
		t.Fatal("verified tie did not win / verified context not ammo", r)
	}
	r = Rank([]Entry{a}, Query{Q: "needle missing absent"})
	if len(r.Ammo)+len(r.Playbook)+len(r.Negative) != 0 {
		t.Fatal("three-term query accepted one term match")
	}
}
