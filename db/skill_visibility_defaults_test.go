package db

import (
	"fmt"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

func skillDefaultsDB(t *testing.T) *DB {
	t.Helper()
	d, err := Open(testDSN(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.Close() })
	return d
}

func skillDefaultsAgent(t *testing.T, d *DB, key string) *Agent {
	t.Helper()
	a, err := d.CreateAgent(key, "Skill defaults fixture", "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = d.Exec(`DELETE FROM settings WHERE key=$1`, skillVisibilityDefaultsKey(a.ID))
		_ = d.DeleteAgent(key)
	})
	return a
}

func assertAgentSkills(t *testing.T, d *DB, id int64, want []string) {
	t.Helper()
	got, err := d.AgentSkillNames(id)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != len(want) || len(got) > 0 && !reflect.DeepEqual(got, want) {
		t.Fatalf("visible skills=%v, want %v", got, want)
	}
}

func TestSkillDefaultsPreserveToggleOffAndExplicitEmptyAcrossInitialization(t *testing.T) {
	d := skillDefaultsDB(t)
	a := skillDefaultsAgent(t, d, fmt.Sprintf("skill_seed_%d", time.Now().UnixNano()))
	defaults := []string{"fixture-alpha", "fixture-beta"}
	if err := d.seedAgentSkillVisibility(a.Key, defaults); err != nil {
		t.Fatal(err)
	}
	assertAgentSkills(t, d, a.ID, defaults)
	if err := d.ToggleSkillVisibility(a.ID, "fixture-alpha", false); err != nil {
		t.Fatal(err)
	}
	if err := d.seedAgentSkillVisibility(a.Key, defaults); err != nil {
		t.Fatal(err)
	}
	assertAgentSkills(t, d, a.ID, []string{"fixture-beta"})
	var enabled bool
	if err := d.QueryRow(`SELECT enabled FROM agent_skill_visibility WHERE agent_id=$1 AND skill_name='fixture-alpha'`, a.ID).Scan(&enabled); err != nil || enabled {
		t.Fatalf("toggle-off tombstone: enabled=%v err=%v", enabled, err)
	}
	if err := d.SetAgentSkillVisibility(a.ID, nil); err != nil {
		t.Fatal(err)
	}
	// A separate connection observes the explicit empty choice as initialized;
	// changing the supplied defaults must not repopulate the selection either.
	other := skillDefaultsDB(t)
	if err := other.seedAgentSkillVisibility(a.Key, append(defaults, "fixture-new")); err != nil {
		t.Fatal(err)
	}
	assertAgentSkills(t, other, a.ID, nil)
}

func TestSkillDefaultsPreservePreexistingSelectionBeforeFirstMarker(t *testing.T) {
	d := skillDefaultsDB(t)
	a := skillDefaultsAgent(t, d, fmt.Sprintf("skill_existing_%d", time.Now().UnixNano()))
	if _, err := d.Exec(`INSERT INTO agent_skill_visibility(agent_id,skill_name,enabled) VALUES ($1,'user-choice',true),($1,'fixture-alpha',false)`, a.ID); err != nil {
		t.Fatal(err)
	}
	if err := d.seedAgentSkillVisibility(a.Key, []string{"fixture-alpha", "fixture-beta"}); err != nil {
		t.Fatal(err)
	}
	assertAgentSkills(t, d, a.ID, []string{"user-choice"})
	if _, found, err := d.GetSetting(skillVisibilityDefaultsKey(a.ID)); err != nil || !found {
		t.Fatalf("existing selection was not marked: found=%v err=%v", found, err)
	}
}

func TestSkillDefaultsRespectEmptyChoiceBeforeFirstSeed(t *testing.T) {
	d := skillDefaultsDB(t)
	a := skillDefaultsAgent(t, d, fmt.Sprintf("skill_empty_%d", time.Now().UnixNano()))
	if err := d.SetAgentSkillVisibility(a.ID, nil); err != nil {
		t.Fatal(err)
	}
	if err := d.seedAgentSkillVisibility(a.Key, []string{"fixture-alpha"}); err != nil {
		t.Fatal(err)
	}
	assertAgentSkills(t, d, a.ID, nil)
}

func TestSkillDefaultsMissingAuxiliaryAgentCanInitializeAfterCreation(t *testing.T) {
	d := skillDefaultsDB(t)
	key := fmt.Sprintf("skill_late_%d", time.Now().UnixNano())
	defaults := []string{"fixture-alpha", "fixture-beta"}
	if err := d.seedAgentSkillVisibility(key, defaults); err != nil {
		t.Fatal(err)
	}
	a := skillDefaultsAgent(t, d, key)
	if err := d.seedAgentSkillVisibility(key, defaults); err != nil {
		t.Fatal(err)
	}
	assertAgentSkills(t, d, a.ID, defaults)
	if err := d.seedAgentSkillVisibility(key, defaults); err != nil {
		t.Fatal(err)
	}
	assertAgentSkills(t, d, a.ID, defaults)
}

func TestSkillDefaultsFailedInitializationRollsBackMarkerAndBindings(t *testing.T) {
	d := skillDefaultsDB(t)
	a := skillDefaultsAgent(t, d, fmt.Sprintf("skill_fail_%d", time.Now().UnixNano()))
	function := fmt.Sprintf("skill_seed_fail_%d", a.ID)
	trigger := function + "_trigger"
	_, err := d.Exec(fmt.Sprintf(`CREATE FUNCTION %s() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN
IF NEW.agent_id=%d AND NEW.skill_name='fixture-beta' THEN RAISE EXCEPTION 'fixture Skill seed failure'; END IF;
RETURN NEW; END; $$; CREATE TRIGGER %s BEFORE INSERT ON agent_skill_visibility FOR EACH ROW EXECUTE FUNCTION %s()`, function, a.ID, trigger, function))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = d.Exec(fmt.Sprintf(`DROP TRIGGER IF EXISTS %s ON agent_skill_visibility; DROP FUNCTION IF EXISTS %s()`, trigger, function))
	})
	err = d.seedAgentSkillVisibility(a.Key, []string{"fixture-alpha", "fixture-beta"})
	if err == nil || !strings.Contains(err.Error(), "fixture Skill seed failure") {
		t.Fatalf("expected initialization failure, got %v", err)
	}
	assertAgentSkills(t, d, a.ID, nil)
	if _, found, err := d.GetSetting(skillVisibilityDefaultsKey(a.ID)); err != nil || found {
		t.Fatalf("failed initialization left completion marker: found=%v err=%v", found, err)
	}
}

func TestSkillDefaultsConcurrentSeedAndUserSelectionSerialize(t *testing.T) {
	d := skillDefaultsDB(t)
	a := skillDefaultsAgent(t, d, fmt.Sprintf("skill_concurrent_%d", time.Now().UnixNano()))
	start := make(chan struct{})
	errs := make(chan error, 2)
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		<-start
		errs <- d.seedAgentSkillVisibility(a.Key, []string{"fixture-alpha", "fixture-beta"})
	}()
	go func() {
		defer wg.Done()
		<-start
		errs <- d.SetAgentSkillVisibility(a.ID, []string{"user-choice"})
	}()
	close(start)
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	assertAgentSkills(t, d, a.ID, []string{"user-choice"})
}
