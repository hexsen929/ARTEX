package server

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/Autumn-27/norma/skill"
	actool "github.com/Autumn-27/norma/tool"
)

func TestNormalizeLegacyChainReferencesOnlyChangesExplicitReferences(t *testing.T) {
	untouched := []string{
		"[external](https://example.invalid/refs/web-sqli.md)",
		"- 外部 URL https://example.invalid/(refs/web-sqli.md)",
		"`cat refs/web-sqli.md`",
		"`open(\"refs/web-sqli.md\")`",
		"open(refs/web-sqli.md)",
		"refs/web-sqli.md is mentioned as ordinary unformatted text",
		"    [indented-code](refs/web-sqli.md)",
		"\t`refs/web-sqli.md`",
		"```markdown\n[code-example](refs/web-sqli.md)\n`refs/web-sqli.md`\n```",
		"~~~markdown\n[code-example](refs/web-sqli.md)\n~~~",
		"``multi-line code\n[not-a-link](refs/web-sqli.md)\nrefs/web-sqli.md``",
		"[unsafe-traversal](refs/../outside.md)",
		"[not-markdown](refs/web-sqli.md.bak)",
	}
	before := "# Index\r\n- `refs/web-sqli.md`\r\n- [原文](refs/web-sqli.md#section)\r\n- 先检查前置判据(refs/web-sqli.md)\r\n" + strings.Join(untouched, "\n")
	sk := skill.Skill{Name: "fixture-alias", Dir: filepath.Join(t.TempDir(), "chains-web"), Description: "preserve metadata", MCPs: []string{"existing-mcp"}, Instructions: before}
	got := normalizeLegacyChainReferences(sk)
	for _, want := range []string{"`${SKILL_DIR}/refs/web-sqli.md`", "](${SKILL_DIR}/refs/web-sqli.md#section)", "判据(${SKILL_DIR}/refs/web-sqli.md)"} {
		if !strings.Contains(got.Instructions, want) {
			t.Fatalf("explicit legacy reference not normalized: %s", want)
		}
	}
	for _, original := range untouched {
		if !strings.Contains(got.Instructions, original) {
			t.Fatalf("non-reference code/URL text was changed: %s", original)
		}
	}
	if !strings.HasPrefix(got.Instructions, "# Index\r\n") || got.Name != sk.Name || got.Dir != sk.Dir || got.Description != sk.Description || !reflect.DeepEqual(got.MCPs, sk.MCPs) || sk.Instructions != before {
		t.Fatal("compatibility adapter mutated original Skill metadata or line endings")
	}
	if twice := normalizeLegacyChainReferences(got); !reflect.DeepEqual(twice, got) {
		t.Fatal("new path format is not idempotent")
	}
}

func TestNormalizeLegacyChainReferencesIsScopedToBundledDirectories(t *testing.T) {
	const original = "- `refs/fixture.md`\n- [引用](refs/fixture.md)\n"
	for _, name := range []string{"chains-meta", "chains-recon", "chains-web", "chains-ad", "chains-pivot", "chains-priv", "chains-app", "chains-rev", "chains-pwn"} {
		sk := skill.Skill{Name: "custom-declared-alias", Dir: filepath.Join(t.TempDir(), name), Instructions: original}
		if got := normalizeLegacyChainReferences(sk); !strings.Contains(got.Instructions, "${SKILL_DIR}/refs/fixture.md") {
			t.Fatalf("bundled directory %s was not adapted", name)
		}
	}
	for _, dir := range []string{"", "custom", "chains-custom", "chains-web.extra", "chains-web-copy"} {
		sk := skill.Skill{Name: "chains-web", Dir: filepath.Join(t.TempDir(), dir), Instructions: original}
		if got := normalizeLegacyChainReferences(sk); !reflect.DeepEqual(got, sk) {
			t.Fatalf("non-bundled directory %s was changed based on its display name", dir)
		}
	}
	modern := skill.Skill{Dir: filepath.Join(t.TempDir(), "chains-web"), Instructions: "- `${SKILL_DIR}/refs/fixture.md`\n- [引用](${SKILL_DIR}/refs/fixture.md)\n"}
	if got := normalizeLegacyChainReferences(modern); !reflect.DeepEqual(got, modern) {
		t.Fatal("new indices were normalized again")
	}
}

func TestLegacyChainBinaryUpgradeUsesActualRootWithoutChangingDisk(t *testing.T) {
	root := filepath.Join(t.TempDir(), "custom Skill 根目录 with spaces")
	dir := filepath.Join(root, "chains-web")
	refs := filepath.Join(dir, "refs")
	if err := os.MkdirAll(refs, 0o755); err != nil {
		t.Fatal(err)
	}
	index := []byte("---\r\nname: chains-web\r\ndescription: preserved installed fixture\r\n---\r\n# User-customized index\r\n- `refs/fixture.md`\r\n- [原文](refs/fixture.md)\r\n")
	indexPath := filepath.Join(dir, "SKILL.md")
	if err := os.WriteFile(indexPath, index, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(refs, "fixture.md"), []byte("read-only fixture reference"), 0o644); err != nil {
		t.Fatal(err)
	}
	all, err := skill.LoadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	original, ok := all.Get("chains-web")
	if !ok {
		t.Fatal("existing installed Skill not loaded")
	}
	registry := skill.NewRegistry()
	registry.Add(normalizeLegacyChainReferences(original))
	intentDir := filepath.Join(t.TempDir(), "tasks", "123", "i456")
	if err := os.MkdirAll(intentDir, 0o755); err != nil {
		t.Fatal(err)
	}
	loaded, err := registry.Tool().Call(t.Context(), json.RawMessage(`{"name":"chains-web"}`), &actool.ToolContext{WorkingDir: intentDir})
	expanded := dir + "/refs/fixture.md"
	if err != nil || loaded.IsError || len(loaded.Extra) != 1 || !strings.Contains(loaded.Extra[0].Text(), expanded) || strings.Contains(loaded.Extra[0].Text(), "${SKILL_DIR}") || !strings.Contains(loaded.Extra[0].Text(), "# User-customized index") {
		t.Fatal("binary-only adapter did not preserve custom text and expand its actual Skill directory", err)
	}
	input, _ := json.Marshal(map[string]string{"file_path": expanded})
	read, err := actool.NewRead().Call(t.Context(), input, &actool.ToolContext{WorkingDir: intentDir})
	if err != nil || read.IsError || !strings.Contains(read.Flatten(), "read-only fixture reference") {
		t.Fatal("adapted absolute reference did not work from an unrelated task directory", err)
	}
	after, err := os.ReadFile(indexPath)
	if err != nil || !bytes.Equal(after, index) {
		t.Fatal("runtime compatibility adapter rewrote the existing user's Skill file", err)
	}
}
