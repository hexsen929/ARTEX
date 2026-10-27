package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Autumn-27/artex/stage"
)

type stagePolicyFixture struct {
	server     *Server
	ctx        context.Context
	dataDir    string
	managerDir string
	keyDir     string
	configPath string
	publicFile string
}

func newStagePolicyFixture(t *testing.T) stagePolicyFixture {
	t.Helper()
	base := t.TempDir()
	t.Chdir(base)
	fixture := stagePolicyFixture{
		dataDir: filepath.Join(base, "stage-data"), managerDir: filepath.Join(base, "manager-data"),
		keyDir: filepath.Join(base, "actual-custom-keys"), configPath: filepath.Join(base, "settings", "custom.json"),
	}
	// A different process-global key directory catches regressions that ignore
	// Server.keyDir and protect config.KeyDir instead of the keys actually loaded.
	t.Setenv("ARTEX_CONFIG", fixture.configPath)
	t.Setenv("ARTEX_KEY_DIR", filepath.Join(base, "unused-environment-keys"))
	for _, dir := range []string{fixture.dataDir, fixture.managerDir, fixture.keyDir} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	writeStagePolicyFixture(t, fixture.configPath)
	writeStagePolicyFixture(t, filepath.Join(fixture.keyDir, jwtKeyFilename))
	writeStagePolicyFixture(t, filepath.Join(fixture.keyDir, gateKeyFilename))
	fixture.publicFile = filepath.Join(fixture.dataDir, "tasks", "7", "i9", "fixture-payload.txt")
	writeStagePolicyFixture(t, fixture.publicFile)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	fixture.ctx = ctx
	fixture.server = &Server{ctx: ctx, keyDir: fixture.keyDir, m: &Manager{dir: fixture.managerDir}}
	fixture.server.initStage(fixture.dataDir)
	if fixture.server.stage == nil {
		t.Fatal("stage initialization failed")
	}
	t.Cleanup(fixture.server.stage.Close)
	if fixture.server.stage.FilePrecheck == nil || fixture.server.stage.FilePolicy == nil || fixture.server.stage.OnEvent == nil {
		t.Fatal("stage initialization omitted a path, descriptor or audit hook")
	}
	return fixture
}

func writeStagePolicyFixture(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("stage-policy-fixture-only"), 0o600); err != nil {
		t.Fatal(err)
	}
}

func assertStageFileRejected(t *testing.T, fixture stagePolicyFixture, path string) {
	t.Helper()
	m := fixture.server.stage
	before := len(m.List())
	if err := m.FilePrecheck(path); err == nil {
		t.Fatal("protected file passed the initialized precheck")
	}
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	err = m.FilePolicy(path, f)
	_ = f.Close()
	if err == nil {
		t.Fatal("protected descriptor passed the initialized file policy")
	}
	if _, err := m.PutFile(path, stage.PutOpts{}); err == nil {
		t.Fatal("direct staging accepted a protected source")
	}
	body, err := json.Marshal(map[string]string{"path": path})
	if err != nil {
		t.Fatal(err)
	}
	w := httptest.NewRecorder()
	fixture.server.stageCreate(w, httptest.NewRequest(http.MethodPost, "/api/stage", strings.NewReader(string(body))))
	if w.Code != http.StatusBadRequest {
		t.Fatalf("manual file staging accepted a protected source: status=%d", w.Code)
	}
	result, err := fixture.server.stageShareTool().Call(fixture.ctx, body, nil)
	if err != nil || !result.IsError {
		t.Fatal("Agent file staging accepted a protected source")
	}
	if len(m.List()) != before {
		t.Fatal("a rejected source created a staged download")
	}
}

func TestInitStagePolicyProtectsActualCustomKeysAndDataRoots(t *testing.T) {
	f := newStagePolicyFixture(t)
	paths := []string{
		f.configPath,
		filepath.Join(f.keyDir, jwtKeyFilename), filepath.Join(f.keyDir, gateKeyFilename),
		filepath.Join(f.keyDir, "additional-private-key.fixture"),
		filepath.Join(f.dataDir, "config.json"), filepath.Join(f.dataDir, ".env"),
		filepath.Join(f.dataDir, gatePathFilename), filepath.Join(f.dataDir, "browser-mcp-proxy.json"),
		filepath.Join(f.managerDir, "browser-mcp-proxy.json"),
	}
	for _, dir := range []string{f.dataDir, f.managerDir} {
		for _, relative := range []string{
			"sessions/private.fixture", "transcripts/private.fixture", "traffic/_ca/private.fixture",
			"traffic-tasks/7/_ca/private.fixture", "tunnels/7/auth.fixture", "mcp-private/browser.fixture",
			"evidence/private.fixture", "archives/tasks/private.fixture",
			"reverse/private.fixture", "task-archives/private.fixture", ".delete-staging/private.fixture", "stage/private.fixture",
		} {
			paths = append(paths, filepath.Join(dir, filepath.FromSlash(relative)))
		}
	}
	for _, path := range paths {
		writeStagePolicyFixture(t, path)
		t.Run(filepath.Base(filepath.Dir(path))+"-"+filepath.Base(path), func(t *testing.T) {
			assertStageFileRejected(t, f, path)
		})
	}
	for name, link := range map[string]func(string, string) error{"hardlink": os.Link, "symlink": os.Symlink} {
		t.Run(name, func(t *testing.T) {
			alias := filepath.Join(f.dataDir, "tasks", "7", name+"-to-key.fixture")
			if err := link(filepath.Join(f.keyDir, jwtKeyFilename), alias); err != nil {
				t.Skipf("filesystem does not support fixture link: %v", err)
			}
			assertStageFileRejected(t, f, alias)
		})
	}
}

func TestInitStagePolicyKeepsToolsAndTaskGeneratedFilesPublishable(t *testing.T) {
	f := newStagePolicyFixture(t)
	for _, path := range []string{
		filepath.Join(f.dataDir, "tools", "fixture-tool"),
		filepath.Join(f.managerDir, "tools", "fixture-tool"),
		f.publicFile,
		filepath.Join(f.managerDir, "tasks", "8", "i3", "generated.fixture"),
	} {
		writeStagePolicyFixture(t, path)
		t.Run(filepath.Base(filepath.Dir(path))+"-"+filepath.Base(path), func(t *testing.T) {
			if err := f.server.stage.FilePrecheck(path); err != nil {
				t.Fatal("a normal tool or task artifact was blocked")
			}
			entry, err := f.server.stage.PutFile(path, stage.PutOpts{})
			if err != nil {
				t.Fatal(err)
			}
			f.server.stage.Delete(entry.Token)
		})
	}
	body, err := json.Marshal(map[string]string{"path": f.publicFile})
	if err != nil {
		t.Fatal(err)
	}
	w := httptest.NewRecorder()
	f.server.stageCreate(w, httptest.NewRequest(http.MethodPost, "/api/stage", strings.NewReader(string(body))))
	if w.Code != http.StatusOK {
		t.Fatalf("manual staging rejected a normal task artifact: status=%d", w.Code)
	}
	result, err := f.server.stageShareTool().Call(f.ctx, body, nil)
	if err != nil || result.IsError {
		t.Fatal("Agent staging rejected a normal task artifact")
	}
}

func TestInitStagePolicyValidatesOpenedDescriptorIdentity(t *testing.T) {
	f := newStagePolicyFixture(t)
	other := filepath.Join(f.dataDir, "tasks", "7", "different.fixture")
	writeStagePolicyFixture(t, other)
	if err := f.server.stage.FilePrecheck(f.publicFile); err != nil {
		t.Fatal(err)
	}
	opened, err := os.Open(other)
	if err != nil {
		t.Fatal(err)
	}
	defer opened.Close()
	if err := f.server.stage.FilePolicy(f.publicFile, opened); err == nil {
		t.Fatal("file descriptor for a different source passed initialized policy")
	}
}
