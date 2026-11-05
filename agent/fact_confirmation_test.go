package agent

import (
	"encoding/json"
	"slices"
	"testing"

	"github.com/Autumn-27/artex/db"
)

func TestExecutionFactConfirmationKeepsEvidenceAndScope(t *testing.T) {
	ts := factTestRig(t)
	id, _, err := ts.recordOneFact(factItem{Summary: "fixture hypothesis", Confidence: "confirmed", Evidence: "initial ambiguous result"}, 0)
	if err != nil {
		t.Fatal(err)
	}
	if p := factPayload(t, ts, id); p["confidence"] != FactConfidenceInferred {
		t.Fatal("record_fact bypassed verification", p)
	}
	call := func(input map[string]any) bool {
		t.Helper()
		raw, _ := json.Marshal(input)
		result, err := ts.confirmFact().Call(t.Context(), raw, nil)
		if err != nil {
			t.Fatal(err)
		}
		return result.IsError
	}
	if !call(map[string]any{"fact_id": id}) {
		t.Fatal("ID-only confirmation was accepted")
	}
	request, _ := json.Marshal(map[string]any{"id": id})
	result, err := ts.nodeDetail().Call(t.Context(), request, nil)
	if err != nil || result.IsError {
		t.Fatal(result, err)
	}
	var detail db.Node
	if err := json.Unmarshal([]byte(result.Flatten()), &detail); err != nil || detail.FactVersion == "" {
		t.Fatal("reader did not expose snapshot version", err)
	}
	input := map[string]any{"fact_id": id, "fact_version": detail.FactVersion, "verification_evidence": "fixture response and paired control", "tested_scope": "account A, object A, read operation; other accounts and writes untested", "verification_note": "observed expected authorization for this pair only"}
	input["fact_version"] = "stale"
	if !call(input) {
		t.Fatal("stale fact confirmation was accepted")
	}
	input["fact_version"] = detail.FactVersion
	if call(input) {
		t.Fatal("valid fact confirmation failed")
	}
	current, err := ts.ts.GetNode(id)
	if err != nil {
		t.Fatal(err)
	}
	p := factPayload(t, ts, id)
	for _, field := range []string{"verification_evidence", "tested_scope", "verification_note"} {
		if p[field] != input[field] {
			t.Fatal("confirmation lost field", field, p)
		}
	}
	if p["confidence"] != FactConfidenceConfirmed || p["evidence"] != "initial ambiguous result" {
		t.Fatal("confirmation changed initial evidence", p)
	}
	compact := compactFact(current)
	if compact["tested_scope"] != input["tested_scope"] || compact["has_verification_evidence"] != true || compact["confidence"] != FactConfidenceConfirmed {
		t.Fatal("planning view lost scope or confidence", compact)
	}
	// A delayed confirmation cannot overwrite a changed snapshot at the DB boundary.
	if err := ts.ts.ConfirmFact(t.Context(), id, "late", detail.FactVersion, db.FactConfirmation{Evidence: "late", Scope: "different", Note: "late"}); err == nil {
		t.Fatal("database accepted stale confirmation")
	}
	if err := ts.ts.SetNodeState(id, "dismissed"); err != nil {
		t.Fatal(err)
	}
	rejected, err := ts.ts.GetNode(id)
	if err != nil {
		t.Fatal(err)
	}
	if err := ts.ts.ConfirmFact(t.Context(), id, "late", db.FactSnapshotVersion(rejected), db.FactConfirmation{Evidence: "late", Scope: "different", Note: "late"}); err == nil {
		t.Fatal("excluded fact was confirmed")
	}
	// Existing confirmed facts stay intact and can receive a real follow-up
	// confirmation; a legacy status must not prevent adding the missing evidence.
	legacyID, err := ts.ts.AddNode(db.KindFact, map[string]any{"summary": "legacy observation", "confidence": "confirmed"}, 5, "confirmed", "fixture", nil)
	if err != nil {
		t.Fatal(err)
	}
	legacy, err := ts.ts.GetNode(legacyID)
	if err != nil {
		t.Fatal(err)
	}
	input["fact_id"], input["fact_version"] = legacyID, db.FactSnapshotVersion(legacy)
	if call(input) || factPayload(t, ts, legacyID)["verification_evidence"] != input["verification_evidence"] {
		t.Fatal("legacy confirmation discarded new evidence")
	}
}

func TestExecutionFactConfirmationSchemaPreservesPolicy(t *testing.T) {
	d := testDB(t)
	t.Cleanup(func() { _ = d.Close() })
	tool := NewToolSet(nil, "worker").confirmFact()
	defaults, _ := json.Marshal(tool.InputSchema())
	old, err := d.GetTool("confirm_fact")
	if err != nil {
		t.Fatal(err)
	}
	if old == nil {
		if err := d.SeedTool("confirm_fact", tool.Description(), defaults, json.RawMessage(`["worker"]`)); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _, _ = d.Exec(`DELETE FROM tools WHERE key='confirm_fact'`) })
		old, err = d.GetTool("confirm_fact")
		if err != nil || old == nil {
			t.Fatal(old, err)
		}
	}
	t.Cleanup(func() { _, _ = d.Exec(`UPDATE tools SET schema=$1 WHERE key='confirm_fact'`, string(old.Schema)) })
	legacy := `{"type":"object","properties":{"fact_id":{"type":"integer","description":"CUSTOM"}},"required":["fact_id"]}`
	if _, err := d.Exec(`UPDATE tools SET schema=$1 WHERE key='confirm_fact'`, legacy); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if err := d.MergeFactConfirmationToolSchema(defaults); err != nil {
			t.Fatal(err)
		}
	}
	saved, err := d.GetTool("confirm_fact")
	if err != nil {
		t.Fatal(err)
	}
	var schema struct {
		Properties map[string]map[string]any `json:"properties"`
		Required   []string                  `json:"required"`
	}
	if err := json.Unmarshal(saved.Schema, &schema); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"fact_version", "verification_evidence", "tested_scope", "verification_note"} {
		if schema.Properties[key] == nil || !slices.Contains(schema.Required, key) {
			t.Fatal("missing confirmation contract", key)
		}
	}
	if schema.Properties["fact_id"]["description"] != "CUSTOM" || len(schema.Required) != 5 || saved.Description != old.Description || saved.Enabled != old.Enabled || !slices.Equal(saved.Agents, old.Agents) {
		t.Fatal("migration changed customization or policy", saved)
	}
}
