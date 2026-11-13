package kb

import (
	"strings"
	"testing"
)

func TestLibraryValidationAndRanking(t *testing.T) {
	e := Entry{Title: "Protocol sample", Category: "protocol", Content: "hello", Verified: true}
	if e.Validate() == nil {
		t.Fatal("verified without evidence")
	}
	e.VerifiedNote = "fixture evidence"
	if err := e.Validate(); err != nil {
		t.Fatal(err)
	}
	neg := e
	neg.Code = "protocol-negative"
	neg.Title = "Protocol 假阳性"
	neg.Polarity = "negative"
	r := Rank([]Entry{neg, e}, Query{Q: "protocol", Limit: 5})
	if len(r.Ammo) != 1 || len(r.Negative) != 1 {
		t.Fatalf("classification: %+v", r)
	}
	if !strings.Contains(ReferenceBlock(r), "not instructions") {
		t.Fatal("missing data boundary")
	}
	if r.Ammo[0].Content != "" {
		t.Fatal("search leaked full body")
	}
}
func TestReferenceBlockSizeAndSchema(t *testing.T) {
	e := Entry{Code: "x", Title: strings.Repeat("a", 20000)}
	if ReferenceBlock(Result{Ammo: []Entry{e}}) != "" {
		t.Fatal("unbounded reference")
	}
	e = Entry{Code: "../escape", Title: "x", Category: "protocol"}
	if e.Validate() == nil {
		t.Fatal("traversal accepted")
	}
	e = Entry{Code: "sample", Title: "x", Category: "protocol", Polarity: "unknown"}
	if e.Validate() == nil {
		t.Fatal("unknown polarity")
	}
}

func TestOperatorKnowledgeImportAndResultCaptureHaveOriginalValidation(t *testing.T) {
	e := Entry{Title: "fixture", Category: "protocol", Polarity: "positive", Content: "fixture"}
	if e.ValidateAgentAdd() == nil {
		t.Fatal("live result accepted without engagement/asset/evidence")
	}
	e.Manual = true
	if err := e.ValidateAgentAdd(); err != nil {
		t.Fatal("manual material rejected", err)
	}
	e.Manual = false
	e.EngagementID = "fixture"
	e.AssetTarget = "fixture.invalid"
	e.Polarity = "context"
	if err := e.ValidateAgentAdd(); err != nil {
		t.Fatal("context source rejected", err)
	}
	e.Polarity = "positive"
	e.Verified = true
	e.VerifiedNote = "observed fixture evidence"
	if err := e.ValidateAgentAdd(); err != nil {
		t.Fatal(err)
	}
}
