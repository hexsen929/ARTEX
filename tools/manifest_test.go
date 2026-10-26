package tools

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// writeManifest 在临时目录写一份清单并返回路径。
func writeManifest(t *testing.T, entries []Entry) string {
	t.Helper()
	b, err := json.Marshal(Manifest{Tools: entries})
	if err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(t.TempDir(), "tools-manifest.json")
	if err := os.WriteFile(p, b, 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestNativeExecutableWithForeignManifestHashIsNotCorrupt(t *testing.T) {
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	foreign := "linux/arm64"
	if runtime.GOOS+"/"+runtime.GOARCH == foreign {
		foreign = "linux/amd64"
	}
	t.Setenv("ARTEX_TEST_NATIVE_TOOL", executable)
	r := CheckEntry(Entry{Name: "native-tool", Env: "ARTEX_TEST_NATIVE_TOOL", Platform: foreign, SHA256: strings.Repeat("0", 64)}, t.TempDir())
	if r.Status != StatusUnpinned || r.PlatformWarning == "" || r.HostIncompatible {
		t.Fatalf("foreign-platform hash was incorrectly applied: %+v", r)
	}
}

func elfFixture(machine uint16) []byte {
	b := make([]byte, 64)
	copy(b, []byte{0x7f, 'E', 'L', 'F', 2, 1, 1})
	binary.LittleEndian.PutUint16(b[16:], 3)
	binary.LittleEndian.PutUint16(b[18:], machine)
	binary.LittleEndian.PutUint32(b[20:], 1)
	binary.LittleEndian.PutUint16(b[52:], 64)
	return b
}

func TestHostExecutableArchitectureMismatchIsExplicit(t *testing.T) {
	machine := uint16(62)
	if runtime.GOOS == "linux" && runtime.GOARCH == "amd64" {
		machine = 183
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "tool"), elfFixture(machine), 0o700); err != nil {
		t.Fatal(err)
	}
	r := CheckEntry(Entry{Name: "host-tool", Path: "tool", Execution: "host"}, dir)
	if !r.HostIncompatible || r.ActualPlatform == "" || r.PlatformWarning == "" {
		t.Fatalf("wrong-host executable was not identified: %+v", r)
	}
}

func TestTargetPayloadRetainsIntegrityCheckOnOtherHost(t *testing.T) {
	blob := elfFixture(62)
	sum := sha256.Sum256(blob)
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "payload"), blob, 0o600); err != nil {
		t.Fatal(err)
	}
	r := CheckEntry(Entry{Name: "target-payload", Path: "payload", Execution: "target", Platform: "linux/amd64", SHA256: hex.EncodeToString(sum[:])}, dir)
	if r.Status != StatusOK || r.HostIncompatible {
		t.Fatalf("target payload was treated as a host executable: %+v", r)
	}
}

func TestLoadMissing(t *testing.T) {
	_, err := Load(filepath.Join(t.TempDir(), "nope.json"))
	if !os.IsNotExist(err) {
		t.Fatalf("缺失清单应满足 os.IsNotExist,得到 %v", err)
	}
}

func TestCheckStatuses(t *testing.T) {
	dataDir := t.TempDir()
	content := []byte("fake-binary")
	sum := sha256.Sum256(content)
	if err := os.WriteFile(filepath.Join(dataDir, "ok"), content, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dataDir, "unpinned"), content, 0o700); err != nil {
		t.Fatal(err)
	}
	entries := []Entry{
		{Name: "ok", Path: "ok", SHA256: hex.EncodeToString(sum[:])},
		{Name: "unpinned", Path: "unpinned"},
		{Name: "missing", Path: "missing"},
		{Name: "mismatch", Path: "ok", SHA256: strings.Repeat("0", 64)},
	}
	m, err := Load(writeManifest(t, entries))
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]Status{}
	for _, r := range Check(m, dataDir) {
		got[r.Entry.Name] = r.Status
	}
	want := map[string]Status{
		"ok": StatusOK, "unpinned": StatusUnpinned,
		"missing": StatusMissing, "mismatch": StatusMismatch,
	}
	for name, ws := range want {
		if got[name] != ws {
			t.Errorf("%s: 期望状态 %v,得到 %v", name, ws, got[name])
		}
	}
}

func TestCheckEnvOverride(t *testing.T) {
	dataDir := t.TempDir()
	alt := filepath.Join(t.TempDir(), "custom-suo5")
	if err := os.WriteFile(alt, []byte("x"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ARTEX_SUO5_PATH", alt)
	r := CheckEntry(Entry{Name: "suo5", Path: "tools/suo5", Env: "ARTEX_SUO5_PATH"}, dataDir)
	if r.Status != StatusUnpinned || r.Path != alt {
		t.Fatalf("env 覆盖未生效: status=%v path=%s", r.Status, r.Path)
	}
}
