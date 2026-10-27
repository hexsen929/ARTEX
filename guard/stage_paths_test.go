package guard

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func stagePolicyFixture(t *testing.T) (*StagePathPolicy, string, string) {
	t.Helper()
	base := t.TempDir()
	data := filepath.Join(base, "data")
	files := []string{filepath.Join(base, "config.json"), filepath.Join(base, "jwt.key"), filepath.Join(base, "gate.key"), filepath.Join(base, ".env")}
	for _, p := range files {
		if err := os.WriteFile(p, []byte("private-fixture-data"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	dirs := []string{filepath.Join(base, "keys")}
	for _, name := range []string{"sessions", "transcripts", "traffic", "evidence", "reverse", "task-archives"} {
		dirs = append(dirs, filepath.Join(data, name))
	}
	for _, dir := range dirs {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "private.txt"), []byte("private"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	for _, dir := range []string{filepath.Join(data, "tools"), filepath.Join(data, "tasks", "1"), filepath.Join(data, "evidence-copy")} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "tool.bin"), []byte("public-tool-fixture"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return NewStagePathPolicy(files, dirs), base, data
}

func TestStagePathPolicyBlocksPrivateFilesAndDirectories(t *testing.T) {
	policy, base, data := stagePolicyFixture(t)
	var paths []string
	for _, name := range []string{"config.json", "jwt.key", "gate.key", ".env"} {
		paths = append(paths, filepath.Join(base, name))
	}
	for _, name := range []string{"sessions", "transcripts", "traffic", "evidence", "reverse", "task-archives"} {
		paths = append(paths, filepath.Join(data, name, "private.txt"))
	}
	paths = append(paths, filepath.Join(base, "keys", "private.txt"))
	paths = append(paths, data+string(filepath.Separator)+".."+string(filepath.Separator)+"jwt.key")
	for _, path := range paths {
		if err := policy.Check(path); !errors.Is(err, ErrStageProtectedPath) {
			t.Fatalf("private path accepted: %s (%v)", filepath.Base(path), err)
		}
		file, err := os.Open(path)
		if err != nil {
			t.Fatal(err)
		}
		checked := policy.CheckOpenFile(path, file)
		_ = file.Close()
		if !errors.Is(checked, ErrStageProtectedPath) {
			t.Fatal("opened private file accepted")
		}
		if strings.Contains(checked.Error(), base) {
			t.Fatal("policy error leaked a private path")
		}
	}
}

func TestStagePathPolicyAllowsToolsAndGeneratedWorkspaceFiles(t *testing.T) {
	policy, _, data := stagePolicyFixture(t)
	for _, path := range []string{filepath.Join(data, "tools", "tool.bin"), filepath.Join(data, "tasks", "1", "tool.bin"), filepath.Join(data, "evidence-copy", "tool.bin")} {
		if err := policy.Check(path); err != nil {
			t.Fatalf("public tool denied: %v", err)
		}
		f, err := os.Open(path)
		if err != nil {
			t.Fatal(err)
		}
		checked := policy.CheckOpenFile(path, f)
		_ = f.Close()
		if checked != nil {
			t.Fatal(checked)
		}
	}
	// An explicit external tool path remains usable without a broad exception
	// that could override the platform's protected files or secret directories.
	external := filepath.Join(t.TempDir(), "external-tool")
	if err := os.WriteFile(external, []byte("fixture"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := policy.Check(external); err != nil {
		t.Fatal(err)
	}
}

func TestStagePathPolicyRejectsSymlinkAndHardlinkAliases(t *testing.T) {
	policy, base, data := stagePolicyFixture(t)
	alias := filepath.Join(data, "tools", "secret-link")
	if err := os.Symlink(filepath.Join(base, "jwt.key"), alias); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if err := policy.Check(alias); !errors.Is(err, ErrStageProtectedPath) {
		t.Fatal("secret symlink accepted")
	}
	dirAlias := filepath.Join(base, "transcript-alias")
	if err := os.Symlink(filepath.Join(data, "transcripts"), dirAlias); err != nil {
		t.Fatal(err)
	}
	if err := policy.Check(filepath.Join(dirAlias, "private.txt")); !errors.Is(err, ErrStageProtectedPath) {
		t.Fatal("private-directory alias accepted")
	}
	hardlink := filepath.Join(data, "tools", "secret-hardlink")
	if err := os.Link(filepath.Join(base, "jwt.key"), hardlink); err != nil {
		t.Skipf("hardlinks unavailable: %v", err)
	}
	if err := policy.Check(hardlink); !errors.Is(err, ErrStageProtectedPath) {
		t.Fatal("secret hardlink accepted")
	}
}

func TestStagePathPolicyDetectsChangedSourceDescriptor(t *testing.T) {
	policy, base, data := stagePolicyFixture(t)
	public := filepath.Join(data, "tools", "tool.bin")
	secret, err := os.Open(filepath.Join(base, "jwt.key"))
	if err != nil {
		t.Fatal(err)
	}
	defer secret.Close()
	if err = policy.CheckOpenFile(public, secret); !errors.Is(err, ErrStageSourceChanged) {
		t.Fatalf("descriptor/path mismatch accepted: %v", err)
	}
	alias := filepath.Join(data, "tools", "racing-link")
	if err = os.Symlink(filepath.Join(base, "jwt.key"), alias); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	// Model the swap back to an allowed file after a protected descriptor was
	// already opened. Post-open identity checking must still reject it.
	if err = os.Remove(alias); err != nil {
		t.Fatal(err)
	}
	if err = os.Symlink(public, alias); err != nil {
		t.Fatal(err)
	}
	if err = policy.CheckOpenFile(alias, secret); !errors.Is(err, ErrStageSourceChanged) {
		t.Fatal("symlink-swap descriptor accepted")
	}
}

func TestStagePathPolicyRejectsMissingAndNonRegularSources(t *testing.T) {
	policy, _, data := stagePolicyFixture(t)
	for _, path := range []string{"", filepath.Join(data, "tools", "missing"), filepath.Join(data, "tools")} {
		if err := policy.Check(path); !errors.Is(err, ErrStagePathUnavailable) {
			t.Fatalf("invalid source accepted: %v", err)
		}
	}
	if err := policy.CheckOpenFile(filepath.Join(data, "tools", "tool.bin"), nil); !errors.Is(err, ErrStagePathUnavailable) {
		t.Fatal("nil source accepted")
	}
}

func TestStagePathPolicyProtectsDirectoryCreatedAfterConstruction(t *testing.T) {
	base := t.TempDir()
	private := filepath.Join(base, "future-private")
	policy := NewStagePathPolicy(nil, []string{private})
	if err := os.Mkdir(private, 0o700); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(private, "fixture.txt")
	if err := os.WriteFile(file, []byte("private"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := policy.Check(file); !errors.Is(err, ErrStageProtectedPath) {
		t.Fatal("late-created private directory accepted")
	}
}

func TestStagePathPolicyRetainsOriginalProtectedSymlinkRoot(t *testing.T) {
	base := t.TempDir()
	original := filepath.Join(base, "original-private")
	replacement := filepath.Join(base, "replacement-private")
	for _, dir := range []string{original, replacement} {
		if err := os.Mkdir(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "private.txt"), []byte("fixture"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	alias := filepath.Join(base, "key-directory")
	if err := os.Symlink(original, alias); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	policy := NewStagePathPolicy(nil, []string{alias})
	if err := os.Remove(alias); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(replacement, alias); err != nil {
		t.Fatal(err)
	}
	for _, dir := range []string{original, replacement} {
		if err := policy.Check(filepath.Join(dir, "private.txt")); !errors.Is(err, ErrStageProtectedPath) {
			t.Fatal("retargeted private directory escaped protection")
		}
	}
}
