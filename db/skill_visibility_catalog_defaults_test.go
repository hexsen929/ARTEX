package db

import (
	"strings"
	"testing"
)

func catalogSkillDefaultsDB(t *testing.T) *DB {
	t.Helper()
	d, err := Open(upstreamMergeFixtureDSN(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.Close() })
	return d
}

func catalogSkillRole(t *testing.T, d *DB, key string) *Agent {
	t.Helper()
	a, err := d.GetAgentByKey(key)
	if err != nil || a == nil {
		t.Fatalf("missing fixture role %s: %v", key, err)
	}
	return a
}

func TestCatalogSkillDefaultsDBOpenDoesNotGrantUnknownFiles(t *testing.T) {
	d := catalogSkillDefaultsDB(t)
	worker := catalogSkillRole(t, d, "worker")
	var rows, markers int
	if err := d.QueryRow(`SELECT count(*) FROM agent_skill_visibility`).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if err := d.QueryRow(`SELECT count(*) FROM settings WHERE key LIKE 'skill_visibility_initialized_v1:%'`).Scan(&markers); err != nil {
		t.Fatal(err)
	}
	if rows != 0 || markers != 0 {
		t.Fatalf("DB-only startup initialized filesystem defaults: rows=%d markers=%d", rows, markers)
	}
	if err := d.SeedBuiltinSkillVisibility(nil); err == nil {
		t.Fatal("unknown catalog must not become an explicit empty selection")
	}
	if configured, err := d.BuiltinSkillDefaultsConfigured(worker.ID, worker.Key); err != nil || configured {
		t.Fatalf("unknown defaults marked configured: %v %v", configured, err)
	}
	if err := d.SeedBuiltinSkillVisibility([]string{"api-recon", "../invalid"}); err == nil {
		t.Fatal("invalid catalog accepted")
	}
	assertAgentSkills(t, d, worker.ID, nil)
	if _, found, err := d.GetSetting(skillVisibilityDefaultsKey(worker.ID)); err != nil || found {
		t.Fatalf("failed catalog left a completion marker: %v %v", found, err)
	}
}

func TestCatalogSkillDefaultsGrantOnlyInstalledPolicyIntersection(t *testing.T) {
	d := catalogSkillDefaultsDB(t)
	worker := catalogSkillRole(t, d, "worker")
	if err := d.SeedBuiltinSkillVisibility([]string{"chains-web", "api-recon", "installed-but-not-default"}); err != nil {
		t.Fatal(err)
	}
	assertAgentSkills(t, d, worker.ID, []string{"api-recon", "chains-web"})
	if configured, err := d.BuiltinSkillDefaultsConfigured(worker.ID, worker.Key); err != nil || !configured {
		t.Fatalf("inspected defaults not configured: %v %v", configured, err)
	}
	var extra int
	if err := d.QueryRow(`SELECT count(*) FROM agent_skill_visibility WHERE skill_name NOT IN('api-recon','chains-web')`).Scan(&extra); err != nil || extra != 0 {
		t.Fatalf("uninstalled or non-policy Skill received authorization: %d %v", extra, err)
	}
	// Later files/default-policy changes never silently expand an existing role.
	if err := d.ToggleSkillVisibility(worker.ID, "api-recon", false); err != nil {
		t.Fatal(err)
	}
	if err := d.SeedBuiltinSkillVisibility([]string{"chains-web", "api-recon", "cve-search", "shodan"}); err != nil {
		t.Fatal(err)
	}
	assertAgentSkills(t, d, worker.ID, []string{"chains-web"})
	if err := d.SetAgentSkillVisibility(worker.ID, nil); err != nil {
		t.Fatal(err)
	}
	if err := d.SeedBuiltinSkillVisibility(BuiltinSkillDefaultNames()); err != nil {
		t.Fatal(err)
	}
	assertAgentSkills(t, d, worker.ID, nil)
}

func TestCatalogSkillDefaultsInspectedEmptyAndLateAuxiliaryRole(t *testing.T) {
	d := catalogSkillDefaultsDB(t)
	worker := catalogSkillRole(t, d, "worker")
	if err := d.SeedBuiltinSkillVisibility([]string{}); err != nil {
		t.Fatal(err)
	}
	if configured, err := d.BuiltinSkillDefaultsConfigured(worker.ID, worker.Key); err != nil || !configured {
		t.Fatalf("inspected empty selection was not initialized: %v %v", configured, err)
	}
	if err := d.SeedBuiltinSkillVisibility(BuiltinSkillDefaultNames()); err != nil {
		t.Fatal(err)
	}
	assertAgentSkills(t, d, worker.ID, nil)
	reporter, err := d.CreateAgent("reporter", "Late auxiliary fixture", "")
	if err != nil {
		t.Fatal(err)
	}
	if configured, err := d.BuiltinSkillDefaultsConfigured(reporter.ID, reporter.Key); err != nil || configured {
		t.Fatalf("missing auxiliary role was prematurely marked: %v %v", configured, err)
	}
	if err := d.SeedBuiltinSkillVisibility([]string{"cve-search"}); err != nil {
		t.Fatal(err)
	}
	assertAgentSkills(t, d, reporter.ID, []string{"cve-search"})
	assertAgentSkills(t, d, worker.ID, nil)
}

func TestCatalogSkillDefaultsPreserveUnmarkedLegacyRows(t *testing.T) {
	d := catalogSkillDefaultsDB(t)
	worker := catalogSkillRole(t, d, "worker")
	if _, err := d.Exec(`INSERT INTO agent_skill_visibility(agent_id,skill_name,enabled) VALUES($1,'legacy-private',true),($1,'api-recon',false)`, worker.ID); err != nil {
		t.Fatal(err)
	}
	if configured, err := d.BuiltinSkillDefaultsConfigured(worker.ID, worker.Key); err != nil || !configured {
		t.Fatalf("explicit legacy rows lost their meaning without marker: %v %v", configured, err)
	}
	if err := d.SeedBuiltinSkillVisibility([]string{}); err != nil {
		t.Fatal(err)
	}
	assertAgentSkills(t, d, worker.ID, []string{"legacy-private"})
	var enabled bool
	if err := d.QueryRow(`SELECT enabled FROM agent_skill_visibility WHERE agent_id=$1 AND skill_name='api-recon'`, worker.ID).Scan(&enabled); err != nil || enabled {
		t.Fatalf("legacy false tombstone overwritten: %v %v", enabled, err)
	}
}

func TestCatalogSkillDefaultsCheckedStateDoesNotBroadenOtherRoles(t *testing.T) {
	d := catalogSkillDefaultsDB(t)
	worker := catalogSkillRole(t, d, "worker")
	if err := d.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := d.BuiltinSkillDefaultsConfigured(worker.ID, worker.Key); err == nil {
		t.Fatal("closed default-policy store must fail explicitly")
	}
	for _, role := range []string{"custom_fixture", "auto", "asset_mapper", CredentialAgentKey} {
		if configured, err := d.BuiltinSkillDefaultsConfigured(worker.ID, role); err != nil || !configured {
			t.Fatalf("role without a default Skill policy depended on that marker: %s %v %v", role, configured, err)
		}
	}
	if _, err := d.BuiltinSkillDefaultsConfigured(0, "worker"); err == nil || !strings.Contains(err.Error(), "Agent ID") {
		t.Fatal("invalid role identity accepted")
	}
}
