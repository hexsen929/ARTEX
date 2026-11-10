package db

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

func TestVerificationHandoffLegacyRepairPreservesEvidenceAndGeneration(t *testing.T) {
	d := memoryFixtureDB(t)
	task := memoryFixtureTask(t, d)
	s := d.Exploration(task.ExplorationID)
	for _, scenario := range []string{"valid", "lookup_rejected", "lookup_mismatch", "already_updated", "missing_result", "foreign_token", "pending_write", "unknown_field", "different_finding", "old_result", "second_run", "incomplete", "approval", "earlier_permission"} {
		t.Run(scenario, func(t *testing.T) {
			f, err := s.RecordFinding(t.Context(), RecordFindingInput{TaskID: task.ID, Summary: "synthetic lead", Severity: "low"})
			if err != nil {
				t.Fatal(err)
			}
			id, err := s.AddIntent(map[string]any{"summary": "fixture"}, 5, nil, "fixture")
			if err != nil {
				t.Fatal(err)
			}
			if ok, err := s.ClaimIntent(id, "fixture"); !ok || err != nil {
				t.Fatal(ok, err)
			}
			token, err := s.BindIntentRun(t.Context(), id)
			if err != nil {
				t.Fatal(err)
			}
			if scenario == "earlier_permission" {
				meta, _ := json.Marshal(map[string]string{"worker_run_token": token})
				for _, kind := range []string{"tool_use", "tool_result"} {
					if _, err := s.AppendActivity(Activity{NodeID: &id, Kind: kind, Tool: "set_finding_verification", ToolUseID: "earlier-denied", IsError: kind == "tool_result", Detail: "Tool call was not permitted", Metadata: meta}); err != nil {
						t.Fatal(err)
					}
				}
			}
			if scenario == "lookup_rejected" || scenario == "lookup_mismatch" {
				meta, _ := json.Marshal(map[string]string{"worker_run_token": token})
				input := `{"tool_name":"missing-local-tool"}`
				output := "Error: unknown tool missing-local-tool"
				if scenario == "lookup_mismatch" {
					output = "Error: unknown tool another-name"
				}
				for _, kind := range []string{"tool_use", "tool_result"} {
					detail := input
					if kind == "tool_result" {
						detail = output
					}
					if _, err := s.AppendActivity(Activity{NodeID: &id, Kind: kind, Tool: "ExecuteExtraTool", ToolUseID: "earlier-lookup", IsError: kind == "tool_result", Detail: detail, Metadata: meta}); err != nil {
						t.Fatal(err)
					}
				}
			}
			for i := range 3 {
				findingID := f.FindingID
				if scenario == "different_finding" && i == 1 {
					findingID++
				}
				meta, _ := json.Marshal(map[string]string{"worker_run_token": token})
				input := fmt.Sprintf(`{"finding_id":%d,"status":"confirmed","version":0,"note":"fixture"}`, findingID)
				useID := fmt.Sprint(i)
				if _, err := s.AppendActivity(Activity{NodeID: &id, Kind: "tool_use", Tool: "set_finding_verification", ToolUseID: useID, Detail: input, Metadata: meta}); err != nil {
					t.Fatal(err)
				}
				output := `{"error":"verification_missing_fields","missing_fields":["additional_evidence"]}`
				if scenario == "unknown_field" {
					output = `{"error":"verification_missing_fields","missing_fields":["external_unknown"]}`
				}
				if scenario == "approval" {
					output = `approval_wait`
				}
				if scenario == "foreign_token" && i == 2 {
					meta = json.RawMessage(`{"worker_run_token":"foreign"}`)
				}
				if scenario != "missing_result" || i != 2 {
					if _, err := s.AppendActivity(Activity{NodeID: &id, Kind: "tool_result", Tool: "set_finding_verification", ToolUseID: useID, IsError: true, Detail: output, Metadata: meta}); err != nil {
						t.Fatal(err)
					}
				}
			}
			if scenario == "pending_write" {
				meta, _ := json.Marshal(map[string]string{"worker_run_token": token})
				if _, err := s.AppendActivity(Activity{NodeID: &id, Kind: "tool_use", Tool: "Bash", ToolUseID: "pending-write", Metadata: meta}); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := s.AppendActivity(Activity{NodeID: &id, Kind: "result", IsError: true, Detail: LegacyVerificationGuardMessage}); err != nil {
				t.Fatal(err)
			}
			if err := s.SetIntentRunOutcome(t.Context(), id, token, "blocked", "tool_loop_guard", "fixture", scenario == "incomplete"); err != nil {
				t.Fatal(err)
			}
			if scenario == "old_result" {
				if _, err := d.Exec(`UPDATE activity SET created_at=created_at-interval '1 hour' WHERE node_id=$1`, id); err != nil {
					t.Fatal(err)
				}
			}
			if scenario == "second_run" {
				if _, err := d.Exec(`UPDATE exploration_nodes SET payload=jsonb_set(payload,'{execution_attempt}','2'::jsonb) WHERE id=$1`, id); err != nil {
					t.Fatal(err)
				}
			}
			if scenario == "already_updated" {
				if err := d.SetFindingVerification(t.Context(), f.FindingID, VerificationConfirmed, "later independent fixture evidence", 0, "actual saved fixture output"); err != nil {
					t.Fatal(err)
				}
			}
			before, _ := s.GetNode(id)
			var evidenceBefore, activityBefore string
			if err := d.QueryRow(`SELECT md5(to_jsonb(f)::text) FROM findings f WHERE id=$1`, f.FindingID).Scan(&evidenceBefore); err != nil {
				t.Fatal(err)
			}
			if err := d.QueryRow(`SELECT md5(string_agg(to_jsonb(a)::text,',' ORDER BY id)) FROM activity a WHERE node_id=$1`, id).Scan(&activityBefore); err != nil {
				t.Fatal(err)
			}
			n, err := s.ReclassifyLegacyToolStops(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			after, _ := s.GetNode(id)
			eligible := scenario == "valid" || scenario == "already_updated" || scenario == "lookup_rejected"
			if !eligible {
				if n != 0 || after.State != before.State || string(after.Payload) != string(before.Payload) {
					t.Fatal("ambiguous record reclassified", n)
				}
				return
			}
			if n != 1 || after.State != "exhausted" || !strings.Contains(string(after.Payload), VerificationHandoffCode) || !strings.Contains(string(after.Payload), "additional_evidence") {
				t.Fatal("missing explicit handoff", n, after.State)
			}
			if continued, err := s.ContinuePlannerIntent(id, "review current saved record", 8); err != nil || continued != id {
				t.Fatal("handoff did not preserve original id", continued, err)
			}
			final, _ := s.GetNode(id)
			if final.State != "exhausted" || string(final.Payload) != string(after.Payload) {
				t.Fatal("handoff replayed the Worker")
			}
			var evidenceAfter, activityAfter string
			d.QueryRow(`SELECT md5(to_jsonb(f)::text) FROM findings f WHERE id=$1`, f.FindingID).Scan(&evidenceAfter)
			d.QueryRow(`SELECT md5(string_agg(to_jsonb(a)::text,',' ORDER BY id)) FROM activity a WHERE node_id=$1`, id).Scan(&activityAfter)
			if evidenceAfter != evidenceBefore || activityAfter != activityBefore {
				t.Fatal("repair changed finding or historical activity")
			}
			if n, err := s.ReclassifyLegacyToolStops(t.Context()); err != nil || n != 0 {
				t.Fatal("repair not idempotent", n, err)
			}
		})
	}
}
