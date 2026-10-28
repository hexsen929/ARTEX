package server

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	actool "github.com/Autumn-27/norma/tool"
)

func writeGoalsReferenceFixture(t *testing.T, path string, content []byte) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, content, 0o644); err != nil {
		t.Fatal(err)
	}
}

func callGoalsReferenceFixture(t *testing.T, tool actool.CoreTool, path string, wantError bool) string {
	t.Helper()
	input, err := json.Marshal(map[string]string{"path": path})
	if err != nil {
		t.Fatal(err)
	}
	result, err := tool.Call(t.Context(), input, nil)
	if err != nil || result.IsError != wantError {
		t.Fatal("unexpected reference result state", err)
	}
	if wantError && result.Flatten() != goalsSkillReferenceError {
		t.Fatal("reference denial exposed variable error details")
	}
	return result.Flatten()
}

func TestGoalsReferenceReadsOnlyAuthorizedKnowledgeDocuments(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "授权 Skill 中文 空格")
	documents := map[string]string{
		"SKILL.md":              "索引：目标拆解指导。",
		"refs/规则.md":            "已授权的知识文档。",
		"refs/sub/extra.txt":    "Additional reference notes.",
		"references/context.md": "Reference context.",
	}
	for relative, text := range documents {
		writeGoalsReferenceFixture(t, filepath.Join(root, filepath.FromSlash(relative)), []byte(text))
	}
	tool, cleanup, err := newGoalsSkillReferenceTool(map[string]string{"fixture": root})
	if err != nil || tool == nil {
		t.Fatal("reference fixture was not assembled", err)
	}
	defer cleanup()
	if tool.Name() != "read_skill_reference" || !tool.IsReadOnly(nil) {
		t.Fatal("goal knowledge reader became a generic or writable tool")
	}
	for relative, text := range documents {
		if got := callGoalsReferenceFixture(t, tool, filepath.Join(root, filepath.FromSlash(relative)), false); got != text {
			t.Fatal("authorized document content changed")
		}
	}
	outside := filepath.Join(base, "ungranted", "refs", "outside.md")
	writeGoalsReferenceFixture(t, outside, []byte("outside-private-value"))
	denied := []string{outside, root, filepath.Join(root, "refs"), "refs/规则.md", root + string(filepath.Separator) + "refs" + string(filepath.Separator) + ".." + string(filepath.Separator) + "SKILL.md", os.DevNull}
	for relative, content := range map[string][]byte{
		".env":             []byte("fixture-secret-not-a-document"),
		"config.json":      []byte(`{"key":"fixture-private"}`),
		"refs/config.json": []byte(`{"key":"fixture-private"}`),
		"refs/script.py":   []byte("print('must never execute')"),
		"refs/binary.md":   {0xff, 0xfe, 0x00},
		"refs/nul.md":      []byte("text\x00private"),
		"refs/large.md":    []byte(strings.Repeat("x", maxGoalsSkillReferenceBytes+1)),
	} {
		path := filepath.Join(root, filepath.FromSlash(relative))
		writeGoalsReferenceFixture(t, path, content)
		denied = append(denied, path)
	}
	for _, path := range denied {
		callGoalsReferenceFixture(t, tool, path, true)
	}
	bad, _ := json.Marshal(map[string]string{"path": filepath.Join(root, "SKILL.md"), "command": "not-accepted"})
	if result, err := tool.Call(t.Context(), bad, nil); err != nil || !result.IsError || result.Flatten() != goalsSkillReferenceError {
		t.Fatal("reference reader accepted an undeclared action or echoed input", err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	valid, _ := json.Marshal(map[string]string{"path": filepath.Join(root, "SKILL.md")})
	if result, err := tool.Call(ctx, valid, nil); err != nil || !result.IsError {
		t.Fatal("cancelled document read continued", err)
	}
	cleanup()
	cleanup()
	callGoalsReferenceFixture(t, tool, filepath.Join(root, "SKILL.md"), true)
}

func TestGoalsReferenceRejectsCanonicalSecretsAndOutsideSymlinks(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "authorized")
	writeGoalsReferenceFixture(t, filepath.Join(root, "SKILL.md"), []byte("fixture index"))
	writeGoalsReferenceFixture(t, filepath.Join(root, ".env"), []byte("secret-not-a-reference"))
	outside := filepath.Join(base, "other", "refs", "outside.md")
	writeGoalsReferenceFixture(t, outside, []byte("outside reference"))
	for name, target := range map[string]string{"secret.md": filepath.Join(root, ".env"), "escape.md": outside} {
		link := filepath.Join(root, "refs", name)
		if err := os.MkdirAll(filepath.Dir(link), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(target, link); err != nil {
			t.Skipf("symlink fixture not supported: %v", err)
		}
	}
	tool, cleanup, err := newGoalsSkillReferenceTool(map[string]string{"fixture": root})
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	callGoalsReferenceFixture(t, tool, filepath.Join(root, "refs", "secret.md"), true)
	callGoalsReferenceFixture(t, tool, filepath.Join(root, "refs", "escape.md"), true)

	// A second explicitly authorized Skill adds only its own document scope;
	// the first reader remains an immutable authorization snapshot.
	union, closeUnion, err := newGoalsSkillReferenceTool(map[string]string{"first": root, "second": filepath.Join(base, "other")})
	if err != nil {
		t.Fatal(err)
	}
	defer closeUnion()
	if got := callGoalsReferenceFixture(t, union, filepath.Join(root, "refs", "escape.md"), false); got != "outside reference" {
		t.Fatal("authorized directory union did not resolve its knowledge document")
	}
	callGoalsReferenceFixture(t, tool, filepath.Join(root, "refs", "escape.md"), true)
}

func TestGoalsReferenceCapturedRootRejectsReplacement(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "fixture")
	document := filepath.Join(root, "refs", "rule.md")
	writeGoalsReferenceFixture(t, document, []byte("original authorized reference"))
	tool, cleanup, err := newGoalsSkillReferenceTool(map[string]string{"fixture": root})
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	if err := os.Rename(root, root+"-original"); err != nil {
		t.Skipf("open-root rename fixture not supported: %v", err)
	}
	writeGoalsReferenceFixture(t, document, []byte("replacement must not inherit authorization"))
	callGoalsReferenceFixture(t, tool, document, true)
}

func TestGoalsReferenceEmptyAuthorizationExposesNoTool(t *testing.T) {
	tool, cleanup, err := newGoalsSkillReferenceTool(nil)
	defer cleanup()
	if err != nil || tool != nil {
		t.Fatal("empty Skill authorization exposed a filesystem tool", err)
	}
}
