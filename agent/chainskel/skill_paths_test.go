package chainskel

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/Autumn-27/norma/skill"
	actool "github.com/Autumn-27/norma/tool"
)

func fixtureRefsDirectories(t *testing.T, categories ...string) map[string]string {
	t.Helper()
	root := filepath.Join(t.TempDir(), "custom Skill root 中文")
	dirs := map[string]string{}
	for _, category := range categories {
		name := "chains-" + category
		dir := filepath.Join(root, name)
		if err := os.MkdirAll(filepath.Join(dir, "refs"), 0o755); err != nil {
			t.Fatal(err)
		}
		dirs[name] = dir
	}
	return dirs
}

func TestRefsGuideRejectsUnresolvedMissingOrUnauthorizedSkillDirectories(t *testing.T) {
	input := payload(t, map[string]any{"chain_tags": []string{"web"}})
	for _, dirs := range []map[string]string{
		nil, {}, {"chains-ad": t.TempDir()}, {"chains-web": "relative/skills/chains-web"},
		{"chains-web": filepath.Join(t.TempDir(), "missing")}, {"chains-web": t.TempDir()},
	} {
		if got := RefsGuideForIntent(input, dirs); got != "" {
			t.Fatalf("unresolved, absent, or ungranted Skill path advertised: %s", got)
		}
	}
}

func TestSkillPathsResolveFromConfiguredRootNotIntentWorkingDirectory(t *testing.T) {
	for _, rootName := range []string{"installation-skills", "custom root 空格 中文"} {
		t.Run(rootName, func(t *testing.T) {
			root := filepath.Join(t.TempDir(), rootName)
			dir := filepath.Join(root, "chains-web")
			if err := os.MkdirAll(filepath.Join(dir, "refs"), 0o755); err != nil {
				t.Fatal(err)
			}
			index := "---\nname: chains-web\ndescription: Read-only path fixture\n---\nRead ${SKILL_DIR}/refs/fixture.md\n"
			if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte(index), 0o644); err != nil {
				t.Fatal(err)
			}
			ref := filepath.Join(dir, "refs", "fixture.md")
			if err := os.WriteFile(ref, []byte("fixture reference content"), 0o644); err != nil {
				t.Fatal(err)
			}
			intentDir := filepath.Join(t.TempDir(), "data", "tasks", "123", "i456")
			if err := os.MkdirAll(intentDir, 0o755); err != nil {
				t.Fatal(err)
			}
			guide := RefsGuideForIntent(payload(t, map[string]any{"chain_tags": []string{"web"}}), map[string]string{"chains-web": dir})
			if !strings.Contains(guide, strconv.Quote(filepath.Join(dir, "refs"))) || strings.Contains(guide, intentDir) {
				t.Fatal("L4 guide did not use this run's configured absolute directory")
			}
			registry, err := skill.LoadDir(root)
			if err != nil {
				t.Fatal(err)
			}
			loaded, err := registry.Tool().Call(t.Context(), json.RawMessage(`{"name":"chains-web"}`), &actool.ToolContext{WorkingDir: intentDir})
			if err != nil || loaded.IsError || len(loaded.Extra) != 1 || !strings.Contains(loaded.Extra[0].Text(), ref) || strings.Contains(loaded.Extra[0].Text(), "${SKILL_DIR}") {
				t.Fatal("SDK did not expand the portable index path against the actual Skill directory", err)
			}
			input, _ := json.Marshal(map[string]string{"file_path": ref})
			read, err := actool.NewRead().Call(t.Context(), input, &actool.ToolContext{WorkingDir: intentDir})
			if err != nil || read.IsError || !strings.Contains(read.Flatten(), "fixture reference content") {
				t.Fatal("Read from the task's different working directory did not resolve the absolute reference", err)
			}
		})
	}
}

func TestBundledChainIndicesUsePortableSkillDirectoryPlaceholders(t *testing.T) {
	indices, err := filepath.Glob(filepath.Join("..", "..", "skills", "chains-*", "SKILL.md"))
	if err != nil || len(indices) == 0 {
		t.Fatal("bundled chain indices unavailable", err)
	}
	refs := regexp.MustCompile(`\$\{SKILL_DIR\}/refs/([A-Za-z0-9_.-]+\.md)`)
	relative := regexp.MustCompile(`refs/[A-Za-z0-9_.-]+\.md`)
	for _, index := range indices {
		data, err := os.ReadFile(index)
		if err != nil {
			t.Fatal(err)
		}
		text := string(data)
		matches := refs.FindAllStringSubmatch(text, -1)
		if len(matches) == 0 || relative.MatchString(strings.ReplaceAll(text, "${SKILL_DIR}/refs/", "")) {
			t.Fatalf("bundled index %s still has working-directory-relative reference paths", filepath.Base(filepath.Dir(index)))
		}
		for _, match := range matches {
			if info, err := os.Stat(filepath.Join(filepath.Dir(index), "refs", match[1])); err != nil || !info.Mode().IsRegular() {
				t.Fatalf("bundled reference in %s is absent", filepath.Base(filepath.Dir(index)))
			}
		}
	}
}
