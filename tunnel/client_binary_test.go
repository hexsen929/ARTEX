package tunnel

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/Autumn-27/artex/stage"
)

func chiselELFFixture(machine uint16) []byte {
	b := make([]byte, 64)
	copy(b, []byte{0x7f, 'E', 'L', 'F', 2, 1, 1})
	binary.LittleEndian.PutUint16(b[16:], 3)
	binary.LittleEndian.PutUint16(b[18:], machine)
	binary.LittleEndian.PutUint32(b[20:], 1)
	binary.LittleEndian.PutUint16(b[52:], 64)
	return b
}

func writeChiselFixture(t *testing.T, path string, data []byte) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o700); err != nil {
		t.Fatal(err)
	}
}

func TestParseChiselTargetRejectsAmbiguousOrUnknownArchitecture(t *testing.T) {
	for _, tc := range []struct {
		name, output, want string
	}{
		{"linux amd64", "noise\nMARK_OS=Linux\nMARK_ARCH=x86_64\n", "linux/amd64"},
		{"linux arm64", "MARK_OS=Linux\r\nMARK_ARCH=aarch64\r\n", "linux/arm64"},
		{"darwin parsed separately from lifecycle support", "MARK_OS=Darwin\nMARK_ARCH=arm64", "darwin/arm64"},
		{"wrong sentinel", "OTHER_OS=Linux\nOTHER_ARCH=x86_64", ""},
		{"missing architecture", "MARK_OS=Linux", ""},
		{"duplicate architecture", "MARK_OS=Linux\nMARK_ARCH=x86_64\nMARK_ARCH=aarch64", ""},
		{"path traversal", "MARK_OS=Linux\nMARK_ARCH=../../host", ""},
		{"unsupported OS", "MARK_OS=FreeBSD\nMARK_ARCH=x86_64", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseChiselTarget(tc.output, "MARK")
			if tc.want == "" {
				if err == nil {
					t.Fatalf("ambiguous platform accepted: %q", got)
				}
			} else if err != nil || got != tc.want {
				t.Fatalf("platform=%q err=%v want=%q", got, err, tc.want)
			}
		})
	}
}

func TestChiselClientSelectsTargetArchitectureAndNeverFallsBackToHost(t *testing.T) {
	dir := t.TempDir()
	host := filepath.Join(dir, "host-chisel")
	target := filepath.Join(dir, "targets", "linux-arm64", "chisel")
	writeChiselFixture(t, host, chiselELFFixture(62))
	writeChiselFixture(t, target, chiselELFFixture(183))
	m := &Manager{chiselPath: host, chiselTargetsDir: filepath.Join(dir, "targets")}
	client, err := m.resolveChiselClient("linux/arm64")
	if err != nil || client.path != target || client.platform != "linux/arm64" || client.sha256 == "" {
		t.Fatalf("selected client=%+v err=%v", client, err)
	}
	if _, err := m.resolveChiselClient("linux/amd64"); err == nil {
		t.Fatal("missing target file silently fell back to a matching host binary")
	}
	m.chiselTargetPath = host // Explicit compatibility override still verifies headers.
	if _, err := m.resolveChiselClient("linux/arm64"); err == nil {
		t.Fatal("explicit override with the wrong architecture was accepted")
	}
	if client, err := m.resolveChiselClient("linux/amd64"); err != nil || client.path != host {
		t.Fatalf("matching explicit override failed: %+v %v", client, err)
	}
	for _, unsupported := range []string{"windows/amd64", "darwin/arm64", "linux/../../amd64", "linux/riscv64"} {
		if _, err := m.resolveChiselClient(unsupported); err == nil {
			t.Fatalf("unsupported automatic lifecycle accepted: %s", unsupported)
		}
	}
}

type chiselFixtureSession struct {
	kind, osName, arch string
	commands           []string
	written            []byte
}

func (*chiselFixtureSession) ID() int64                   { return 7 }
func (s *chiselFixtureSession) Kind() string              { return s.kind }
func (*chiselFixtureSession) Test(context.Context) error  { return nil }
func (*chiselFixtureSession) Close(context.Context) error { return nil }
func (*chiselFixtureSession) ReadFile(context.Context, string) ([]byte, error) {
	return nil, nil
}
func (s *chiselFixtureSession) WriteFile(_ context.Context, _ string, b []byte) error {
	s.written = append([]byte(nil), b...)
	return nil
}
func (s *chiselFixtureSession) Exec(_ context.Context, cmd string, _ time.Duration) (string, string, error) {
	s.commands = append(s.commands, cmd)
	if marker := regexp.MustCompile(`ARTEX_CHISEL_[0-9a-f]+`).FindString(cmd); marker != "" {
		return marker + "_OS=" + s.osName + "\n" + marker + "_ARCH=" + s.arch + "\n", "", nil
	}
	if strings.HasPrefix(cmd, "chmod ") {
		return "ARTEX_CHMOD_OK", "", nil
	}
	return "ARTEX_DL_FAIL", "", nil
}

func TestPrepareChiselProbesTargetAndRecordsSelectedClient(t *testing.T) {
	host, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	data := chiselELFFixture(183)
	target := filepath.Join(dir, "linux-arm64", "chisel")
	writeChiselFixture(t, target, data)
	m := &Manager{chiselPath: host, chiselTargetsDir: dir}
	sess := &chiselFixtureSession{kind: "http_php", osName: "Linux", arch: "aarch64"}
	p := &Plan{}
	client, err := m.prepareChisel(context.Background(), sess, p)
	wantSum := sha256.Sum256(data)
	if err != nil || client.path != target || p.ClientPlatform != "linux/arm64" || p.ClientSHA256 != hex.EncodeToString(wantSum[:]) {
		t.Fatalf("target preparation failed: client=%+v plan=%+v err=%v", client, p, err)
	}
	if len(sess.commands) != 1 || !strings.Contains(sess.commands[0], "uname -s") || !strings.Contains(sess.commands[0], "uname -m") {
		t.Fatalf("target platform was not actually probed: %v", sess.commands)
	}
	// A stored plan is audit data, never authority to skip probing on redeploy.
	sess.arch = "x86_64"
	if _, err := m.prepareChisel(context.Background(), sess, p); err == nil {
		t.Fatal("redeploy trusted the old target architecture despite a changed session")
	}
}

func TestChiselWindowsDoesNotRunPOSIXProbe(t *testing.T) {
	sess := &chiselFixtureSession{kind: "http_aspx"}
	if _, err := detectChiselTarget(context.Background(), sess); err == nil || len(sess.commands) != 0 {
		t.Fatalf("Windows session should fail explicitly before POSIX commands: err=%v commands=%v", err, sess.commands)
	}
	sess = &chiselFixtureSession{kind: "reverse", osName: "Darwin", arch: "arm64"}
	if _, err := detectChiselTarget(context.Background(), sess); err == nil {
		t.Fatal("Darwin automatic lifecycle was claimed despite Linux-only process control")
	}
}

func TestChiselDeliveryUsesSelectedTargetBytesForSessionFallback(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	st, err := stage.New(ctx, filepath.Join(t.TempDir(), "stage"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	st.SetBaseURL("http://127.0.0.1:8787")
	dir := t.TempDir()
	host := filepath.Join(dir, "host")
	target := filepath.Join(dir, "target")
	writeChiselFixture(t, host, []byte("host bytes must never be delivered"))
	data := chiselELFFixture(183)
	writeChiselFixture(t, target, data)
	sum := sha256.Sum256(data)
	client := chiselClientBinary{path: target, platform: "linux/arm64", sha256: hex.EncodeToString(sum[:])}
	m := &Manager{stage: st, chiselPath: host}
	p := &Plan{TaskID: 1, PlatformAddr: "127.0.0.1:20001", RemoteDir: "/tmp/artex-fixture", RemoteBin: "/tmp/artex-fixture/ch"}
	sess := &chiselFixtureSession{kind: "http_php"}
	if err := m.deliver(ctx, sess, p, client); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(sess.written, data) || p.StageToken == "" {
		t.Fatalf("wrong bytes delivered or missing stage ledger: bytes=%x token=%q", sess.written, p.StageToken)
	}
	writeChiselFixture(t, target, []byte("changed"))
	sess.written = nil
	if err := m.deliver(ctx, sess, p, client); err == nil || sess.written != nil {
		t.Fatalf("changed client file was delivered: err=%v bytes=%x", err, sess.written)
	}
}

func TestChiselSnapshotRejectsSourceChangeBeforeStagePublication(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	st, err := stage.New(ctx, filepath.Join(t.TempDir(), "stage"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	target := filepath.Join(t.TempDir(), "chisel")
	data := chiselELFFixture(183)
	writeChiselFixture(t, target, data)
	sum := sha256.Sum256(data)
	client := chiselClientBinary{path: target, platform: "linux/arm64", sha256: hex.EncodeToString(sum[:])}
	// PutFile previously ran this policy only after deliver's separate path hash.
	// An in-place update here could publish unchecked bytes under the old hash.
	st.FilePolicy = func(path string, _ *os.File) error {
		return os.WriteFile(path, []byte("replacement after selection"), 0o700)
	}
	m := &Manager{stage: st}
	sess := &chiselFixtureSession{kind: "http_php"}
	p := &Plan{TaskID: 1}
	if err := m.deliver(ctx, sess, p, client); err == nil {
		t.Fatal("source mutation was accepted")
	}
	if len(st.List()) != 0 || p.StageToken != "" || len(sess.commands) != 0 || sess.written != nil {
		t.Fatalf("unverified bytes were published or delivered: stages=%d commands=%v", len(st.List()), sess.commands)
	}
}

func TestChiselStageAndFallbackShareImmutableSnapshot(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stageDir := filepath.Join(t.TempDir(), "stage")
	st, err := stage.New(ctx, stageDir)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	st.SetBaseURL("http://127.0.0.1:8787")
	target := filepath.Join(t.TempDir(), "chisel")
	data := chiselELFFixture(183)
	writeChiselFixture(t, target, data)
	sum := sha256.Sum256(data)
	client := chiselClientBinary{path: target, platform: "linux/arm64", sha256: hex.EncodeToString(sum[:])}
	var staged []byte
	st.OnEvent = func(event stage.Event) {
		if event.Kind != stage.EventPut {
			return
		}
		var readErr error
		staged, readErr = os.ReadFile(filepath.Join(stageDir, event.Entry.Token, event.Entry.Name))
		if readErr != nil {
			t.Fatal(readErr)
		}
		// A concurrent tool update after publication must not change the session
		// fallback bytes or require opening the source pathname again.
		if err := os.WriteFile(target, []byte("updated source"), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	m := &Manager{stage: st}
	p := &Plan{TaskID: 1, PlatformAddr: "127.0.0.1:20001", RemoteDir: "/tmp/artex-fixture", RemoteBin: "/tmp/artex-fixture/ch"}
	sess := &chiselFixtureSession{kind: "http_php"}
	if err := m.deliver(ctx, sess, p, client); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(staged, data) || !bytes.Equal(sess.written, data) {
		t.Fatalf("stage and fallback diverged from verified snapshot: stage=%x fallback=%x", staged, sess.written)
	}
}
