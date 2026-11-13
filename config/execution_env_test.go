package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestOperatorExecutionEnvironmentUsesOwnedRuntimeWithoutOverwritingExplicitChoices(t *testing.T) {
	data := t.TempDir()
	os.Mkdir(filepath.Join(data, "tools"), 0700)
	for _, key := range []string{"ARTEX_HOME", "DSH_HOME", "ARTEX_DATA_DIR", "ARTEX_KB_DIR"} {
		t.Setenv(key, "")
	}
	t.Setenv("ARTEX_CONFIG", filepath.Join(t.TempDir(), "missing.json"))
	t.Setenv("PATH", "/usr/bin:/bin")
	if err := ConfigureExecutionEnvironment(data); err != nil {
		t.Fatal(err)
	}
	if os.Getenv("ARTEX_DATA_DIR") != data || !strings.HasPrefix(os.Getenv("PATH"), filepath.Join(data, "tools")+string(os.PathListSeparator)) {
		t.Fatal("runtime namespace missing")
	}
	t.Setenv("DSH_HOME", "explicit-operator-home")
	if err := ConfigureExecutionEnvironment(data); err != nil {
		t.Fatal(err)
	}
	if os.Getenv("DSH_HOME") != "explicit-operator-home" {
		t.Fatal("explicit operator setting overwritten")
	}
	if strings.Count(os.Getenv("PATH"), filepath.Join(data, "tools")) != 1 {
		t.Fatal("duplicate PATH entry")
	}
}
