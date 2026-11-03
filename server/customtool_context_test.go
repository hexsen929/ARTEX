package server

import (
	"context"
	"encoding/json"
	"encoding/pem"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Autumn-27/artex/agent"
	"github.com/Autumn-27/artex/db"
	"github.com/Autumn-27/artex/traffic"
	"github.com/Autumn-27/artex/tunnel"
	actool "github.com/Autumn-27/norma/tool"
)

func customScriptServer(t *testing.T) (*Server, string) {
	t.Helper()
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Fatal("Python is required by the custom-script CI regressions")
	}
	dsn, _, err := db.DSN()
	if err != nil {
		t.Fatal("PostgreSQL fixture configuration is required")
	}
	pg, err := db.Open(dsn)
	if err != nil {
		t.Fatal("PostgreSQL fixture initialization failed")
	}
	t.Cleanup(func() { _ = pg.Close() })
	previous, present, err := pg.GetSetting(settingPythonInterp)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if present {
			_ = pg.SetSetting(settingPythonInterp, previous)
		} else {
			_ = pg.DeleteSetting(settingPythonInterp)
		}
	})
	return &Server{m: &Manager{pg: pg, dir: t.TempDir()}}, python
}

func TestCustomScriptFailuresHaveErrorResultsAndEditorFlags(t *testing.T) {
	s, python := customScriptServer(t)
	for _, tc := range []struct {
		name, code, want string
		timeout          int
		missing, failure bool
	}{
		{name: "success", code: "import sys; print('stdout-fixture'); print('stderr-fixture', file=sys.stderr)", want: "stdout-fixture"},
		{name: "exit", code: "import sys; print('before-exit', flush=True); sys.exit(7)", want: "before-exit", failure: true},
		{name: "exception", code: "print('before-exception', flush=True); raise RuntimeError('fixture-error')", want: "fixture-error", failure: true},
		{name: "timeout", code: "import time; time.sleep(20)", timeout: 100, want: "超时终止", failure: true},
		{name: "launch", code: "print('not-run')", missing: true, want: "exit:", failure: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			interpreter := python
			if tc.missing {
				interpreter = filepath.Join(t.TempDir(), "missing-python")
			}
			if err := s.m.pg.SetSetting(settingPythonInterp, interpreter); err != nil {
				t.Fatal(err)
			}
			spec, _ := json.Marshal(scriptExec{Code: tc.code, TimeoutMs: tc.timeout})
			res, err := s.runScriptTool(t.Context(), "fixture", spec, nil, &actool.ToolContext{WorkingDir: s.m.dir})
			if err != nil || res.IsError != tc.failure || !strings.Contains(res.Flatten(), tc.want) {
				t.Fatalf("script result mismatch: error=%v is_error=%v output=%q", err, res.IsError, res.Flatten())
			}
			request, _ := json.Marshal(testToolReq{Kind: "script", Exec: spec})
			w := httptest.NewRecorder()
			s.pgTestCustomTool(w, httptest.NewRequest(http.MethodPost, "/api/tools/test", strings.NewReader(string(request))))
			var result struct {
				Output  string `json:"output"`
				IsError bool   `json:"is_error"`
			}
			if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &result) != nil || result.IsError != tc.failure || !strings.Contains(result.Output, tc.want) {
				t.Fatalf("editor result did not preserve execution outcome: status=%d body=%s", w.Code, w.Body)
			}
			files, err := filepath.Glob(filepath.Join(s.m.dir, ".tools", "*.py"))
			if err != nil || len(files) != 0 {
				t.Fatal("temporary script was retained")
			}
		})
	}
}

func TestCustomScriptOutputAndCancellationAreBounded(t *testing.T) {
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Fatal("Python fixture is required")
	}
	out, err := execPython(t.Context(), python, "bounded", "import sys; sys.stdout.write('x' * 3000000); sys.stderr.write('y' * 3000000)", nil, t.TempDir(), nil, 10*time.Second)
	if err != nil || len(out) > customToolMaxOutput+100 || !strings.Contains(out, "输出超过上限") {
		t.Fatalf("subprocess output was not bounded: length=%d error=%v", len(out), err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	out, err = execPython(ctx, python, "cancelled", "print('must-not-run')", nil, t.TempDir(), nil, time.Second)
	if !errors.Is(err, context.Canceled) || strings.Contains(out, "must-not-run") || !strings.Contains(out, "执行已取消") {
		t.Fatalf("cancelled execution reported success: %v %q", err, out)
	}
}

func customHTTPFixture(t *testing.T, s *Server, ctx context.Context, rawURL string, spec httpExec, env []string) actool.Result {
	t.Helper()
	spec.URL = rawURL
	execRaw, _ := json.Marshal(spec)
	res, err := s.runHTTPTool(ctx, execRaw, nil, &actool.ToolContext{Env: env})
	if err != nil {
		t.Fatal(err)
	}
	return res
}

func TestCustomHTTPDirectAndExplicitEmptyEnvNeverUseEnvironmentOrGlobalProxy(t *testing.T) {
	var proxyCalls atomic.Int64
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		proxyCalls.Add(1)
		_, _ = io.WriteString(w, "wrong-route")
	}))
	defer proxy.Close()
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "direct-fixture") }))
	defer target.Close()
	for _, name := range []string{"HTTP_PROXY", "http_proxy", "HTTPS_PROXY", "https_proxy", "ALL_PROXY", "all_proxy"} {
		t.Setenv(name, proxy.URL)
	}
	s := &Server{m: &Manager{globalProxy: proxy.URL}}
	for _, tc := range []struct {
		name string
		spec httpExec
		env  []string
	}{
		{name: "explicit direct"},
		{name: "host explicit empty", spec: httpExec{UseRecordingProxy: true}, env: []string{"HTTP_PROXY=", "ALL_PROXY=" + proxy.URL}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tr, _, err := s.customHTTPTransport(t.Context(), tc.spec, &actool.ToolContext{Env: tc.env}, "http")
			if err != nil || tr == nil || tr.Proxy != nil {
				t.Fatalf("direct transport inherited a proxy: %v %#v", err, tr)
			}
			tr.CloseIdleConnections()
			res := customHTTPFixture(t, s, t.Context(), target.URL, tc.spec, tc.env)
			if res.IsError || !strings.Contains(res.Flatten(), "direct-fixture") {
				t.Fatalf("explicit direct route failed: %s", res.Flatten())
			}
		})
	}
	if proxyCalls.Load() != 0 {
		t.Fatal("a direct request used an inherited/global proxy")
	}
}

func TestCustomHTTPHostRoutesAreIsolatedAndRecordingSwitchRemainsAuthoritative(t *testing.T) {
	proxy := func(label string) *httptest.Server {
		return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, label) }))
	}
	a, b, global := proxy("route-a"), proxy("route-b"), proxy("platform-route")
	defer a.Close()
	defer b.Close()
	defer global.Close()
	s := &Server{m: &Manager{globalProxy: global.URL}}
	var wg sync.WaitGroup
	for _, tc := range []struct{ proxy, want string }{{a.URL, "route-a"}, {b.URL, "route-b"}} {
		wg.Add(1)
		go func(tc struct{ proxy, want string }) {
			defer wg.Done()
			res := customHTTPFixture(t, s, t.Context(), "http://fixture.invalid/", httpExec{UseRecordingProxy: true, Proxy: global.URL}, []string{"HTTP_PROXY=" + tc.proxy})
			if res.IsError || !strings.Contains(res.Flatten(), tc.want) {
				t.Errorf("host route was replaced: %s", res.Flatten())
			}
		}(tc)
	}
	wg.Wait()
	res := customHTTPFixture(t, s, t.Context(), "http://fixture.invalid/", httpExec{UseRecordingProxy: true, Proxy: a.URL}, nil)
	if res.IsError || !strings.Contains(res.Flatten(), "platform-route") {
		t.Fatal("recording switch no longer selected the platform route")
	}
	res = customHTTPFixture(t, s, t.Context(), "http://fixture.invalid/", httpExec{Proxy: b.URL}, []string{"HTTP_PROXY=" + a.URL})
	if res.IsError || !strings.Contains(res.Flatten(), "route-b") {
		t.Fatal("explicit proxy in non-platform mode was ignored")
	}
}

func TestCustomHTTPTaskRouteWinsOverHostEnvAndNeverFallsBackOnFailure(t *testing.T) {
	plan, _ := json.Marshal(tunnel.Plan{SocksPort: 22001, AuthUser: "fixture-user", AuthPass: "fixture-pass"})
	lister := &proxyCompatTunnelLister{recs: map[int64][]*db.TunnelRecord{1: {{ID: 1, Kind: tunnel.KindSocks, Adapter: tunnel.AdapterSuo5, State: db.TunnelAlive, DeployParams: plan}}}}
	m := &Manager{globalProxy: "http://127.0.0.1:22002"}
	m.taskProxy = newTaskProxyManager(t.TempDir(), lister, m.GlobalProxy, 0, 0)
	defer m.taskProxy.Close()
	s := &Server{m: m}
	ctx := agent.WithRunInfo(t.Context(), agent.RunInfo{TaskID: 1, ExplorationID: 1})
	tr, _, err := s.customHTTPTransport(ctx, httpExec{UseRecordingProxy: true}, &actool.ToolContext{Env: []string{"HTTP_PROXY=http://127.0.0.1:22003"}}, "https")
	if err != nil {
		t.Fatal(err)
	}
	defer tr.CloseIdleConnections()
	u, err := tr.Proxy(httptest.NewRequest(http.MethodGet, "https://fixture.invalid/", nil))
	if err != nil || u == nil || u.Scheme != "socks5" || u.Host != "127.0.0.1:22001" || u.User.Username() != "fixture-user" {
		t.Fatalf("task route was not preserved: %v %v", u, err)
	}
	lister.err = errors.New("private-route-read-error")
	tr, _, err = s.customHTTPTransport(ctx, httpExec{UseRecordingProxy: true}, &actool.ToolContext{Env: []string{"HTTP_PROXY=http://127.0.0.1:22003"}}, "http")
	if err == nil || tr != nil || strings.Contains(err.Error(), "private-route-read-error") {
		t.Fatal("unknown task route fell back or leaked driver details")
	}
}

func TestCustomHTTPTaskRecorderAndCAAreTaskLocal(t *testing.T) {
	parent, err := traffic.Open(t.TempDir(), "")
	if err != nil {
		t.Fatal(err)
	}
	defer parent.Close()
	port := freePort(t)
	m := &Manager{traffic: parent, trafficOn: true}
	m.taskProxy = newTaskProxyManager(t.TempDir(), &fakeTunnelLister{}, m.GlobalProxy, port, min(port+16, 65535), func() *traffic.Traffic { return parent })
	defer m.taskProxy.Close()
	s := &Server{m: m}
	addresses := map[string]bool{}
	for _, taskID := range []int64{1, 2} {
		ctx := agent.WithRunInfo(t.Context(), agent.RunInfo{TaskID: taskID, ExplorationID: taskID})
		tr, _, err := s.customHTTPTransport(ctx, httpExec{UseRecordingProxy: true}, nil, "https")
		if err != nil || tr == nil || tr.TLSClientConfig == nil || tr.TLSClientConfig.RootCAs == nil || tr.TLSClientConfig.InsecureSkipVerify {
			t.Fatalf("task recorder CA was not configured: %v %#v", err, tr)
		}
		u, err := tr.Proxy(httptest.NewRequest(http.MethodGet, "https://fixture.invalid/", nil))
		if err != nil || u == nil || addresses[u.String()] {
			t.Fatalf("task recorders were shared: %v %v", u, err)
		}
		addresses[u.String()] = true
		tr.CloseIdleConnections()
	}
}

func TestCustomHTTPInvalidProxyOrCAMustNotBecomeDirect(t *testing.T) {
	for _, proxy := range []string{"http://", "not-a-proxy", "file:///tmp/proxy", "http://%invalid"} {
		tr, err := customHTTPProxyTransport(proxy, "")
		if err == nil || tr != nil {
			t.Fatalf("invalid proxy became direct: %q", proxy)
		}
	}
	ca := filepath.Join(t.TempDir(), "invalid-ca.pem")
	if err := os.WriteFile(ca, []byte("not-a-certificate"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{ca, filepath.Join(t.TempDir(), "missing-ca.pem")} {
		if tr, err := customHTTPProxyTransport("http://127.0.0.1:22001", path); err == nil || tr != nil {
			t.Fatal("CA failure silently changed the route")
		}
	}
	tlsFixture := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	defer tlsFixture.Close()
	if err := os.WriteFile(ca, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: tlsFixture.Certificate().Raw}), 0o600); err != nil {
		t.Fatal(err)
	}
	tr, err := customHTTPProxyTransport("http://127.0.0.1:22001", ca)
	if err != nil || tr.TLSClientConfig == nil || tr.TLSClientConfig.InsecureSkipVerify {
		t.Fatal("valid CA did not retain normal TLS verification")
	}
	tr.CloseIdleConnections()
}

func TestCustomHTTPIncompleteResponseIsAnErrorAndFailedDirectCallDoesNotNeedManager(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Length", "100")
		_, _ = io.WriteString(w, "partial-fixture")
	}))
	defer srv.Close()
	res := customHTTPFixture(t, &Server{}, t.Context(), srv.URL, httpExec{}, nil)
	if !res.IsError || !strings.Contains(res.Flatten(), "partial-fixture") || !strings.Contains(res.Flatten(), "读取未完成") {
		t.Fatal("incomplete response was reported successful")
	}
	closed := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	address := closed.URL
	closed.Close()
	res = customHTTPFixture(t, &Server{}, t.Context(), address, httpExec{TimeoutMs: 100}, nil)
	if !res.IsError {
		t.Fatal("failed direct request was reported successful")
	}
}

func TestCustomHTTPEnvironmentKeysRespectProtocolAndExplicitEmpty(t *testing.T) {
	tc := &actool.ToolContext{Env: []string{"HTTP_PROXY=http://127.0.0.1:1", "HTTPS_PROXY=http://127.0.0.1:2", "SSL_CERT_FILE=/fixture/ca.pem"}}
	for _, scheme := range []string{"http", "https"} {
		addr, ca, present := customHTTPEnvProxy(tc, scheme)
		u, err := url.Parse(addr)
		if !present || err != nil || ca != "/fixture/ca.pem" || (scheme == "http" && u.Port() != "1") || (scheme == "https" && u.Port() != "2") {
			t.Fatalf("wrong host environment selection: %s %s %s %v", scheme, addr, ca, present)
		}
	}
	tc.Env = append(tc.Env, "HTTP_PROXY=")
	if addr, ca, present := customHTTPEnvProxy(tc, "http"); !present || addr != "" || ca != "" {
		t.Fatal("an explicit empty host route did not remain direct")
	}
}

// A stalled route lookup must obey the same timeout as the HTTP operation,
// rather than spending the manager's longer route deadline before noticing it.
type customHTTPBlockedRoute struct{}

func (customHTTPBlockedRoute) ListByTask(ctx context.Context, _ int64) ([]*db.TunnelRecord, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}

func TestCustomHTTPRouteLookupSharesRequestDeadline(t *testing.T) {
	m := &Manager{globalProxy: "http://127.0.0.1:1"}
	m.taskProxy = newTaskProxyManager(t.TempDir(), customHTTPBlockedRoute{}, m.GlobalProxy, 0, 0)
	defer m.taskProxy.Close()
	s := &Server{m: m}
	ctx := agent.WithRunInfo(t.Context(), agent.RunInfo{TaskID: 1, ExplorationID: 1})
	started := time.Now()
	res := customHTTPFixture(t, s, ctx, "http://fixture.invalid/", httpExec{UseRecordingProxy: true, TimeoutMs: 50}, nil)
	if !res.IsError || time.Since(started) > 3*time.Second {
		t.Fatal("route lookup escaped the custom HTTP request deadline")
	}
}
