package db

import (
	"encoding/json"
	"fmt"
	"sync"
	"testing"
	"time"
)

func automaticRecoveryFixture(t *testing.T) (*DB, *ExplorationStore) {
	t.Helper()
	d, err := Open(testDSN(t))
	if err != nil {
		t.Fatal(err)
	}
	task, err := d.CreateTask("automatic recovery fixture", "fixture", nil, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.DeleteTask(task.ID); _ = d.Close() })
	return d, d.Exploration(task.ExplorationID)
}

func TestAutomaticRecoveryCheckpointDecisionAndPreservation(t *testing.T) {
	for _, scenario := range []string{"zero_tool", "complete_tool", "empty_bash_eof", "pending_command", "pending_other", "incomplete", "token_mismatch", "prior_pending", "approval", "quota", "auth", "retry_limit", "backoff", "paused", "terminal_task", "queued", "disabled", "schema_override", "unknown"} {
		t.Run(scenario, func(t *testing.T) {
			d, s := automaticRecoveryFixture(t)
			id, err := s.AddIntent(map[string]any{"summary": "fixture", "stable_marker": "preserved"}, 5, nil, "fixture")
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
			event := func(kind, name, detail string, failed bool) {
				meta, _ := json.Marshal(map[string]any{"worker_run_token": token})
				if _, err := s.AppendActivity(Activity{NodeID: &id, Kind: kind, Tool: name, ToolUseID: "probe", Detail: detail, IsError: failed, Metadata: meta}); err != nil {
					t.Fatal(err)
				}
			}
			switch scenario {
			case "complete_tool":
				event("tool_use", "Read", `{"file_path":"fixture"}`, false)
				event("tool_result", "Read", "fixture result", false)
			case "empty_bash_eof", "schema_override":
				event("tool_use", "Bash", `{}`, false)
			case "pending_command":
				event("tool_use", "Bash", `{"command":"write fixture"}`, false)
			case "pending_other":
				event("tool_use", "Write", `{}`, false)
			case "approval":
				event("tool_use", "Read", `{}`, false)
				event("tool_result", "Read", "Blocked by hook: approval_wait: fixture", true)
			}
			if _, err := s.AppendActivity(Activity{NodeID: &id, Kind: "result", Detail: "terminal model_error; error `unexpected EOF`", IsError: true}); err != nil {
				t.Fatal(err)
			}
			code := "transport_failure"
			if scenario == "quota" {
				code = "quota_exhausted"
			}
			if scenario == "auth" {
				code = "authentication"
			}
			if scenario == "unknown" {
				code = "unknown_failure"
			}
			if err = s.SetIntentRunOutcome(t.Context(), id, token, "blocked", code, "fixture original error", scenario == "incomplete"); err != nil {
				t.Fatal(err)
			}
			switch scenario {
			case "token_mismatch":
				_, err = d.Exec(`UPDATE exploration_nodes SET payload=jsonb_set(payload,'{execution_checkpoint,run_token}','"stale"') WHERE id=$1`, id)
			case "prior_pending":
				_, err = d.Exec(`UPDATE exploration_nodes SET payload=payload||'{"previous_checkpoint":{"pending":{"old":"Write"}}}'::jsonb WHERE id=$1`, id)
			case "retry_limit":
				_, err = d.Exec(`UPDATE exploration_nodes SET payload=payload||'{"automatic_model_recovery":{"attempts":2,"limit":2}}'::jsonb WHERE id=$1`, id)
			case "schema_override":
				_, err = d.Exec(`INSERT INTO tools(key) VALUES('Bash')`)
				t.Cleanup(func() { _, _ = d.Exec(`DELETE FROM tools WHERE key='Bash'`) })
			case "queued":
				_, err = d.Exec(`UPDATE tasks SET queued=true WHERE exploration_id=$1`, s.ID())
			case "paused":
				_, err = d.Exec(`UPDATE tasks SET paused=true WHERE exploration_id=$1`, s.ID())
			case "terminal_task":
				_, err = d.Exec(`UPDATE tasks SET status='done' WHERE exploration_id=$1`, s.ID())
			}
			if err != nil {
				t.Fatal(err)
			}
			when := time.Now().Add(2 * time.Minute)
			if scenario == "backoff" {
				when = time.Now()
			}
			retryLimit := 2
			if scenario == "disabled" {
				retryLimit = 0
			}
			ids, err := s.RecoverTransientIntents(t.Context(), when, retryLimit, time.Second)
			if err != nil {
				t.Fatal(err)
			}
			want := scenario == "zero_tool" || scenario == "complete_tool" || scenario == "empty_bash_eof"
			if (len(ids) == 1) != want {
				t.Fatalf("recovered %v, want recovery=%v", ids, want)
			}
			n, err := s.GetNode(id)
			if err != nil {
				t.Fatal(err)
			}
			var p recoveryPayload
			if err = json.Unmarshal(n.Payload, &p); err != nil {
				t.Fatal(err)
			}
			if want {
				if n.State != "open" || ids[0] != id || p.Recovery == nil || p.Recovery.Attempts != 1 || p.Result.Code != code || p.Result.Message != "fixture original error" || p.Token != token {
					t.Fatalf("lost original state: %s", n.Payload)
				}
				if scenario == "empty_bash_eof" && (len(p.Checkpoint.NotExecuted) != 1 || p.Checkpoint.ToolCalls != 1 || p.Checkpoint.ToolResults != 0 || len(p.Checkpoint.Pending) != 0) {
					t.Fatalf("empty call was fabricated as execution: %+v", p.Checkpoint)
				}
				again, err := s.RecoverTransientIntents(t.Context(), when, 2, time.Second)
				if err != nil || len(again) != 0 {
					t.Fatal("duplicate recovery", again, err)
				}
			} else if n.State != "blocked" {
				t.Fatalf("unsafe intent revived: %s", n.Payload)
			}
			var marker string
			if err = d.QueryRow(`SELECT payload->>'stable_marker' FROM exploration_nodes WHERE id=$1`, id).Scan(&marker); err != nil || marker != "preserved" {
				t.Fatal(marker, err)
			}
		})
	}
}

func TestAutomaticRecoveryConcurrencyAndDurableLimit(t *testing.T) {
	_, s := automaticRecoveryFixture(t)
	id, err := s.AddIntent(map[string]any{"summary": "fixture"}, 5, nil, "fixture")
	if err != nil {
		t.Fatal(err)
	}
	fail := func() {
		t.Helper()
		if ok, err := s.ClaimIntent(id, "worker"); err != nil || !ok {
			t.Fatal(ok, err)
		}
		token, err := s.BindIntentRun(t.Context(), id)
		if err != nil {
			t.Fatal(err)
		}
		if err = s.SetIntentOutcome(id, "blocked", "transport_failure", "fixture", token); err != nil {
			t.Fatal(err)
		}
	}
	for attempt := 1; attempt <= 2; attempt++ {
		fail()
		var wg sync.WaitGroup
		out := make(chan []int64, 8)
		for range 8 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				ids, err := s.RecoverTransientIntents(t.Context(), time.Now().Add(5*time.Minute), 2, time.Second)
				if err != nil {
					t.Error(err)
				}
				out <- ids
			}()
		}
		wg.Wait()
		close(out)
		count := 0
		for ids := range out {
			count += len(ids)
		}
		n, _ := s.GetNode(id)
		var p recoveryPayload
		_ = json.Unmarshal(n.Payload, &p)
		if count != 1 || p.Recovery == nil || p.Recovery.Attempts != attempt {
			t.Fatalf("duplicate/lost durable retry: count=%d p=%+v", count, p.Recovery)
		}
	}
	fail()
	ids, err := s.RecoverTransientIntents(t.Context(), time.Now().Add(time.Hour), 2, time.Second)
	if err != nil || len(ids) != 0 {
		t.Fatal("retry limit reset", ids, err)
	}
}

func TestAutomaticRecoveryRollbackDoesNotPublishOrEraseEvidence(t *testing.T) {
	d, s := automaticRecoveryFixture(t)
	id, err := s.AddIntent(map[string]any{"summary": "fixture"}, 5, nil, "fixture")
	if err != nil {
		t.Fatal(err)
	}
	_, _ = s.ClaimIntent(id, "worker")
	token, err := s.BindIntentRun(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.SetIntentOutcome(id, "blocked", "transport_failure", "fixture", token); err != nil {
		t.Fatal(err)
	}
	name := fmt.Sprintf("reject_recovery_%d", time.Now().UnixNano())
	_, err = d.Exec(`CREATE FUNCTION ` + name + `() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN IF NEW.id=` + fmt.Sprint(id) + ` AND NEW.state='open' THEN RAISE EXCEPTION 'fixture'; END IF; RETURN NEW; END $$`)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = d.Exec(`DROP FUNCTION IF EXISTS ` + name + `() CASCADE`) })
	_, err = d.Exec(`CREATE TRIGGER ` + name + ` BEFORE UPDATE ON exploration_nodes FOR EACH ROW EXECUTE FUNCTION ` + name + `() `)
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.RecoverTransientIntents(t.Context(), time.Now().Add(time.Hour), 2, time.Second)
	if err == nil {
		t.Fatal("write rejection accepted")
	}
	n, _ := s.GetNode(id)
	var p recoveryPayload
	_ = json.Unmarshal(n.Payload, &p)
	if n.State != "blocked" || p.Recovery != nil {
		t.Fatal("failed recovery changed intent")
	}
}
