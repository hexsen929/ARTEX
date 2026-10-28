package server

import (
	"database/sql"
	"testing"

	"github.com/Autumn-27/artex/db"
)

// Temporary tables shadow only these four seed relations on this transaction's
// pinned connection. Tests never delete or rewrite the real seeded Agent rows.
func verifierSeedFixtureTx(t *testing.T, rejectTrigger bool) *sql.Tx {
	t.Helper()
	pg := openIntranetTestDB(t)
	tx, err := pg.Begin()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = tx.Rollback() })
	triggerConstraint := ""
	if rejectTrigger {
		triggerConstraint = " CHECK (NOT on_finding)"
	}
	_, err = tx.Exec(`CREATE TEMP TABLE agents (
 id BIGSERIAL PRIMARY KEY, key TEXT NOT NULL UNIQUE, name TEXT, description TEXT,
 role TEXT, builtin BOOLEAN, enabled BOOLEAN, current_prompt_id BIGINT
) ON COMMIT DROP;
CREATE TEMP TABLE agent_prompts (
 id BIGSERIAL PRIMARY KEY, agent_id BIGINT, version INTEGER, template_text TEXT,
 note TEXT, updated_by TEXT
) ON COMMIT DROP;
CREATE TEMP TABLE settings (key TEXT PRIMARY KEY, value TEXT NOT NULL) ON COMMIT DROP;
CREATE TEMP TABLE agent_triggers (
 id BIGSERIAL PRIMARY KEY, agent_key TEXT NOT NULL, enabled BOOLEAN,
 on_finding BOOLEAN` + triggerConstraint + `
) ON COMMIT DROP;`)
	if err != nil {
		t.Fatal(err)
	}
	return tx
}

func verifierSeedFixtureCounts(t *testing.T, tx *sql.Tx) (agents, prompts, triggers, flags int) {
	t.Helper()
	if err := tx.QueryRow(`SELECT
 (SELECT count(*) FROM agents), (SELECT count(*) FROM agent_prompts),
 (SELECT count(*) FROM agent_triggers), (SELECT count(*) FROM settings)`).Scan(&agents, &prompts, &triggers, &flags); err != nil {
		t.Fatal(err)
	}
	return
}

func TestFindingVerifierSeedIsAtomicAndDoesNotReviveUserControls(t *testing.T) {
	t.Run("fresh seed is idempotent and deletions stay deleted", func(t *testing.T) {
		tx := verifierSeedFixtureTx(t, false)
		if err := seedFindingVerifierRows(tx); err != nil {
			t.Fatal(err)
		}
		if err := seedFindingVerifierRows(tx); err != nil {
			t.Fatal(err)
		}
		a, p, tr, flags := verifierSeedFixtureCounts(t, tx)
		if a != 1 || p != 1 || tr != 1 || flags != 1 {
			t.Fatalf("first-install seed is incomplete/duplicated: %d/%d/%d/%d", a, p, tr, flags)
		}
		if _, err := tx.Exec(`UPDATE agents SET enabled=false;
UPDATE agent_triggers SET enabled=false`); err != nil {
			t.Fatal(err)
		}
		if err := seedFindingVerifierRows(tx); err != nil {
			t.Fatal(err)
		}
		var active bool
		if err := tx.QueryRow(`SELECT EXISTS(SELECT 1 FROM agents WHERE enabled)
 OR EXISTS(SELECT 1 FROM agent_triggers WHERE enabled)`).Scan(&active); err != nil || active {
			t.Fatal("seed revived a disabled Agent or trigger", err)
		}
		if _, err := tx.Exec(`DELETE FROM agent_triggers`); err != nil {
			t.Fatal(err)
		}
		if err := seedFindingVerifierRows(tx); err != nil {
			t.Fatal(err)
		}
		_, _, tr, _ = verifierSeedFixtureCounts(t, tx)
		if tr != 0 {
			t.Fatal("seed recreated a deleted automatic trigger")
		}
		if _, err := tx.Exec(`DELETE FROM agent_prompts; DELETE FROM agents`); err != nil {
			t.Fatal(err)
		}
		if err := seedFindingVerifierRows(tx); err != nil {
			t.Fatal(err)
		}
		a, p, tr, flags = verifierSeedFixtureCounts(t, tx)
		if a != 0 || p != 0 || tr != 0 || flags != 1 {
			t.Fatal("completed migration recreated a deleted Agent or history")
		}
	})

	t.Run("pre-existing custom Agent and disabled trigger are preserved", func(t *testing.T) {
		tx := verifierSeedFixtureTx(t, false)
		if _, err := tx.Exec(`INSERT INTO agents(id,key,name,enabled,current_prompt_id)
VALUES(42,'verifier','custom verifier',false,84);
INSERT INTO agent_prompts(id,agent_id,version,template_text) VALUES(84,42,7,'CUSTOM VERIFIER BODY');
INSERT INTO agent_triggers(agent_key,enabled,on_finding) VALUES('verifier',false,true)`); err != nil {
			t.Fatal(err)
		}
		if err := seedFindingVerifierRows(tx); err != nil {
			t.Fatal(err)
		}
		var name, body string
		var enabled bool
		var version int
		if err := tx.QueryRow(`SELECT a.name,a.enabled,p.version,p.template_text
FROM agents a JOIN agent_prompts p ON p.id=a.current_prompt_id WHERE a.key='verifier'`).Scan(&name, &enabled, &version, &body); err != nil {
			t.Fatal(err)
		}
		if name != "custom verifier" || enabled || version != 7 || body != "CUSTOM VERIFIER BODY" {
			t.Fatal("seed changed the pre-existing Agent or prompt")
		}
		a, p, tr, flags := verifierSeedFixtureCounts(t, tx)
		if a != 1 || p != 1 || tr != 1 || flags != 1 {
			t.Fatal("pre-existing rows were duplicated")
		}
		if err := tx.QueryRow(`SELECT enabled FROM agent_triggers`).Scan(&enabled); err != nil || enabled {
			t.Fatal("pre-existing disabled trigger was changed", err)
		}
	})

	t.Run("pre-existing Agent without trigger does not silently opt in", func(t *testing.T) {
		tx := verifierSeedFixtureTx(t, false)
		if _, err := tx.Exec(`INSERT INTO agents(key,name,enabled) VALUES('verifier','custom without automatic work',true)`); err != nil {
			t.Fatal(err)
		}
		if err := seedFindingVerifierRows(tx); err != nil {
			t.Fatal(err)
		}
		a, p, tr, flags := verifierSeedFixtureCounts(t, tx)
		if a != 1 || p != 0 || tr != 0 || flags != 1 {
			t.Fatal("seed added a default prompt or automatic trigger to an existing key")
		}
	})

	t.Run("trigger insert failure rolls back all seed rows", func(t *testing.T) {
		tx := verifierSeedFixtureTx(t, true)
		if _, err := tx.Exec(`SAVEPOINT before_verifier_seed`); err != nil {
			t.Fatal(err)
		}
		if err := seedFindingVerifierRows(tx); err == nil {
			t.Fatal("failing trigger constraint did not fail the seed")
		}
		if _, err := tx.Exec(`ROLLBACK TO SAVEPOINT before_verifier_seed`); err != nil {
			t.Fatal(err)
		}
		a, p, tr, flags := verifierSeedFixtureCounts(t, tx)
		if a != 0 || p != 0 || tr != 0 || flags != 0 {
			t.Fatalf("failed seed left partial state: %d/%d/%d/%d", a, p, tr, flags)
		}
	})
}

func disableAutomaticVerifierTriggersForFixture(t *testing.T, f *findingCheckPauseFixture) int64 {
	t.Helper()
	pg := f.s.m.pg
	rows, err := pg.Query(`SELECT id,enabled FROM agent_triggers WHERE agent_key=$1`, db.FindingVerifierAgentKey)
	if err != nil {
		t.Fatal(err)
	}
	prior := map[int64]bool{}
	for rows.Next() {
		var id int64
		var enabled bool
		if err := rows.Scan(&id, &enabled); err != nil {
			rows.Close()
			t.Fatal(err)
		}
		prior[id] = enabled
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		t.Fatal(err)
	}
	rows.Close()
	if _, err := pg.Exec(`UPDATE agent_triggers SET enabled=false WHERE agent_key=$1`, db.FindingVerifierAgentKey); err != nil {
		t.Fatal(err)
	}
	trigger, err := pg.CreateTrigger(&db.AgentTrigger{AgentKey: db.FindingVerifierAgentKey, Enabled: true, OnFinding: true})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = pg.DeleteTrigger(trigger.ID)
		for id, enabled := range prior {
			_, _ = pg.Exec(`UPDATE agent_triggers SET enabled=$2 WHERE id=$1`, id, enabled)
		}
	})
	return trigger.ID
}

func TestAutomaticFindingVerificationRespectsAllOperatorSwitches(t *testing.T) {
	f := newFindingCheckPauseFixture(t)
	triggerID := disableAutomaticVerifierTriggersForFixture(t, f)
	for _, tc := range []struct {
		name, mode                     string
		agentEnabled, enabled, finding bool
	}{
		{name: "global mode off", mode: "off", agentEnabled: true, enabled: true, finding: true},
		{name: "trigger disabled", mode: "auto", agentEnabled: true, enabled: false, finding: true},
		{name: "finding trigger removed", mode: "auto", agentEnabled: true, enabled: true, finding: false},
		{name: "Agent disabled", mode: "auto", agentEnabled: false, enabled: true, finding: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := f.s.m.pg.SetSetting(settingFindingVerifyMode, tc.mode); err != nil {
				t.Fatal(err)
			}
			if _, err := f.s.m.pg.Exec(`UPDATE agents SET enabled=$2 WHERE key=$1`, db.FindingVerifierAgentKey, tc.agentEnabled); err != nil {
				t.Fatal(err)
			}
			if _, err := f.s.m.pg.Exec(`UPDATE agent_triggers SET enabled=$2,on_finding=$3 WHERE id=$1`, triggerID, tc.enabled, tc.finding); err != nil {
				t.Fatal(err)
			}
			check, created, err := f.s.requestAutomaticFindingCheck(f.finding, db.FindingCheckKindVerify)
			if err != nil || check != nil || created {
				t.Fatal("disabled automatic control admitted a verification", err)
			}
			f.s.onFindingMerged(f.finding)
			f.s.dispatchFindingVerify(db.TaskEvent{NodeID: f.node})
			checks, err := f.s.m.pg.ListFindingChecks(f.finding)
			if err != nil || len(checks) != 0 || f.calls.Load() != 0 {
				t.Fatal("an automatic entry point bypassed an operator switch", err)
			}
		})
	}
}

func TestManualFindingVerificationRemainsExplicitWithAutomaticControlsOff(t *testing.T) {
	f := newFindingCheckPauseFixture(t)
	triggerID := disableAutomaticVerifierTriggersForFixture(t, f)
	if err := f.s.m.pg.SetSetting(settingFindingVerifyMode, "off"); err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.m.pg.Exec(`UPDATE agent_triggers SET enabled=false WHERE id=$1`, triggerID); err != nil {
		t.Fatal(err)
	}
	f.pauseCommitted(t)
	check, created, err := f.s.RequestFindingCheck(f.finding, db.FindingCheckKindVerify)
	if err != nil || !created || check == nil {
		t.Fatal("explicit manual verification was blocked by automatic controls", err)
	}
	f.waitProvider(t)
	f.pauseCommitted(t)
	waitRetestIdle(t, f.s)
}

func TestAutomaticFindingVerificationFailsClosedWhenMetadataIsUnavailable(t *testing.T) {
	pg := openIntranetTestDB(t)
	if err := pg.Close(); err != nil {
		t.Fatal(err)
	}
	s := &Server{m: &Manager{pg: pg}}
	enabled, err := s.automaticFindingVerificationEnabled()
	if err == nil || enabled {
		t.Fatal("unreadable metadata enabled automatic verification")
	}
}
