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

func TestHostVariantSelectsItsOwnPathAndHash(t *testing.T) {
	dir := t.TempDir()
	content := []byte("native-tool-variant")
	if err := os.WriteFile(filepath.Join(dir, "native"), content, 0o700); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(content)
	host := runtime.GOOS + "/" + runtime.GOARCH
	r := CheckEntry(Entry{
		Name: "tool", Execution: "host", Path: "legacy", Platform: "legacy/platform", SHA256: strings.Repeat("0", 64),
		Variants: []Variant{
			{Platform: "unavailable/platform", Path: "foreign", SHA256: strings.Repeat("1", 64)},
			{Platform: host, Path: "native", Version: "native-version", SHA256: hex.EncodeToString(sum[:])},
		},
	}, dir)
	if r.Status != StatusOK || r.Entry.Platform != host || r.Entry.Version != "native-version" || r.Path != filepath.Join(dir, "native") || r.PlatformWarning != "" {
		t.Fatalf("host variant was not independently selected and verified: %+v", r)
	}
}

func TestUnpinnedVariantNeverInheritsLegacyChecksum(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "native"), []byte("tool"), 0o700); err != nil {
		t.Fatal(err)
	}
	r := CheckEntry(Entry{
		Name: "tool", Path: "native", SHA256: strings.Repeat("0", 64),
		Variants: []Variant{{Platform: runtime.GOOS + "/" + runtime.GOARCH}},
	}, dir)
	if r.Status != StatusUnpinned || r.Entry.SHA256 != "" || r.Actual != "" {
		t.Fatalf("platform inherited an unrelated checksum: %+v", r)
	}
}

func TestMissingHostVariantInspectsOverrideWithoutForeignChecksum(t *testing.T) {
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("ARTEX_TEST_NATIVE_VARIANT", executable)
	r := CheckEntry(Entry{
		Name: "tool", Env: "ARTEX_TEST_NATIVE_VARIANT", SHA256: strings.Repeat("0", 64),
		Variants: []Variant{{Platform: "unavailable/platform", SHA256: strings.Repeat("1", 64)}},
	}, t.TempDir())
	if r.Status != StatusUnpinned || r.PlatformWarning == "" || r.HostIncompatible || r.Entry.SHA256 != "" {
		t.Fatalf("missing platform pin applied another platform checksum: %+v", r)
	}
}

func TestTargetVariantsAreAllCheckedRegardlessOfHost(t *testing.T) {
	dir := t.TempDir()
	var variants []Variant
	for _, item := range []struct {
		platform string
		machine  uint16
	}{
		{"linux/amd64", 62},
		{"linux/arm64", 183},
	} {
		name := strings.ReplaceAll(item.platform, "/", "-")
		content := elfFixture(item.machine)
		if err := os.WriteFile(filepath.Join(dir, name), content, 0o600); err != nil {
			t.Fatal(err)
		}
		sum := sha256.Sum256(content)
		variants = append(variants, Variant{Platform: item.platform, Path: name, SHA256: hex.EncodeToString(sum[:])})
	}
	results := Check(&Manifest{Tools: []Entry{{Name: "target", Execution: "target", Variants: variants}}}, dir)
	if len(results) != 2 {
		t.Fatalf("checked %d target variants, want both", len(results))
	}
	for _, r := range results {
		if r.Status != StatusOK || r.PlatformWarning != "" || r.HostIncompatible {
			t.Errorf("target variant incorrectly selected by host: %+v", r)
		}
	}
}

func TestTargetExecutableHeaderMustMatchDeclaredPlatform(t *testing.T) {
	dir := t.TempDir()
	content := elfFixture(62)
	if err := os.WriteFile(filepath.Join(dir, "target"), content, 0o600); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(content)
	r := CheckEntry(Entry{Name: "target", Execution: "target", Path: "target", Platform: "linux/arm64", SHA256: hex.EncodeToString(sum[:])}, dir)
	if r.Status != StatusOK || r.PlatformWarning == "" || r.HostIncompatible {
		t.Fatalf("target architecture mismatch was not reported separately from integrity: %+v", r)
	}
}

func TestPlatformMatchesUniversalExecutables(t *testing.T) {
	for _, tc := range []struct {
		list, want string
		matches    bool
	}{
		{"darwin/amd64,darwin/arm64", "darwin/arm64", true},
		{"linux/amd64", "darwin/arm64", false},
		{"linux/amd64", "linux/arm64,linux/amd64", true},
		{"any", "darwin/arm64", true},
		{"", "darwin/arm64", false},
	} {
		if got := PlatformMatches(tc.list, tc.want); got != tc.matches {
			t.Errorf("PlatformMatches(%q, %q) = %v, want %v", tc.list, tc.want, got, tc.matches)
		}
	}
}

func TestMissingToolPathDoesNotCountDataDirectoryAsInstalled(t *testing.T) {
	r := CheckEntry(Entry{Name: "no-path"}, t.TempDir())
	if r.Status != StatusMissing || r.Path != "" || r.PlatformWarning == "" {
		t.Fatalf("data directory was incorrectly treated as an installed tool: %+v", r)
	}
}
