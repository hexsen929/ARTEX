package server

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func writeSkillInspectionFixture(t *testing.T, root, name, content string) string {
	t.Helper()
	dir := filepath.Join(root, name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "SKILL.md")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestInspectSkillEntryUsesConfiguredUnicodeRootAndDeclaredAlias(t *testing.T) {
	root := filepath.Join(t.TempDir(), "自定义 Skill 根目录")
	writeSkillInspectionFixture(t, root, "directory-alias", "---\r\nname: declared-alias\r\ndescription: Read-only fixture\r\nmcps: [fixture-one, fixture-two]\r\n---\r\n# Fixture body\r\n")
	got := inspectSkillEntry(root, "directory-alias")
	if !got.Loadable || got.LoadError != "" || got.DeclaredName != "declared-alias" {
		t.Fatalf("valid configured Skill alias rejected: %+v", got)
	}
	writeSkillInspectionFixture(t, root, "plain", "# Plain Markdown without frontmatter\nRead-only fixture.\n")
	if got := inspectSkillEntry(root, "plain"); !got.Loadable || got.DeclaredName != "plain" {
		t.Fatalf("SDK-compatible directory fallback rejected: %+v", got)
	}
	writeSkillInspectionFixture(t, root, "scalar-mcps", "---\nname: scalar-mcps\nmcp: fixture-one, fixture-two\n---\nRead-only fixture.\n")
	if got := inspectSkillEntry(root, "scalar-mcps"); !got.Loadable {
		t.Fatalf("SDK-compatible scalar dependency declaration rejected: %+v", got)
	}
}

func TestInspectSkillEntryReportsInvalidAndMissingEntriesWithoutTextLeaks(t *testing.T) {
	root := t.TempDir()
	const privateText = "fixture-private-index-text-do-not-echo"
	cases := map[string]string{
		"bad-yaml":        "---\nname: [broken\n" + privateText + "\n---\nBody\n",
		"missing-close":   "---\nname: missing-close\n" + privateText,
		"markdown-header": "---\n## name: broken\ndescription: x\n# title\n```json\n{" + privateText + "}\n```\n---\nBody\n",
		"empty-body":      "---\nname: empty-body\n---\n  \n",
		"bad-mcp":         "---\nname: bad-mcp\nmcps: {unexpected: value}\n---\nBody\n",
		"space-name":      "---\nname: ' trailing '\n---\nBody\n",
		"invalid-text":    "# Body\n\x00" + privateText,
		"bom-index":       "\xef\xbb\xbf---\nname: bom-index\n---\nBody\n",
	}
	for name, content := range cases {
		writeSkillInspectionFixture(t, root, name, content)
		got := inspectSkillEntry(root, name)
		if got.Loadable || got.LoadError == "" || strings.Contains(got.LoadError, privateText) || strings.Contains(got.LoadError, root) {
			t.Errorf("invalid entry %s accepted or private parser text leaked: %+v", name, got)
		}
	}
	if err := os.MkdirAll(filepath.Join(root, "missing-index"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"missing-index", "absent", "../escape", ".", "..", "/absolute", `nested\escape`} {
		got := inspectSkillEntry(root, name)
		if got.Loadable || got.LoadError == "" {
			t.Errorf("invalid or missing path %q accepted", name)
		}
	}
	if got := inspectSkillEntry("", "plain"); got.Loadable || got.LoadError == "" {
		t.Fatal("empty root must not silently select process working directory")
	}
	writeSkillInspectionFixture(t, root, "oversized", strings.Repeat("x", maxSkillIndexBytes+1))
	if got := inspectSkillEntry(root, "oversized"); got.Loadable || got.LoadError == "" {
		t.Fatal("unbounded Skill index was accepted")
	}
}

func TestInspectSkillEntryAllowsConfiguredRootSymlinkButRejectsEscapingEntry(t *testing.T) {
	root := filepath.Join(t.TempDir(), "actual root")
	path := writeSkillInspectionFixture(t, root, "valid", "---\nname: valid\n---\nBody\n")
	alias := filepath.Join(t.TempDir(), "configured root link")
	if err := os.Symlink(root, alias); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if got := inspectSkillEntry(alias, "valid"); !got.Loadable {
		t.Fatalf("explicitly configured root symlink rejected: %+v", got)
	}
	// An index symlink that stays inside the configured root remains supported.
	shared := filepath.Join(root, "shared-index.md")
	if err := os.Rename(path, shared); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(shared, path); err != nil {
		t.Fatal(err)
	}
	if got := inspectSkillEntry(alias, "valid"); !got.Loadable {
		t.Fatalf("in-root index symlink rejected: %+v", got)
	}
	outside := filepath.Join(t.TempDir(), "private-index.md")
	if err := os.WriteFile(outside, []byte("fixture-private-text"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, path); err != nil {
		t.Fatal(err)
	}
	if got := inspectSkillEntry(alias, "valid"); got.Loadable || !strings.Contains(got.LoadError, "根目录之外") {
		t.Fatalf("escaping index symlink accepted: %+v", got)
	}
}

func TestInspectSkillCatalogDetectsDuplicateAliasesAndPreservesBadDirectories(t *testing.T) {
	root := t.TempDir()
	for _, name := range []string{"first", "second"} {
		writeSkillInspectionFixture(t, root, name, "---\nname: same-alias\n---\nBody\n")
	}
	if err := os.Mkdir(filepath.Join(root, "without-index"), 0o755); err != nil {
		t.Fatal(err)
	}
	catalog, err := inspectSkillCatalog(root)
	if err != nil || len(catalog) != 3 {
		t.Fatal("catalog dropped diagnostic entries", err)
	}
	for _, name := range []string{"first", "second"} {
		if catalog[name].Loadable || !strings.Contains(catalog[name].LoadError, "重复") {
			t.Fatalf("duplicate alias not diagnosed: %+v", catalog[name])
		}
	}
	if catalog["without-index"].Loadable || catalog["without-index"].LoadError == "" {
		t.Fatal("missing index was disguised as a loadable unused Skill")
	}
	if _, err := inspectSkillCatalog(filepath.Join(root, "absent")); err == nil || strings.Contains(err.Error(), root) {
		t.Fatal("catalog root failure missing or leaking its configured path")
	}
}

func TestInspectSkillEntryUnreadableIndex(t *testing.T) {
	if runtime.GOOS == "windows" || os.Geteuid() == 0 {
		t.Skip("permission-bit fixture requires a non-root Unix account")
	}
	root := t.TempDir()
	path := writeSkillInspectionFixture(t, root, "unreadable", "Body\n")
	if err := os.Chmod(path, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(path, 0o644) })
	if got := inspectSkillEntry(root, "unreadable"); got.Loadable || got.LoadError == "" {
		t.Fatal("unreadable index accepted")
	}
}

func TestBundledScopeSentryIndexHasValidHeaderWithoutImplicitMCPGrant(t *testing.T) {
	root := filepath.Join("..", "skills")
	got := inspectSkillEntry(root, "scopesentry")
	if !got.Loadable || got.DeclaredName != "scopesentry-mcp" {
		t.Fatalf("bundled ScopeSentry metadata is not loadable: %+v", got)
	}
	data, err := os.ReadFile(filepath.Join(root, "scopesentry", "SKILL.md"))
	if err != nil {
		t.Fatal(err)
	}
	parts := strings.SplitN(string(data), "\n---", 2)
	if len(parts) != 2 || strings.Contains(parts[0], "mcps:") || strings.Contains(parts[0], "mcp:") || !strings.Contains(parts[1], "# ScopeSentry MCP 使用指南") {
		t.Fatal("header repair changed the body or silently declared an MCP dependency")
	}
}
