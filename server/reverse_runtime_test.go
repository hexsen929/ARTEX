package server

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestPenelopePreparedRuntimeAndExplicitOverride(t *testing.T) {
	t.Setenv("ARTEX_PENELOPE_PATH", "")
	t.Setenv("ARTEX_PENELOPE_PYTHON", "")
	data := t.TempDir()
	if got := newPenelopeHandler(context.Background(), data, nil, nil).python; got != "python3" {
		t.Fatalf("legacy interpreter = %q", got)
	}
	prepared := filepath.Join(data, "tools", "python-envs", "penelope", "bin", "python")
	if err := os.MkdirAll(filepath.Dir(prepared), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(prepared, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if got := newPenelopeHandler(context.Background(), data, nil, nil).python; got != prepared {
		t.Fatalf("prepared interpreter = %q, want %q", got, prepared)
	}
	t.Setenv("ARTEX_PENELOPE_PYTHON", "/custom/python")
	if got := newPenelopeHandler(context.Background(), data, nil, nil).python; got != "/custom/python" {
		t.Fatalf("explicit interpreter = %q", got)
	}
}
