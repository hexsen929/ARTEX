package db

import (
	"encoding/json"
	"testing"
)

func TestReadBudgetLegacyStopReclassifiedWithoutExecuting(t *testing.T) {
	d := memoryFixtureDB(t)
	task := memoryFixtureTask(t, d)
	s := d.Exploration(task.ExplorationID)
	for _, scenario := range []string{"read", "pending_read", "pending_write", "contract", "authentication", "incomplete", "old_result", "foreign_token", "missing_token_second_run"} {
		t.Run(scenario, func(t *testing.T) {
			id, err := s.AddIntent(map[string]any{"summary": "fixture"}, 5, nil, "fixture")
			if err != nil {
				t.Fatal(err)
			}
			if ok, err := s.ClaimIntent(id, "fixture"); err != nil || !ok {
				t.Fatal(ok, err)
			}
			token, err := s.BindIntentRun(t.Context(), id)
			if err != nil {
				t.Fatal(err)
			}
			if scenario == "pending_read" || scenario == "pending_write" {
				tool := "traffic_search"
				if scenario == "pending_write" {
					tool = "Bash"
				}
				meta, _ := json.Marshal(map[string]string{"worker_run_token": token})
				if _, err := s.AppendActivity(Activity{NodeID: &id, Kind: "tool_use", Tool: tool, ToolUseID: "pending-fixture", Metadata: meta}); err != nil {
					t.Fatal(err)
				}
			}
			result := LegacyTrafficReadBudgetMessage
			if scenario == "contract" {
				result = "tool_loop_guard: repeated missing fields"
			}
			var resultMetadata json.RawMessage
			if scenario == "foreign_token" {
				resultMetadata = json.RawMessage(`{"worker_run_token":"different-generation"}`)
			}
			if _, err := s.AppendActivity(Activity{NodeID: &id, Kind: "result", IsError: true, Summary: result, Detail: result, Metadata: resultMetadata}); err != nil {
				t.Fatal(err)
			}
			code := "tool_loop_guard"
			if scenario == "authentication" {
				code = "authentication"
			}
			if err = s.SetIntentRunOutcome(t.Context(), id, token, "blocked", code, "fixture", scenario == "incomplete"); err != nil {
				t.Fatal(err)
			}
			if scenario == "old_result" {
				if _, err := d.Exec(`UPDATE activity SET created_at=created_at-interval '1 hour' WHERE node_id=$1`, id); err != nil {
					t.Fatal(err)
				}
			}
			if scenario == "missing_token_second_run" {
				if _, err := d.Exec(`UPDATE exploration_nodes SET payload=jsonb_set(payload,'{execution_attempt}','2'::jsonb) WHERE id=$1`, id); err != nil {
					t.Fatal(err)
				}
			}
			before, _ := s.GetNode(id)
			n, err := s.ReclassifyLegacyReadBudgetStops(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			after, _ := s.GetNode(id)
			if scenario != "read" && scenario != "pending_read" {
				if n != 0 || after.State != before.State || string(after.Payload) != string(before.Payload) {
					t.Fatal("non-read or incomplete work was rewritten", n)
				}
				return
			}
			var payload map[string]any
			json.Unmarshal(after.Payload, &payload)
			if n != 1 || after.State != "exhausted" || payload["worker_result"].(map[string]any)["code"] != "traffic_read_budget" || payload["execution_attempt"] != float64(1) {
				t.Fatal(n, after.State, payload)
			}
			continued, err := s.ContinuePlannerIntent(id, "review saved fixture", 8)
			if err != nil || continued != id {
				t.Fatal(continued, err)
			}
			final, _ := s.GetNode(id)
			if final.State != "exhausted" || string(final.Payload) != string(after.Payload) {
				t.Fatal("read handoff replayed worker")
			}
			if n, err = s.ReclassifyLegacyReadBudgetStops(t.Context()); err != nil || n != 0 {
				t.Fatal("repair not idempotent", n, err)
			}
			var stored string
			if err = d.QueryRow(`SELECT detail FROM activity WHERE node_id=$1 AND kind='result'`, id).Scan(&stored); err != nil || stored != result {
				t.Fatal("historical result changed", err)
			}
		})
	}
}
