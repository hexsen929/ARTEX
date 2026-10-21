//go:build ignore

package db

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestFindingEvidenceSupplementIsAtomicAndInvalidatesReview(t *testing.T) {
	d, task, es, goal, fact := goalProofFixture(t)
	f, err := es.RecordFinding(t.Context(), RecordFindingInput{TaskID: task.ID, Summary: "fixture", Evidence: "old evidence", Severity: "low"})
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := es.GoalProofSnapshot(t.Context(), goal, f.NodeID)
	if err != nil {
		t.Fatal(err)
	}
	if err = es.CommitGoalProof(t.Context(), snapshot, true, "planner", "fixture proof"); err != nil {
		t.Fatal(err)
	}
	current, _ := d.GetFinding(f.FindingID)
	version := current.EvidenceVersion
	if current.EvidenceAssessment != "supported_for_goal" {
		t.Fatal(current.EvidenceAssessment)
	}
	if _, err = d.UpdateFindingEvidence(t.Context(), f.FindingID, version, task.ID+1, "foreign", "worker", "change", nil); err == nil {
		t.Fatal("foreign write accepted")
	}
	other, otherTask, otherStore := completionFixture(t)
	defer other.Close()
	foreignFact, err := otherStore.AddNode(KindFact, map[string]any{"detail": "unrelated"}, 0, "confirmed", "test", nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = d.UpdateFindingEvidence(t.Context(), f.FindingID, version, task.ID, "bad fact", "worker", "change", []int64{foreignFact}); err == nil {
		t.Fatal("foreign fact accepted", otherTask.ID)
	}
	after, _ := d.GetFinding(f.FindingID)
	if after.Evidence != "old evidence" || after.EvidenceVersion != version {
		t.Fatal("failed update was not atomic")
	}
	next, err := d.UpdateFindingEvidence(t.Context(), f.FindingID, version, task.ID, "actual response and conditions", "worker", "new saved fact", []int64{fact})
	if err != nil || next != version+1 {
		t.Fatal(next, err)
	}
	if _, err = d.UpdateFindingEvidence(t.Context(), f.FindingID, version, task.ID, "stale overwrite", "worker", "stale", nil); !errors.Is(err, ErrEvidenceConflict) {
		t.Fatal("stale writer accepted", err)
	}
	if err = es.CommitGoalProof(t.Context(), snapshot, true, "planner", "old snapshot"); !errors.Is(err, ErrGoalProofChanged) {
		t.Fatal("old review committed", err)
	}
	after, _ = d.GetFinding(f.FindingID)
	if after.EvidenceAssessment != "needs_review" {
		t.Fatal(after.EvidenceAssessment)
	}
	if _, err = d.BeginTaskCompletion(t.Context(), task.ID, "old met state", nil, time.Now().Add(time.Minute), false); err == nil {
		t.Fatal("stale proof permitted task completion")
	}
	snapshot, err = es.GoalProofSnapshot(t.Context(), goal, f.NodeID)
	if err != nil || !strings.Contains(string(snapshot.SupportingFacts), "flag{verified_saved_result}") {
		t.Fatal("fact body absent", err, string(snapshot.SupportingFacts))
	}
	var history json.RawMessage
	if err = d.QueryRow(`SELECT evidence_audit FROM findings WHERE id=$1`, f.FindingID).Scan(&history); err != nil || !strings.Contains(string(history), "old evidence") {
		t.Fatal("old evidence lost", err, string(history))
	}
	if err = es.CommitGoalProof(t.Context(), snapshot, true, "planner", "full source reviewed"); err != nil {
		t.Fatal(err)
	}
	if _, err = d.Exec(`UPDATE exploration_nodes SET payload=$2::jsonb,content_version=content_version+1 WHERE id=$1`, fact, `{"detail":"corrected observation"}`); err != nil {
		t.Fatal(err)
	}
	after, _ = d.GetFinding(f.FindingID)
	if after.EvidenceAssessment != "needs_review" {
		t.Fatal("changed source remained supported", after.EvidenceAssessment)
	}
}

func TestFindingAssessmentsSurviveAdditionalProofAndInvalidateOnReopen(t *testing.T) {
	d, task, es, goal, _ := goalProofFixture(t)
	var ids []int64
	for range 2 {
		f, err := es.RecordFinding(t.Context(), RecordFindingInput{TaskID: task.ID, Summary: "independent fixture", Evidence: "saved response", Severity: "low"})
		if err != nil {
			t.Fatal(err)
		}
		snapshot, err := es.GoalProofSnapshot(t.Context(), goal, f.NodeID)
		if err != nil {
			t.Fatal(err)
		}
		if err = es.CommitGoalProof(t.Context(), snapshot, true, "planner", "saved proof"); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, f.FindingID)
	}
	for _, id := range ids {
		f, _ := d.GetFinding(id)
		if f.EvidenceAssessment != "supported_for_goal" {
			t.Fatal("additional proof invalidated unchanged evidence", f.EvidenceAssessment)
		}
	}
	g, _ := es.GetNode(goal)
	if err := es.ReopenGoal(t.Context(), goal, g.ContentVersion, "human", "wrong contract"); err != nil {
		t.Fatal(err)
	}
	for _, id := range ids {
		f, _ := d.GetFinding(id)
		if f.EvidenceAssessment != "needs_review" {
			t.Fatal("reopened goal still supported", f.EvidenceAssessment)
		}
	}
}
