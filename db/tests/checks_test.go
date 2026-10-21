//go:build ignore

package db

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestCheckRecoveryBudgetSurvivesRenameAndReopen(t *testing.T) {
	d, task, es := completionFixture(t)
	check := CheckSpec{Target: "http://127.0.0.1/login", Mechanism: "authentication"}
	var previous int64
	for attempt := 1; attempt <= MaxCheckAttempts; attempt++ {
		id, err := es.AddCheckIntent(t.Context(), map[string]any{"summary": "different wording " + strings.Repeat("x", attempt)}, 5, nil, "planner", check, previous, "repair request encoding")
		if err != nil {
			t.Fatal(err)
		}
		if _, err = es.AddCheckIntent(t.Context(), map[string]any{"summary": "parallel copy"}, 5, nil, "planner", check, 0, "new wording"); err == nil {
			t.Fatal("duplicate active check admitted")
		}
		if err = es.SetIntentState(id, "running"); err != nil {
			t.Fatal(err)
		}
		cp := Checkpoint{Result: "blocked", Scope: []string{"login request"}, Evidence: "429 rate limited", Gaps: []string{"remaining authorized candidates"}, Barrier: "rate_limit", NextAction: "wait for reset"}
		if _, err = es.RecordCheckFact(t.Context(), map[string]any{"summary": "bounded attempt"}, id, cp, "worker", nil); err != nil {
			t.Fatal(err)
		}
		if err = es.FinalizeIntentResult(id, "blocked", "rate_limit", "bounded_backoff"); err != nil {
			t.Fatal(err)
		}
		previous = id
	}
	// A new handle on the same DB models process restart; no in-memory budget.
	reloaded := d.Exploration(task.ExplorationID)
	if _, err := reloaded.AddCheckIntent(t.Context(), map[string]any{"summary": "new work ID after restart"}, 5, nil, "planner", check, 0, "same reset"); err == nil {
		t.Fatal("rename reset attempts")
	}
	if _, err := reloaded.AddCheckIntent(t.Context(), map[string]any{"summary": "bypass target"}, 5, nil, "planner", CheckSpec{Target: "another", Mechanism: "authentication"}, previous, "new target"); err == nil {
		t.Fatal("retry changed identity")
	}
}

func TestCheckExitDoesNotImplyCompleteCoverage(t *testing.T) {
	_, _, es := completionFixture(t)
	id, err := es.AddCheckIntent(t.Context(), map[string]any{"summary": "inspect"}, 5, nil, "planner", CheckSpec{Target: "local fixture", Mechanism: "sqli"}, 0, "")
	if err != nil {
		t.Fatal(err)
	}
	if err = es.SetIntentState(id, "running"); err != nil {
		t.Fatal(err)
	}
	if err = es.FinalizeIntentResult(id, "done", "none", ""); err != nil {
		t.Fatal(err)
	}
	n, _ := es.GetNode(id)
	if n.State != "exhausted" {
		t.Fatalf("exit became coverage: %s", n.State)
	}
	var payload struct {
		End struct {
			State     string `json:"state"`
			Requested string `json:"requested_state"`
		} `json:"execution_end"`
	}
	if err := json.Unmarshal(n.Payload, &payload); err != nil || payload.End.State != "exhausted" || payload.End.Requested != "done" {
		t.Fatalf("execution metadata disagrees with saved state: %s err=%v", n.Payload, err)
	}
}

func TestAuthCheckpointRequestBudgetSurvivesRecovery(t *testing.T) {
	_, _, es := completionFixture(t)
	check := CheckSpec{Target: "local interrupted login", Mechanism: "authentication"}
	id, err := es.AddCheckIntent(t.Context(), map[string]any{"summary": "authorized candidates"}, 5, nil, "planner", check, 0, "")
	if err != nil {
		t.Fatal(err)
	}
	cp := Checkpoint{Result: "blocked", Scope: []string{"two authorized candidates"}, Evidence: "a rejected, b rate limited", Gaps: []string{"b incomplete"}, Barrier: "429", Auth: &AuthCoverage{Request: "POST /login JSON candidate", Budget: 2, Candidates: []AuthCandidate{{ID: "a", State: "failed", Attempts: 1}, {ID: "b", State: "remaining", Attempts: 1}}}}
	if _, err = es.RecordCheckFact(t.Context(), map[string]any{"summary": "saved"}, id, cp, "worker", nil); err != nil {
		t.Fatal(err)
	}
	if err = es.SetIntentState(id, "running"); err != nil {
		t.Fatal(err)
	}
	if err = es.FinalizeIntentResult(id, "blocked", "rate_limit", "wait"); err != nil {
		t.Fatal(err)
	}
	retry, err := es.AddCheckIntent(t.Context(), map[string]any{"summary": "recover"}, 5, nil, "planner", check, id, "rate limit reset")
	if err != nil {
		t.Fatal(err)
	}
	cp.Auth.Candidates[1].Attempts = 0
	if _, err = es.RecordCheckFact(t.Context(), map[string]any{"summary": "reset interrupted request"}, retry, cp, "worker", nil); err == nil {
		t.Fatal("interrupted request count reset")
	}
	cp.Auth.Candidates[1].Attempts = 2
	if _, err = es.RecordCheckFact(t.Context(), map[string]any{"summary": "retry over budget"}, retry, cp, "worker", nil); err == nil {
		t.Fatal("recovery exceeded original request budget")
	}
	cp.Auth.Candidates[1].Attempts = 1
	cp.Auth.Budget = 3
	if _, err = es.RecordCheckFact(t.Context(), map[string]any{"summary": "raise budget"}, retry, cp, "worker", nil); err == nil {
		t.Fatal("recovery raised authorized budget")
	}
	n, err := es.GetNode(retry)
	if err != nil || n == nil {
		t.Fatal(err)
	}
	var saved struct {
		Checkpoint Checkpoint `json:"checkpoint"`
	}
	if err = json.Unmarshal(n.Payload, &saved); err != nil || saved.Checkpoint.Auth == nil || saved.Checkpoint.Auth.Budget != 2 || saved.Checkpoint.Auth.Candidates[1].Attempts != 1 {
		t.Fatalf("failed writes changed checkpoint: %s err=%v", n.Payload, err)
	}
}

func TestAuthCheckpointRetainsPlanAndRejectsPartialNegative(t *testing.T) {
	_, _, es := completionFixture(t)
	id, err := es.AddCheckIntent(t.Context(), map[string]any{"summary": "authorized login"}, 5, nil, "planner", CheckSpec{Target: "local login", Mechanism: "authentication"}, 0, "")
	if err != nil {
		t.Fatal(err)
	}
	cp := Checkpoint{Result: "partial", Scope: []string{"two authorized candidates"}, Evidence: "candidate-1 denied", Gaps: []string{"candidate-2 not executed"}, Auth: &AuthCoverage{Request: "POST /login JSON username,password", Budget: 2, Candidates: []AuthCandidate{{ID: "candidate-1", State: "failed"}, {ID: "candidate-2", State: "remaining"}}}}
	if _, err = es.RecordCheckFact(t.Context(), map[string]any{"summary": "checkpoint"}, id, cp, "worker", nil); err != nil {
		t.Fatal(err)
	}
	omitted := cp
	omitted.Auth, omitted.Result, omitted.Gaps = nil, "negative", nil
	if _, err = es.RecordCheckFact(t.Context(), map[string]any{"summary": "omitted auth"}, id, omitted, "worker", nil); err == nil {
		t.Fatal("omitting auth erased remaining candidates")
	}
	cp.Result = "negative"
	cp.Gaps = nil
	if _, err = es.RecordCheckFact(t.Context(), map[string]any{"summary": "none found"}, id, cp, "worker", nil); err == nil {
		t.Fatal("partial coverage became negative")
	}
	cp.Result = "partial"
	cp.Gaps = []string{"candidate-2"}
	cp.Auth.Candidates[0].State = "remaining"
	if _, err = es.RecordCheckFact(t.Context(), map[string]any{"summary": "reset"}, id, cp, "worker", nil); err == nil {
		t.Fatal("saved attempt reset")
	}
	cp.Auth.Candidates[0].State = "failed"
	cp.Auth.Candidates[1].State = "verified"
	cp.Result = "positive"
	cp.Gaps = nil
	if _, err = es.RecordCheckFact(t.Context(), map[string]any{"summary": "HTTP 200"}, id, cp, "worker", nil); err == nil {
		t.Fatal("HTTP 200 counted as verified identity")
	}
	cp.Auth.Identity = "ordinary user fixture"
	cp.Auth.IdentityEvidence = "GET /me returned role=user and expected id"
	if _, err = es.RecordCheckFact(t.Context(), map[string]any{"summary": "identity verified"}, id, cp, "worker", nil); err != nil {
		t.Fatal(err)
	}
}

func TestGoalContractNegativeSupportsInspectionOnly(t *testing.T) {
	for _, goalType := range []string{"inspection", "outcome"} {
		t.Run(goalType, func(t *testing.T) {
			_, _, es, g, _ := goalProofFixture(t)
			_, err := es.db.Exec(`UPDATE exploration_nodes SET payload=payload || jsonb_build_object('goal_type',$2::text) WHERE id=$1`, g, goalType)
			if err != nil {
				t.Fatal(err)
			}
			cp := Checkpoint{Result: "negative", Scope: []string{"specified login parameters"}, Evidence: "all authorized cases returned rejection", Gaps: []string{}}
			ev, err := es.AddNode(KindFact, map[string]any{"summary": "no injection in tested scope", "checkpoint": cp}, 5, "confirmed", "worker", nil)
			if err != nil {
				t.Fatal(err)
			}
			snapshot, err := es.GoalProofSnapshot(t.Context(), g, ev)
			if err != nil {
				t.Fatal(err)
			}
			err = es.CommitGoalProof(t.Context(), snapshot, true, "planner", "completed prescribed check")
			if (err == nil) != (goalType == "inspection") {
				t.Fatalf("type=%s err=%v", goalType, err)
			}
		})
	}
}

func TestGoalProofContainsSavedFindingAndRejectsScopeDrift(t *testing.T) {
	d, task, es, g, _ := goalProofFixture(t)
	f, err := es.RecordFinding(t.Context(), RecordFindingInput{TaskID: task.ID, ExplorationID: es.ID(), Summary: "script accepted Origin", VulnClass: "CSRF", Severity: "high", Evidence: "Python manually supplied Cookie/Origin"})
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := es.GoalProofSnapshot(t.Context(), g, f.NodeID)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(snapshot.Finding), "manually supplied") || !strings.Contains(string(snapshot.Finding), "traffic") {
		t.Fatal("actual saved evidence missing")
	}
	if _, err = d.Assets().AddAgentScope(task.ID, "subdomain", "new.example", "new scope", "manual"); err != nil {
		t.Fatal(err)
	}
	if err = es.CommitGoalProof(t.Context(), snapshot, true, "planner", "stale scope"); err == nil {
		t.Fatal("scope drift accepted")
	}
}

func TestGoalProofMissingEvidenceRetainedAsLead(t *testing.T) {
	d, task, es, g, _ := goalProofFixture(t)
	f, err := es.RecordFinding(t.Context(), RecordFindingInput{TaskID: task.ID, ExplorationID: es.ID(), Summary: "upload/XSS", Severity: "high"})
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := es.GoalProofSnapshot(t.Context(), g, f.NodeID)
	if err != nil {
		t.Fatal(err)
	}
	if err = es.CommitGoalProof(t.Context(), snapshot, true, "planner", "finding exists"); err == nil {
		t.Fatal("empty evidence supports goal")
	}
	if _, err = d.GetFinding(f.FindingID); err != nil {
		t.Fatal("lead discarded", err)
	}
}

func TestProgressIgnoresRenamesAndRepeatedEvidence(t *testing.T) {
	d, task, es := completionFixture(t)
	_, err := es.AddNode(KindFact, map[string]any{"summary": "first wording", "evidence": "GET /me => 401"}, 5, "confirmed", "worker", nil)
	if err != nil {
		t.Fatal(err)
	}
	before, err := es.ProgressDigest(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		if _, err = es.AddIntent(map[string]any{"summary": "new planned wording"}, 5, nil, "planner"); err != nil {
			t.Fatal(err)
		}
		if _, err = es.AddNode(KindFact, map[string]any{"summary": "repeated wording", "evidence": "GET /me => 401"}, 5, "confirmed", "worker", nil); err != nil {
			t.Fatal(err)
		}
		after, err := es.ProgressDigest(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		if before != after {
			t.Fatal("copied evidence became progress")
		}
		rounds, err := d.RecordNoProgressRound(t.Context(), task.ID, before, after, false)
		if err != nil || rounds != i+1 {
			t.Fatalf("rounds=%d err=%v", rounds, err)
		}
	}
}

func TestEngagementReplacementIsAtomicAndReopensGoals(t *testing.T) {
	d, task, es, g, ev := goalProofFixture(t)
	snapshot, err := es.GoalProofSnapshot(t.Context(), g, ev)
	if err != nil {
		t.Fatal(err)
	}
	if err = es.CommitGoalProof(t.Context(), snapshot, true, "planner", "saved"); err != nil {
		t.Fatal(err)
	}
	oldWork, err := es.AddIntent(map[string]any{"summary": "inspect old port"}, 5, nil, "planner")
	if err != nil {
		t.Fatal(err)
	}
	change := EngagementChange{Goal: "inspect new port only", Description: "authorized local fixture", Reason: "operator changed port", Goals: []EngagementGoal{{Text: "inspect localhost:9443", GoalType: "inspection", RequiredChecks: []string{"new port"}}}, Constraints: []Constraint{{Kind: "deny", Text: "do not access old port"}}, Scopes: []ScopeInput{{Kind: "invalid", Value: "anything"}}}
	if err = d.SetPaused(task.ID, true); err != nil {
		t.Fatal(err)
	}
	if err = d.UpdateEngagement(t.Context(), task.ID, change); err == nil {
		t.Fatal("invalid scope committed")
	}
	n, _ := es.GetNode(g)
	if n == nil || n.State != "met" {
		t.Fatal("rollback lost old goals")
	}
	change.Scopes = []ScopeInput{{Kind: "ip", Value: "127.0.0.1"}}
	if err = d.UpdateEngagement(t.Context(), task.ID, change); err != nil {
		t.Fatal(err)
	}
	goals, err := es.ListByKind(KindGoal, 100)
	if err != nil || len(goals) != 1 || goals[0].State != "open" {
		t.Fatal("new contract retains old success", err)
	}
	var old json.RawMessage
	if err = d.QueryRow(`SELECT previous FROM task_engagement_audits WHERE task_id=$1`, task.ID).Scan(&old); err != nil || !strings.Contains(string(old), "verified_saved_result") && !strings.Contains(string(old), "goal_audit") {
		t.Fatal("old contract audit missing", err)
	}
	if fact, _ := es.GetNode(ev); fact == nil {
		t.Fatal("saved evidence lost")
	}
	oldNode, err := es.GetNode(oldWork)
	if err != nil || oldNode.State != "stopped" || !strings.Contains(string(oldNode.Payload), "retired_contract") {
		t.Fatal("old work not retired", err)
	}
	if reopened, err := es.ReopenIntent(oldWork); err != nil || reopened {
		t.Fatal("old authorization revived", err)
	}
}

func TestCheckRecoveryRequiresLatestCheckpoint(t *testing.T) {
	_, _, es := completionFixture(t)
	check := CheckSpec{Target: "local fixture", Mechanism: "authentication"}
	i1, err := es.AddCheckIntent(t.Context(), map[string]any{"summary": "first"}, 5, nil, "planner", check, 0, "")
	if err != nil {
		t.Fatal(err)
	}
	if err = es.SetIntentState(i1, "blocked"); err != nil {
		t.Fatal(err)
	}
	i2, err := es.AddCheckIntent(t.Context(), map[string]any{"summary": "continued"}, 5, nil, "planner", check, i1, "waited for reset")
	if err != nil {
		t.Fatal(err)
	}
	cp := Checkpoint{Result: "partial", Scope: []string{"login"}, Evidence: "candidate-1 denied", Gaps: []string{"candidate-2"}, Auth: &AuthCoverage{Request: "POST /login", Budget: 2, Candidates: []AuthCandidate{{ID: "c1", State: "failed"}, {ID: "c2", State: "remaining"}}}}
	if _, err = es.RecordCheckFact(t.Context(), map[string]any{"summary": "newer progress"}, i2, cp, "worker", nil); err != nil {
		t.Fatal(err)
	}
	if err = es.SetIntentState(i2, "blocked"); err != nil {
		t.Fatal(err)
	}
	if _, err = es.AddCheckIntent(t.Context(), map[string]any{"summary": "stale ancestor"}, 5, nil, "planner", check, i1, "another wait"); err == nil {
		t.Fatal("stale ancestor lost newer coverage")
	}
	i3, err := es.AddCheckIntent(t.Context(), map[string]any{"summary": "latest"}, 5, nil, "planner", check, i2, "another wait")
	if err != nil {
		t.Fatal(err)
	}
	n, _ := es.GetNode(i3)
	if !strings.Contains(string(n.Payload), "candidate-1 denied") {
		t.Fatal("latest progress not inherited")
	}
}

func TestFindingAssessmentTracksCurrentReviewInputs(t *testing.T) {
	d, task, es, goal, _ := goalProofFixture(t)
	f, err := es.RecordFinding(t.Context(), RecordFindingInput{TaskID: task.ID, ExplorationID: es.ID(), Summary: "fixture evidence", VulnClass: "fixture", Severity: "low", Evidence: "saved response"})
	if err != nil {
		t.Fatal(err)
	}
	assertAssessment := func(want string) {
		t.Helper()
		row, err := d.GetFinding(f.FindingID)
		if err != nil || row == nil || row.EvidenceAssessment != want {
			t.Fatalf("want %s, row=%+v err=%v", want, row, err)
		}
		meta, err := d.FindingMetaByNodeID(task.ID)
		if err != nil || meta[f.NodeID].EvidenceAssessment != want {
			t.Fatalf("task/global assessment differ: %+v %v", meta, err)
		}
	}
	assertAssessment("needs_review")
	snapshot, err := es.GoalProofSnapshot(t.Context(), goal, f.NodeID)
	if err != nil {
		t.Fatal(err)
	}
	if err = es.CommitGoalProof(t.Context(), snapshot, false, "planner", "browser mechanism missing"); err != nil {
		t.Fatal(err)
	}
	assertAssessment("unsupported_for_goal")
	if err = es.CommitGoalProof(t.Context(), snapshot, true, "planner", "saved response supports fixture goal"); err != nil {
		t.Fatal(err)
	}
	assertAssessment("supported_for_goal")
	if _, err = es.AddConstraint("deny", "new permitted scope", "human"); err != nil {
		t.Fatal(err)
	}
	assertAssessment("needs_review")
	snapshot, err = es.GoalProofSnapshot(t.Context(), goal, f.NodeID)
	if err != nil {
		t.Fatal(err)
	}
	if err = es.CommitGoalProof(t.Context(), snapshot, true, "planner", "new contract reviewed"); err != nil {
		t.Fatal(err)
	}
	assertAssessment("supported_for_goal")
	if _, err = d.Exec(`UPDATE findings SET evidence_version=evidence_version+1 WHERE id=$1`, f.FindingID); err != nil {
		t.Fatal(err)
	}
	assertAssessment("needs_review")
}
