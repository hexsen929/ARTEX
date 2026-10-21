package config

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeInstructionsFixture(t *testing.T, path, text string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestLoadCodexInstructionsTopLevelAndRelativePath(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.toml")
	base := "  Model instruction {{literal.variable}}\r\n请保留空白。\n"
	writeInstructionsFixture(t, filepath.Join(dir, "model.md"), "\ufeff"+base)
	writeInstructionsFixture(t, configPath, "\ufeff"+`model = "unrelated-model"
model_instructions_file = "model.md"
developer_instructions = """
Developer line 1.
Developer line 2 with \"quotes\".
"""
[model_providers.custom]
model_instructions_file = "must-not-read.md"
developer_instructions = "must-not-apply"
api_key = "fixture-secret-not-for-import"
[profiles.custom]
developer_instructions = "profile-must-not-apply"
`)
	got, err := LoadCodexInstructions(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if got.ConfigPath != configPath || got.ModelInstructionsPath != filepath.Join(dir, "model.md") {
		t.Fatalf("resolved paths: %#v", got)
	}
	if got.BaseInstructions != base {
		t.Fatalf("model file changed: %q", got.BaseInstructions)
	}
	if want := "Developer line 1.\nDeveloper line 2 with \"quotes\".\n"; got.DeveloperInstructions != want {
		t.Fatalf("developer instructions=%q, want %q", got.DeveloperInstructions, want)
	}
}

func TestLoadCodexInstructionsDefaultsAndMissingKeys(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("CODEX_HOME", dir)
	writeInstructionsFixture(t, filepath.Join(dir, "config.toml"), `developer_instructions = 'Literal backslash: \n'
[model_providers.custom]
model_instructions_file = 'nested.md'
developer_instructions = 'nested developer'
`)
	got, err := LoadCodexInstructions("")
	if err != nil {
		t.Fatal(err)
	}
	if got.ConfigPath != filepath.Join(dir, "config.toml") || got.BaseInstructions != "" || got.ModelInstructionsPath != "" {
		t.Fatalf("default config path or absent root key: %#v", got)
	}
	if got.DeveloperInstructions != `Literal backslash: \n` {
		t.Fatalf("TOML literal string changed: %q", got.DeveloperInstructions)
	}
	for _, text := range []string{"", "# no instruction settings\nmodel = 'm'\n", "model_instructions_file = ''\ndeveloper_instructions = ''\n"} {
		writeInstructionsFixture(t, filepath.Join(dir, "config.toml"), text)
		got, err := LoadCodexInstructions("")
		if err != nil || got.BaseInstructions != "" || got.DeveloperInstructions != "" || got.ModelInstructionsPath != "" {
			t.Fatalf("empty instruction settings: %#v err=%v", got, err)
		}
	}
}

func TestLoadCodexInstructionsAbsoluteAndHomePaths(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.toml")
	modelPath := filepath.Join(dir, "absolute.md")
	writeInstructionsFixture(t, modelPath, "absolute base")
	writeInstructionsFixture(t, configPath, "model_instructions_file = '"+filepath.ToSlash(modelPath)+"'\n")
	got, err := LoadCodexInstructions(configPath)
	if err != nil || got.ModelInstructionsPath != modelPath || got.BaseInstructions != "absolute base" {
		t.Fatalf("absolute file: %#v err=%v", got, err)
	}
	// Resolve the user's home without changing HOME or touching any real file there.
	homeDir, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	resolved, err := resolveInstructionsPath("~/.codex/instructions.md", dir)
	if err != nil || resolved != filepath.Join(homeDir, ".codex", "instructions.md") {
		t.Fatalf("tilde resolution=%q err=%v", resolved, err)
	}
	resolved, err = resolveInstructionsPath("../base.md", filepath.Join(dir, "config"))
	if err != nil || resolved != filepath.Join(dir, "base.md") {
		t.Fatalf("relative parent path=%q err=%v", resolved, err)
	}
}

func TestLoadCodexInstructionsErrorsDoNotExposeContents(t *testing.T) {
	for _, tc := range []struct {
		name string
		text string
		want string
	}{
		{"syntax", "developer_instructions = \"fixture-secret-invalid", "invalid Codex configuration TOML"},
		{"wrong model type", "model_instructions_file = ['fixture-secret-invalid']", "invalid Codex configuration TOML"},
		{"wrong developer type", "developer_instructions = {secret = 'fixture-secret-invalid'}", "invalid Codex configuration TOML"},
		{"duplicate root", "developer_instructions='fixture-secret-invalid'\ndeveloper_instructions='other'", "invalid Codex configuration TOML"},
		{"encoded NUL", `developer_instructions = "fixture-secret-invalid\u0000"`, "must contain UTF-8 text without NUL"},
		{"missing model file", "model_instructions_file = 'missing.md'", "read model instructions file"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.toml")
			writeInstructionsFixture(t, path, tc.text)
			_, err := LoadCodexInstructions(path)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error=%v, want %q", err, tc.want)
			}
			if strings.Contains(err.Error(), "fixture-secret-invalid") {
				t.Fatal("configuration error exposed source contents")
			}
		})
	}
	missing := filepath.Join(t.TempDir(), "missing.toml")
	if _, err := LoadCodexInstructions(missing); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("missing config should return a file-not-found error: %v", err)
	}
}

func TestReadInstructionsTextFileValidation(t *testing.T) {
	for _, tc := range []struct {
		name string
		raw  []byte
		want string
	}{
		{"empty", nil, "is empty"},
		{"whitespace", []byte(" \r\n\t"), "is empty"},
		{"BOM only", []byte{0xef, 0xbb, 0xbf}, "is empty"},
		{"invalid UTF8", []byte{0xff}, "must contain UTF-8"},
		{"NUL", []byte("x\x00y"), "must contain UTF-8"},
		{"oversized", bytes.Repeat([]byte("a"), instructionsTextLimit+1), "exceeds 1 MiB"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "instructions.md")
			if err := os.WriteFile(path, tc.raw, 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := ReadInstructionsTextFile(path); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error=%v, want %q", err, tc.want)
			}
		})
	}
	dir := t.TempDir()
	if _, err := ReadInstructionsTextFile(dir); err == nil || !strings.Contains(err.Error(), "regular file") {
		t.Fatalf("directory must be rejected: %v", err)
	}
	path := filepath.Join(dir, "limit.md")
	text := strings.Repeat("a", instructionsTextLimit)
	writeInstructionsFixture(t, path, text)
	if got, err := ReadInstructionsTextFile(path); err != nil || got != text {
		t.Fatalf("file at the size limit: length=%d err=%v", len(got), err)
	}
	link := filepath.Join(dir, "link.md")
	if err := os.Symlink(path, link); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	if _, err := ReadInstructionsTextFile(link); err == nil || !strings.Contains(err.Error(), "regular file") {
		t.Fatalf("symlink must be rejected: %v", err)
	}
}
