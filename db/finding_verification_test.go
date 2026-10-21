package db

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
)

func TestFindingVerificationIndependentAndVersioned(t *testing.T) {
	d, err := Open(testDSN(t))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	task, err := d.CreateTask("verification fixture", "fixture", nil, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer d.DeleteTask(task.ID)
	es := d.Exploration(task.ExplorationID)
	f, err := es.RecordFinding(t.Context(), RecordFindingInput{TaskID: task.ID, Summary: "potential finding", Severity: "high", Evidence: "recorded local result"})
	if err != nil {
		t.Fatal(err)
	}
	defer d.DeleteFinding(f.FindingID)
	initial, err := d.GetFinding(f.FindingID)
	if err != nil || initial.VerificationStatus != VerificationUnverified {
		t.Fatalf("new finding=%+v %v", initial, err)
	}
	if err = d.SetFindingVerification(t.Context(), f.FindingID, VerificationBlocked, "network unavailable; need a baseline", 0); err != nil {
		t.Fatal(err)
	}
	blocked, _ := d.GetFinding(f.FindingID)
	if blocked.Severity != "high" || blocked.Status != "pending" || blocked.VerificationStatus != "blocked" || blocked.EvidenceVersion != 1 {
		t.Fatalf("triage/severity changed: %+v", blocked)
	}
	items, n, err := d.ListFindingsPage(FindingFilter{TaskID: taskIDString(task.ID), Verification: VerificationBlocked}, 1, 20)
	if err != nil || n != 1 || len(items) != 1 {
		t.Fatalf("filter %d %+v %v", n, items, err)
	}
	if err = d.SetFindingVerification(t.Context(), f.FindingID, VerificationConfirmed, "confirmed by comparison", 0); !errors.Is(err, ErrEvidenceConflict) {
		t.Fatalf("stale update %v", err)
	}
	if err = d.SetFindingVerification(t.Context(), f.FindingID, VerificationConfirmed, "", 1); !errors.Is(err, ErrVerificationInput) {
		t.Fatalf("no basis accepted: %v", err)
	}
	if err = d.SetFindingVerification(t.Context(), f.FindingID, VerificationConfirmed, "recorded local result matches the acceptance condition", 1); err != nil {
		t.Fatal(err)
	}
	node, _ := es.GetNode(f.NodeID)
	var payload map[string]any
	_ = json.Unmarshal(node.Payload, &payload)
	if payload["verification_status"] != VerificationConfirmed {
		t.Fatalf("graph not synced: %s", node.Payload)
	}
	meta, _ := d.FindingMetaByNodeID(task.ID)
	if meta[f.NodeID].VerificationStatus != VerificationConfirmed {
		t.Fatal("per-task metadata differs")
	}
	exp, err := d.ListFindingsForExport(FindingFilter{}, []int64{f.FindingID})
	if err != nil || len(exp) != 1 || exp[0].VerificationStatus != VerificationConfirmed {
		t.Fatalf("export metadata %v %v", exp, err)
	}
	var events int
	d.QueryRow(`SELECT count(*) FROM finding_verification_events WHERE finding_id=$1`, f.FindingID).Scan(&events)
	if events != 2 {
		t.Fatalf("audit count %d", events)
	}
	st, err := d.FindingStats()
	if err != nil || st.Verified < 1 {
		t.Fatalf("stats %+v %v", st, err)
	}
}
func taskIDString(id int64) string { return fmt.Sprint(id) }
func TestVerificationEvidenceRequirements(t *testing.T) {
	for _, s := range []string{VerificationConfirmed, VerificationBlocked, VerificationRejected, "invalid"} {
		if ValidateVerification(s, "", "") == nil {
			t.Errorf("accepted empty %s", s)
		}
	}
	if ValidateVerification(VerificationUnverified, "", "") != nil {
		t.Fatal("unconfirmed lead discarded")
	}
}
func TestIntentOutcomeCannotOverwritePause(t *testing.T) {
	d, e := Open(testDSN(t))
	if e != nil {
		t.Fatal(e)
	}
	defer d.Close()
	tk, e := d.CreateTask("outcome fixture", "fixture", nil, 0, 0)
	if e != nil {
		t.Fatal(e)
	}
	defer d.DeleteTask(tk.ID)
	es := d.Exploration(tk.ExplorationID)
	id, e := es.AddIntent(map[string]any{"summary": "fixture"}, 5, nil, "test")
	if e != nil {
		t.Fatal(e)
	}
	es.SetIntentState(id, "running")
	if e = es.SetIntentOutcome(id, "exhausted", "run_budget", "budget exhausted"); e != nil {
		t.Fatal(e)
	}
	n, _ := es.GetNode(id)
	if n.State != "exhausted" || !strings.Contains(string(n.Payload), "run_budget") {
		t.Fatalf("outcome %+v", n)
	}
	es.SetIntentState(id, "paused")
	if e = es.SetIntentOutcome(id, "done", "completed", "late"); !errors.Is(e, ErrIntentStateConflict) {
		t.Fatalf("overwrote pause: %v", e)
	}
}
