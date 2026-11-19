package server

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
)

func TestManagedFileReplacementKeepsPreviousFileOnCopyError(t *testing.T) {
	root, err := os.OpenRoot(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()

	const name = "测试.txt"
	if err := replaceManagedFile(root, name, strings.NewReader("original"), 0o600); err != nil {
		t.Fatal(err)
	}
	copyFailure := errors.New("interrupted copy")
	source := io.MultiReader(strings.NewReader("partial"), managedFileFailedReader{copyFailure})
	if err := replaceManagedFile(root, name, source, 0o644); !errors.Is(err, copyFailure) {
		t.Fatalf("expected copy failure, got %v", err)
	}
	if got, err := root.ReadFile(name); err != nil || string(got) != "original" {
		t.Fatalf("failed replacement affected previous file: %q, %v", got, err)
	}
	if err := replaceManagedFile(root, name, strings.NewReader("complete"), 0o644); err != nil {
		t.Fatal(err)
	}
	file, info, err := openManagedRegularFile(root, name)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	got, err := io.ReadAll(file)
	if err != nil || string(got) != "complete" {
		t.Fatalf("unexpected replacement content: %q, %v", got, err)
	}
	if runtime.GOOS != "windows" && info.Mode().Perm() != 0o600 {
		t.Fatalf("replacement changed file permissions to %o", info.Mode().Perm())
	}
}

type managedFileFailedReader struct{ err error }

func (r managedFileFailedReader) Read([]byte) (int, error) { return 0, r.err }

func TestSkillEditorKeepsEachDirectorySeparate(t *testing.T) {
	library := t.TempDir()
	taskNotes := writeSkillInspectionFixture(t, library, "notes", "# Notes\n")
	writeSkillInspectionFixture(t, library, "examples", "# Examples\n")
	if err := os.Symlink(filepath.Join("..", "notes", "SKILL.md"), filepath.Join(library, "examples", "linked.md")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	root, err := openSkillEditorRoot(library, "examples")
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()

	files, err := listSkillFiles(root)
	if err != nil || !slices.Equal(files, []string{"SKILL.md"}) {
		t.Fatalf("skill editor should list only its own editable contents: %q, %v", files, err)
	}
	if data, err := readSkillEditorFile(root, "SKILL.md", maxSkillIndexBytes); err != nil || string(data) != "# Examples\n" {
		t.Fatalf("normal skill entry cannot be read: %q, %v", data, err)
	}
	if err := replaceManagedFile(root, "SKILL.md", strings.NewReader("# Updated examples\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if data, err := os.ReadFile(taskNotes); err != nil || string(data) != "# Notes\n" {
		t.Fatalf("editing one skill affected another: %q, %v", data, err)
	}
}
