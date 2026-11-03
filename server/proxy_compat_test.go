package server

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/Autumn-27/artex/agent"
	"github.com/Autumn-27/artex/db"
	"github.com/Autumn-27/artex/traffic"
	"github.com/Autumn-27/artex/tunnel"
)

type proxyCompatTunnelLister struct {
	recs map[int64][]*db.TunnelRecord
	err  error
}

func (l *proxyCompatTunnelLister) ListByTask(_ context.Context, taskID int64) ([]*db.TunnelRecord, error) {
	return l.recs[taskID], l.err
}

func proxyCompatTunnel(t *testing.T, state string) *db.TunnelRecord {
	t.Helper()
	plan, err := json.Marshal(tunnel.Plan{SocksPort: 22001})
	if err != nil {
		t.Fatal(err)
	}
	return &db.TunnelRecord{ID: 91, State: state, Kind: tunnel.KindSocks, DeployParams: plan}
}

func TestProxyCompatCaptureOffGlobalRouteTracksPoolWithoutLosingTaskTunnel(t *testing.T) {
	proxy := func(label string) *httptest.Server {
		return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, label) }))
	}
	a, b := proxy("a"), proxy("b")
	defer a.Close()
	defer b.Close()
	m := &Manager{globalProxy: a.URL}
	fp, err := newDynamicForwardProxy(m.GlobalProxy)
	if err != nil {
		t.Fatal(err)
	}
	m.forwardProxy = fp
	defer m.Close()
	lister := &proxyCompatTunnelLister{recs: map[int64][]*db.TunnelRecord{2: {proxyCompatTunnel(t, db.TunnelAlive)}}}
	m.taskProxy = newTaskProxyManager(t.TempDir(), lister, m.GlobalProxy, 0, 0)
	m.proxyPool = &proxyPoolState{active: "a", entries: []proxyPoolEntry{
		{ProxyPoolEntry: ProxyPoolEntry{ID: "a", Enabled: true}, RawURL: a.URL},
		{ProxyPoolEntry: ProxyPoolEntry{ID: "b", Enabled: true}, RawURL: b.URL},
	}}
	m.proxyPoolConfigured.Store(true)
	addr, ca, err := m.TaskProxyForTaskChecked(t.Context(), 1)
	if err != nil || addr != fp.Addr() || ca != "" {
		t.Fatalf("capture-off global route = %q/%q, %v", addr, ca, err)
	}
	u, _ := url.Parse(addr)
	tr := &http.Transport{Proxy: http.ProxyURL(u)}
	defer tr.CloseIdleConnections()
	client := &http.Client{Transport: tr, Timeout: time.Second}
	request := func(want string) {
		t.Helper()
		resp, err := client.Get("http://fixture.invalid/")
		if err != nil {
			t.Fatal(err)
		}
		body, readErr := io.ReadAll(resp.Body)
		resp.Body.Close()
		if readErr != nil || string(body) != want {
			t.Fatalf("same run route returned %q, %v; want %q", body, readErr, want)
		}
	}
	request("a")
	if err := m.activateProxyID("b", "manual"); err != nil {
		t.Fatal(err)
	}
	request("b")
	if addr, ca, err := m.TaskProxyForTaskChecked(t.Context(), 2); err != nil || addr != "socks5://127.0.0.1:22001" || ca != "" {
		t.Fatalf("task tunnel was replaced by global pool: %q/%q, %v", addr, ca, err)
	}
	if len(m.taskProxy.proxies) != 0 {
		t.Fatal("capture-off resolution created a recording listener")
	}
}

func TestProxyCompatPoolSwitchRefreshesExistingTaskMITMsButKeepsTunnels(t *testing.T) {
	m := &Manager{globalProxy: "http://127.0.0.1:18081"}
	lister := &proxyCompatTunnelLister{recs: map[int64][]*db.TunnelRecord{2: {proxyCompatTunnel(t, db.TunnelAlive)}}}
	m.taskProxy = newTaskProxyManager(t.TempDir(), lister, m.GlobalProxy, 0, 0)
	defer m.Close()
	for id, upstream := range map[int64]string{1: m.GlobalProxy(), 2: "socks5://127.0.0.1:22001"} {
		tr, err := traffic.Open(t.TempDir(), "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		if err := tr.SetUpstreamProxy(upstream); err != nil {
			t.Fatal(err)
		}
		m.taskProxy.proxies[id] = &taskProxyInst{tr: tr, upstream: upstream}
	}
	m.proxyPool = &proxyPoolState{active: "a", entries: []proxyPoolEntry{{ProxyPoolEntry: ProxyPoolEntry{ID: "b", Enabled: true}, RawURL: "http://127.0.0.1:18082"}}}
	if err := m.activateProxyID("b", "manual"); err != nil {
		t.Fatal(err)
	}
	if got := m.taskProxy.proxies[1].tr.UpstreamProxy(); got != m.GlobalProxy() {
		t.Fatalf("global task MITM kept stale upstream %q", got)
	}
	if got := m.taskProxy.proxies[2].tr.UpstreamProxy(); got != "socks5://127.0.0.1:22001" {
		t.Fatalf("tunnel task MITM was rerouted to %q", got)
	}
}

func TestProxyCompatGlobalFailureDoesNotProbeOrPauseIndependentOrUnknownTaskRoute(t *testing.T) {
	for _, tc := range []struct {
		name string
		recs []*db.TunnelRecord
		err  error
	}{
		{name: "alive tunnel", recs: []*db.TunnelRecord{proxyCompatTunnel(t, db.TunnelAlive)}},
		{name: "bound tunnel not ready", recs: []*db.TunnelRecord{proxyCompatTunnel(t, db.TunnelError)}},
		{name: "ledger unavailable", err: errors.New("fixture ledger unavailable")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := &Manager{globalProxy: "http://127.0.0.1:18081"}
			lister := &proxyCompatTunnelLister{recs: map[int64][]*db.TunnelRecord{7: tc.recs}, err: tc.err}
			m.taskProxy = newTaskProxyManager(t.TempDir(), lister, m.GlobalProxy, 0, 0)
			s := &Server{m: m, ctx: context.Background(), proxyProbe: func(context.Context, string, string, []string, time.Duration) egressProbeResult {
				t.Fatal("independent/unknown task route initiated a global probe")
				return egressProbeResult{}
			}}
			if s.taskUsesGlobalProxy("7") {
				t.Fatal("independent/unknown task classified as global proxy user")
			}
			s.handleProxyFailure("7", errors.New("proxyconnect: connection refused"))
			s.pauseProxyTask("7", errors.New("proxy down"))
		})
	}
}

func TestProxyCompatRuntimeFallbackRetainsAuthentication(t *testing.T) {
	const raw = "http://fixture-user:fixture-password@127.0.0.1:18081"
	m := &Manager{globalProxy: raw}
	if got := m.ProxyAddr(); got != raw {
		t.Fatalf("runtime proxy address was display-masked: %q", got)
	}
	if s := maskSecrets(m.ProxyAddr()); strings.Contains(s, "fixture-password") {
		t.Fatal("display mask does not redact runtime proxy credentials")
	}
}

func TestProxyCompatForwardProxyDefaultAddresses(t *testing.T) {
	for _, tc := range []struct{ raw, port, want string }{
		{"http://proxy.example", "80", "proxy.example:80"},
		{"https://proxy.example", "443", "proxy.example:443"},
		{"https://[::1]", "443", "[::1]:443"},
		{"https://[::1]:9443", "443", "[::1]:9443"},
		{"socks5://proxy.example", "1080", "proxy.example:1080"},
	} {
		u, err := url.Parse(tc.raw)
		if err != nil {
			t.Fatal(err)
		}
		if got := forwardProxyDialAddress(u, tc.port); got != tc.want {
			t.Errorf("address(%q)=%q, want %q", tc.raw, got, tc.want)
		}
	}
}

func TestProxyCompatPauseCancelsFindingChecksInsidePauseTransition(t *testing.T) {
	m := proxyTestManager(t)
	task, err := m.CreateTask("proxy finding cancellation fixture", "fixture", nil, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer m.DeleteTask(task.ID, DeleteTaskOptions{})
	m.mu.Lock()
	m.globalProxy = "http://127.0.0.1:18081"
	m.mu.Unlock()
	s := newAdmissionTestServer(m, nil)
	m.SetTaskControlMutex(&s.concMu)
	ctx, cancel := context.WithCancelCause(context.Background())
	defer cancel(context.Canceled)
	s.findingCheckTasks.Store(int64(713), task.ID)
	s.chatCancel[s.convBusyKey(713)] = cancel
	s.pauseProxyTask(task.ID, errors.New("proxy down"))
	if context.Cause(ctx) != agent.AbortPausedByProxy {
		t.Fatalf("finding verification survived proxy pause: %v", context.Cause(ctx))
	}
	if st := task.lifecycleSnapshot(); !st.Paused || st.PauseReason != "paused_by_proxy" {
		t.Fatalf("proxy pause was not persisted: %+v", st)
	}
}
