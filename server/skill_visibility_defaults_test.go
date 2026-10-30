package server

import (
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/Autumn-27/artex/db"
)

func TestInstalledSkillDefaultsMatchPublicReleaseBundle(t *testing.T) {
	root := filepath.Join("..", "skills")
	available, skipped, err := installedBuiltinSkillDefaults(root)
	if err != nil {
		t.Fatal(err)
	}
	wantMissing := []string{"bool-sqli-admin-takeover", "ctf-ai-ml", "ctf-web", "cve-search", "shodan"}
	if !reflect.DeepEqual(skipped, wantMissing) || len(available) != 11 {
		t.Fatalf("public package defaults disagree with shipped files: available=%v skipped=%v", available, skipped)
	}
	if slices.Contains(available, "scopesentry") {
		t.Fatal("installed optional Skill was automatically default-authorized")
	}
	for _, name := range available {
		if !slices.Contains(db.BuiltinSkillDefaultNames(), name) {
			t.Fatalf("non-policy Skill auto-granted: %s", name)
		}
	}
}

func TestInstalledSkillDefaultsUseConfiguredRootAndNoOptionalExpansion(t *testing.T) {
	root := filepath.Join(t.TempDir(), "配置的 Skill 目录 with spaces")
	writeSkillInspectionFixture(t, root, "api-recon", "---\nname: api-guide-alias\n---\nRead-only fixture.\n")
	writeSkillInspectionFixture(t, root, "chains-web", "# Read-only fixture\n")
	writeSkillInspectionFixture(t, root, "not-a-default", "# Read-only fixture\n")
	available, _, err := installedBuiltinSkillDefaults(root)
	if err != nil || !reflect.DeepEqual(available, []string{"api-recon", "chains-web"}) {
		t.Fatalf("configured root or directory-name policy lost: %v %v", available, err)
	}
	alias := filepath.Join(t.TempDir(), "configured-root-link")
	if err := os.Symlink(root, alias); err != nil {
		t.Skipf("root symlink unavailable: %v", err)
	}
	linked, _, err := installedBuiltinSkillDefaults(alias)
	if err != nil || !reflect.DeepEqual(linked, available) {
		t.Fatalf("explicit root symlink rejected: %v %v", linked, err)
	}
}

func TestInstalledSkillDefaultsSkipInvalidIndexesAndSDKAliasOverwrites(t *testing.T) {
	const privateText = "fixture-private-index-content-must-not-leak"
	root := t.TempDir()
	writeSkillInspectionFixture(t, root, "api-recon", "---\nname: api-recon\n---\nRead-only fixture.\n")
	writeSkillInspectionFixture(t, root, "chains-web", "---\nname: [invalid\n"+privateText+"\n---\nBody\n")
	// SDK LoadDir accepts bare Markdown too. Its final alias registry, not
	// directory existence alone, determines whether the authorized entry loads.
	if err := os.WriteFile(filepath.Join(root, "z-overwrite.md"), []byte("---\nname: api-recon\n---\nRead-only alias fixture.\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	available, skipped, err := installedBuiltinSkillDefaults(root)
	if err != nil || len(available) != 0 || !slices.Contains(skipped, "api-recon") || !slices.Contains(skipped, "chains-web") {
		t.Fatalf("invalid/overwritten default granted: available=%v skipped=%v err=%v", available, skipped, err)
	}
	for _, name := range skipped {
		if strings.Contains(name, privateText) || strings.Contains(name, root) {
			t.Fatal("default diagnostics leaked private index text or paths")
		}
	}
	if err := os.Remove(filepath.Join(root, "z-overwrite.md")); err != nil {
		t.Fatal(err)
	}
	writeSkillInspectionFixture(t, root, "chains-web", "---\nname: api-recon\n---\nRead-only duplicate alias.\n")
	available, _, err = installedBuiltinSkillDefaults(root)
	if err != nil || len(available) != 0 {
		t.Fatalf("directory alias collision received defaults: %v %v", available, err)
	}
}

func TestInstalledSkillDefaultsUnknownRootIsNotInspectedEmpty(t *testing.T) {
	for _, root := range []string{"", filepath.Join(t.TempDir(), "absent-private-root")} {
		available, _, err := installedBuiltinSkillDefaults(root)
		if err == nil || available != nil || root != "" && strings.Contains(err.Error(), root) {
			t.Fatalf("unknown catalog became an empty default choice or leaked its path: %v %v", available, err)
		}
	}
	root := t.TempDir()
	available, skipped, err := installedBuiltinSkillDefaults(root)
	if err != nil || available == nil || len(available) != 0 || len(skipped) != len(db.BuiltinSkillDefaultNames()) {
		t.Fatalf("successfully inspected empty directory not represented explicitly: %v %v %v", available, skipped, err)
	}
}

func TestInstalledSkillDefaultsUnreadableRootIsNotMarkedEmpty(t *testing.T) {
	root := filepath.Join(t.TempDir(), "unreadable-root")
	if err := os.Mkdir(root, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(root, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(root, 0o700) })
	if _, err := os.ReadDir(root); err == nil {
		t.Skip("current process can still read the permission-denied fixture")
	}
	available, _, err := installedBuiltinSkillDefaults(root)
	if err == nil || available != nil || strings.Contains(err.Error(), root) {
		t.Fatalf("unreadable root accepted or leaked: %v %v", available, err)
	}
}

func TestInstalledSkillDefaultsStartupSeedsOnlyLoadableFilesAndKeepsSelections(t *testing.T) {
	if os.Getenv("ARTEX_PG_DSN") == "" {
		t.Skip("requires an explicitly configured isolated CI database")
	}
	pg := openIntranetTestDB(t)
	pg.SetMaxOpenConns(1)
	pg.SetMaxIdleConns(1)
	// Shadow only authorization/initialization state on one connection. No
	// existing public role selection or marker is reset by this fixture.
	if _, err := pg.Exec(`CREATE TEMP TABLE settings (LIKE public.settings INCLUDING ALL);
CREATE TEMP TABLE agent_skill_visibility (LIKE public.agent_skill_visibility INCLUDING ALL)`); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = pg.Exec(`DROP TABLE IF EXISTS pg_temp.agent_skill_visibility, pg_temp.settings`) })
	worker, err := pg.GetAgentByKey("worker")
	if err != nil || worker == nil {
		t.Fatalf("worker fixture unavailable: %v", err)
	}
	unknown := filepath.Join(t.TempDir(), "missing-root")
	if err := seedInstalledBuiltinSkillVisibility(pg, unknown); err == nil {
		t.Fatal("startup ignored an unknown Skill catalog")
	}
	if configured, err := pg.BuiltinSkillDefaultsConfigured(worker.ID, worker.Key); err != nil || configured {
		t.Fatalf("unknown startup catalog marked role initialized: %v %v", configured, err)
	}
	available, _, err := installedBuiltinSkillDefaults(filepath.Join("..", "skills"))
	if err != nil {
		t.Fatal(err)
	}
	if err := seedInstalledBuiltinSkillVisibility(pg, filepath.Join("..", "skills")); err != nil {
		t.Fatal(err)
	}
	names, err := pg.AgentSkillNames(worker.ID)
	if err != nil || len(names) == 0 {
		t.Fatalf("fresh startup has no loadable worker defaults: %v %v", names, err)
	}
	for _, name := range names {
		if !slices.Contains(available, name) {
			t.Fatalf("startup granted an uninstalled default: %s", name)
		}
	}
	if configured, err := pg.BuiltinSkillDefaultsConfigured(worker.ID, worker.Key); err != nil || !configured {
		t.Fatalf("known startup catalog failed to complete initialization: %v %v", configured, err)
	}
	if err := pg.SetAgentSkillVisibility(worker.ID, nil); err != nil {
		t.Fatal(err)
	}
	later := t.TempDir()
	writeSkillInspectionFixture(t, later, "cve-search", "# Optional read-only fixture\n")
	writeSkillInspectionFixture(t, later, "shodan", "# Optional read-only fixture\n")
	if err := seedInstalledBuiltinSkillVisibility(pg, later); err != nil {
		t.Fatal(err)
	}
	if names, err := pg.AgentSkillNames(worker.ID); err != nil || len(names) != 0 {
		t.Fatalf("later installation restored an explicitly empty selection: %v %v", names, err)
	}
	if err := pg.ToggleSkillVisibility(worker.ID, "uninstalled-private-fixture", true); err != nil {
		t.Fatal(err)
	}
	if err := seedInstalledBuiltinSkillVisibility(pg, t.TempDir()); err != nil {
		t.Fatal(err)
	}
	if names, err := pg.AgentSkillNames(worker.ID); err != nil || !reflect.DeepEqual(names, []string{"uninstalled-private-fixture"}) {
		t.Fatalf("existing selection was silently removed when files disappeared: %v %v", names, err)
	}
}
