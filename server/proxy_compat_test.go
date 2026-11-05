package server

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
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

func proxyCompatClosedManager(t *testing.T) *Manager {
	t.Helper()
	closed, err := sql.Open("pgx", "postgres://proxy-fixture.invalid/unused")
	if err != nil {
		t.Fatal(err)
	}
	if err := closed.Close(); err != nil {
		t.Fatal(err)
	}
	m := &Manager{pg: &db.DB{DB: closed}, globalProxy: "http://127.0.0.1:18081", proxyPool: &proxyPoolState{
		active: "a", entries: []proxyPoolEntry{
			{ProxyPoolEntry: ProxyPoolEntry{ID: "a", Enabled: true, Health: "healthy"}, RawURL: "http://127.0.0.1:18081"},
			{ProxyPoolEntry: ProxyPoolEntry{ID: "b", Enabled: true, Health: "healthy"}, RawURL: "http://127.0.0.1:18082"},
		},
	}}
	m.proxyPoolConfigured.Store(true)
	return m
}

func TestProxyCompatReadAndWriteFailureKeepOriginalRoute(t *testing.T) {
	for _, action := range []string{"startup read", "activate", "replace", "edit", "delete"} {
		t.Run(action, func(t *testing.T) {
			m := proxyCompatClosedManager(t)
			entries, active, logs := m.ProxyPoolSnapshot()
			original := m.GlobalProxy()
			var err error
			s := &Server{m: m}
			switch action {
			case "startup read":
				err = m.initProxyPool()
			case "activate":
				err = m.activateProxyID("b", "manual")
			case "replace":
				err = m.setProxyPoolEntries([]proxyPoolEntry{{ProxyPoolEntry: ProxyPoolEntry{ID: "replacement", Enabled: true}, RawURL: "http://127.0.0.1:18083"}}, "replacement")
			case "edit":
				r := httptest.NewRequest(http.MethodPatch, "/api/settings/proxy-pool/a", strings.NewReader(`{"url":"http://127.0.0.1:18083"}`))
				r.SetPathValue("id", "a")
				w := httptest.NewRecorder()
				s.updateProxyPool(w, r)
				if w.Code < 400 {
					t.Fatalf("failed DB accepted mutation: %d %s", w.Code, w.Body.String())
				}
				err = errors.New("expected API persistence failure")
			case "delete":
				r := httptest.NewRequest(http.MethodDelete, "/api/settings/proxy-pool/a", nil)
				r.SetPathValue("id", "a")
				w := httptest.NewRecorder()
				s.deleteProxyPool(w, r)
				if w.Code < 400 {
					t.Fatalf("failed DB accepted deletion: %d %s", w.Code, w.Body.String())
				}
				err = errors.New("expected API persistence failure")
			}
			if err == nil {
				t.Fatal("database failure became success")
			}
			after, afterActive, afterLogs := m.ProxyPoolSnapshot()
			if !reflect.DeepEqual(entries, after) || active != afterActive || !reflect.DeepEqual(logs, afterLogs) || m.GlobalProxy() != original || !m.proxyPoolConfigured.Load() {
				t.Fatalf("failed mutation changed original state: %+v, active=%s, route=%s", after, afterActive, m.GlobalProxy())
			}
		})
	}
}

func TestProxyCompatInvalidStartupPoolIsNotAnEmptyPool(t *testing.T) {
	for _, raw := range []string{"", "null", "[]", "{}", `{"entries":{}}`, `{"entries":[{"id":"x","raw_url":"ftp://fixture"}]}`, `{"entries":[{"id":"x"},{"id":"x"}]}`} {
		if _, _, _, err := decodeProxyPoolStartup(map[string]string{settingProxyPool: raw, settingGlobalProxy: "http://127.0.0.1:18081"}); err == nil {
			t.Errorf("invalid pool treated as cleared: %q", raw)
		}
	}
	for _, raw := range []string{`{"entries":[],"active":""}`, `{"entries":[{"id":"x","enabled":false,"raw_url":"http://127.0.0.1:18081"}],"active":"x"}`} {
		pool, route, persist, err := decodeProxyPoolStartup(map[string]string{settingProxyPool: raw, settingGlobalProxy: "http://stale.example:8080", settingProxyPoolActive: "stale"})
		if err != nil || route != "" || pool.Active != "" || !persist {
			t.Errorf("explicit cleared/disabled pool was not respected: %+v %q %v", pool, route, err)
		}
	}
	pool, route, persist, err := decodeProxyPoolStartup(map[string]string{settingGlobalProxy: "http://user:fixture-password@127.0.0.1:18081"})
	if err != nil || pool.Active != "proxy-legacy" || route != pool.Entries[0].RawURL || !persist {
		t.Fatalf("legacy proxy was not migrated intact: %+v %q %v", pool, route, err)
	}
}

func TestProxyCompatRoutePersistenceIsAtomicOnPostgresRejection(t *testing.T) {
	m := proxyTestManager(t)
	keys := []string{settingProxyPool, settingProxyPoolActive, settingGlobalProxy}
	old := map[string]string{}
	present := map[string]bool{}
	for _, key := range keys {
		value, ok, err := m.pg.GetSetting(key)
		if err != nil {
			t.Fatal(err)
		}
		old[key], present[key] = value, ok
	}
	defer func() {
		for _, key := range keys {
			if present[key] {
				_ = m.pg.SetSetting(key, old[key])
			} else {
				_ = m.pg.DeleteSetting(key)
			}
		}
	}()
	m.mu.Lock()
	m.globalProxy = "http://127.0.0.1:18081"
	m.mu.Unlock()
	m.proxyPool = &proxyPoolState{active: "a", entries: []proxyPoolEntry{
		{ProxyPoolEntry: ProxyPoolEntry{ID: "a", Enabled: true, Health: "healthy"}, RawURL: "http://127.0.0.1:18081"},
		{ProxyPoolEntry: ProxyPoolEntry{ID: "b", Enabled: true, Health: "healthy"}, RawURL: "http://127.0.0.1:18082"},
	}}
	m.proxyPool.mu.Lock()
	err := m.persistProxyPoolLocked()
	m.proxyPool.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	before := map[string]string{}
	for _, key := range keys {
		value, _, err := m.pg.GetSetting(key)
		if err != nil {
			t.Fatal(err)
		}
		before[key] = value
	}
	name := fmt.Sprintf("artex_proxy_reject_%d", time.Now().UnixNano())
	if _, err = m.pg.Exec(`CREATE FUNCTION ` + name + `() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN IF NEW.key='proxy_pool_entries' THEN RAISE EXCEPTION 'fixture reject route'; END IF; RETURN NEW; END $$`); err != nil {
		t.Fatal(err)
	}
	defer m.pg.Exec(`DROP FUNCTION IF EXISTS ` + name + `()`)
	if _, err = m.pg.Exec(`CREATE TRIGGER ` + name + ` BEFORE INSERT OR UPDATE ON settings FOR EACH ROW EXECUTE FUNCTION ` + name + `()`); err != nil {
		t.Fatal(err)
	}
	defer m.pg.Exec(`DROP TRIGGER IF EXISTS ` + name + ` ON settings`)
	if err = m.activateProxyID("b", "manual"); err == nil {
		t.Fatal("rejected atomic route write was reported successful")
	}
	if err = m.SetGlobalProxyEnabled(false); err == nil {
		t.Fatal("rejected global proxy disable was reported successful")
	}
	if m.proxyPool.disabled {
		t.Fatal("rejected disable changed global proxy intent")
	}
	if m.GlobalProxy() != "http://127.0.0.1:18081" || m.proxyPoolActiveID() != "a" {
		t.Fatal("rejected atomic route write changed runtime")
	}
	for _, key := range keys {
		value, _, err := m.pg.GetSetting(key)
		if err != nil || value != before[key] {
			t.Errorf("partial persistent route for %s: %q err=%v", key, value, err)
		}
	}
}

func TestProxyCompatCloseJoinsReconnectProbe(t *testing.T) {
	m := &Manager{globalProxy: "http://127.0.0.1:18081", tasks: map[string]*Task{"7": {ID: "7", Status: "running", Paused: true, PauseReason: "paused_by_proxy"}}}
	entered, finished := make(chan struct{}), make(chan struct{})
	s := &Server{m: m, ctx: context.Background(), proxyProbe: func(ctx context.Context, _ string, _ string, _ []string, _ time.Duration) egressProbeResult {
		close(entered)
		<-ctx.Done()
		close(finished)
		return egressProbeResult{Error: "fixture cancelled"}
	}}
	s.signalProxySupervisor()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("reconnect probe did not begin")
	}
	done := make(chan struct{})
	go func() { s.Close(); close(done) }()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Close did not cancel and join the reconnect probe")
	}
	select {
	case <-finished:
	default:
		t.Fatal("Close returned while reconnect probe still ran")
	}
	// A late callback may signal, but must not recreate a shutdown supervisor.
	s.signalProxySupervisor()
	s.Close()
}

func TestProxyCompatCloseJoinsTrafficCallbackAndRejectsLateCallbacks(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	entered := make(chan struct{})
	s := &Server{m: &Manager{globalProxy: "http://127.0.0.1:18081"}, ctx: context.Background(), proxyCtx: ctx, proxyCancel: cancel,
		proxyProbe: func(ctx context.Context, _ string, _ string, _ []string, _ time.Duration) egressProbeResult {
			close(entered)
			<-ctx.Done()
			return egressProbeResult{Error: "fixture cancelled"}
		},
	}
	callbackDone := make(chan struct{})
	go func() {
		s.handleProxyTrafficFailure(errors.New("proxyconnect: connection refused"))
		close(callbackDone)
	}()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("traffic callback did not start probe")
	}
	closed := make(chan struct{})
	go func() { s.Close(); close(closed) }()
	select {
	case <-closed:
	case <-time.After(time.Second):
		t.Fatal("Close did not join the admitted traffic callback")
	}
	select {
	case <-callbackDone:
	case <-time.After(time.Second):
		t.Fatal("traffic callback survived Close")
	}
	s.handleProxyTrafficFailure(errors.New("proxyconnect: connection refused"))
	if s.beginProxyCallback() {
		s.proxyCallbackWG.Done()
		t.Fatal("late callback admission succeeded after Close")
	}
}

func TestProxyCompatCloseDiscardsCancelledPoolProbeBeforeRouteReconciliation(t *testing.T) {
	const first = "http://127.0.0.1:18081"
	const second = "http://127.0.0.1:18082"
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	m := &Manager{globalProxy: first, tasks: map[string]*Task{}, proxyPool: &proxyPoolState{
		active: "a", entries: []proxyPoolEntry{
			// A cancelled probe would cross the default failure threshold and
			// reroute to b if shutdown results were published as real failures.
			{ProxyPoolEntry: ProxyPoolEntry{ID: "a", Enabled: true, Health: "healthy", ConsecutiveFailures: defaultProxyHealthThreshold - 1}, RawURL: first},
			{ProxyPoolEntry: ProxyPoolEntry{ID: "b", Enabled: true, Health: "healthy"}, RawURL: second},
		},
	}}
	m.proxyPoolConfigured.Store(true)
	started, cancelled := make(chan struct{}), make(chan struct{})
	s := &Server{m: m, ctx: context.Background(), proxyCtx: ctx, proxyCancel: cancel,
		proxyProbe: func(ctx context.Context, raw, _ string, _ []string, _ time.Duration) egressProbeResult {
			if raw == first {
				close(started)
				<-ctx.Done()
				close(cancelled)
				return egressProbeResult{Error: "fixture cancelled"}
			}
			return egressProbeResult{OK: true, ExitIP: "192.0.2.2"}
		},
	}
	before, active, logs := m.ProxyPoolSnapshot()
	s.proxyPoolWG.Add(1)
	tickDone := make(chan struct{})
	go func() {
		defer s.proxyPoolWG.Done()
		defer close(tickDone)
		s.proxyPoolTick()
	}()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("pool probe did not begin")
	}
	closed := make(chan struct{})
	go func() { s.Close(); close(closed) }()
	select {
	case <-closed:
	case <-time.After(time.Second):
		t.Fatal("Close did not cancel and join pool tick")
	}
	select {
	case <-cancelled:
	default:
		t.Fatal("Close returned before the blocked probe observed cancellation")
	}
	select {
	case <-tickDone:
	default:
		t.Fatal("Close returned before pool tick completed")
	}
	after, afterActive, afterLogs := m.ProxyPoolSnapshot()
	if !reflect.DeepEqual(before[0], after[0]) {
		t.Fatalf("shutdown cancellation was published as proxy health: before=%+v after=%+v", before[0], after[0])
	}
	if afterActive != active || m.GlobalProxy() != first || !reflect.DeepEqual(logs, afterLogs) {
		t.Fatalf("pool tick switched route during Close: active=%s route=%s logs=%+v", afterActive, m.GlobalProxy(), afterLogs)
	}
	// No late tick may probe or publish after Close has completed.
	s.proxyPoolTick()
	latest, latestActive, latestLogs := m.ProxyPoolSnapshot()
	if !reflect.DeepEqual(after, latest) || latestActive != afterActive || !reflect.DeepEqual(afterLogs, latestLogs) {
		t.Fatal("late pool tick mutated the shutdown snapshot")
	}
}

func TestProxyCompatRecoveryRejectsShutdownBeforeLifecycleAdmission(t *testing.T) {
	for _, mode := range []string{"cancelled child context", "closed server"} {
		t.Run(mode, func(t *testing.T) {
			task := &Task{ID: "7", Status: "running", Paused: true, PauseReason: "paused_by_proxy", ReconnectGeneration: 9, ReconnectAttempt: 2, ReconnectMax: 5}
			// No PG is needed: rejecting shutdown must happen before any new
			// lifecycle reads or writes, including the calendar/clear-reason path.
			s := &Server{m: &Manager{tasks: map[string]*Task{"7": task}}}
			if mode == "cancelled child context" {
				ctx, cancel := context.WithCancel(context.Background())
				cancel()
				s.proxyCtx = ctx
			} else {
				s.Close()
			}
			before := task.lifecycleSnapshot()
			called := false
			s.proxyReconnectSuccessWithGuards(map[string]proxyReconnectGuard{"7": {generation: 9}}, func() { called = true })
			if called || !reflect.DeepEqual(before, task.lifecycleSnapshot()) {
				t.Fatalf("shutdown admitted recovery: before=%+v after=%+v hook=%v", before, task.lifecycleSnapshot(), called)
			}
		})
	}
}
