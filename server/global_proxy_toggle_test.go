package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"
)

func toggleProxyEntries() []proxyPoolEntry {
	return []proxyPoolEntry{
		{ProxyPoolEntry: ProxyPoolEntry{ID: "a", Enabled: true, Health: "healthy"}, RawURL: "http://a.fixture.invalid:8080"},
		{ProxyPoolEntry: ProxyPoolEntry{ID: "b", Enabled: true, Health: "healthy"}, RawURL: "http://b.fixture.invalid:8080"},
	}
}

func TestProxyGlobalToggleKeepsMembersAndSurvivesStartup(t *testing.T) {
	entries := toggleProxyEntries()
	m := &Manager{globalProxy: entries[0].RawURL, proxyPool: &proxyPoolState{entries: entries, active: "a", generation: 7}}
	if err := m.SetGlobalProxy(""); err != nil {
		t.Fatal(err)
	}
	if !m.proxyPool.disabled || m.GlobalProxy() != "" || m.proxyPool.active != "" || m.proxyPool.generation != 8 || !reflect.DeepEqual(m.proxyPoolEntries(), entries) {
		t.Fatal("disabling changed membership or failed to invalidate the old route")
	}
	if m.rotateProxy("timed") {
		t.Fatal("automatic rotation re-enabled the global proxy")
	}
	if err := m.persistProxyPoolLocked(); err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(proxyPoolPersist{Entries: m.proxyPool.entries, Active: m.proxyPool.active, Disabled: m.proxyPool.disabled})
	if err != nil {
		t.Fatal(err)
	}
	pool, route, persist, err := decodeProxyPoolStartup(map[string]string{settingProxyPool: string(raw), settingGlobalProxy: entries[0].RawURL, settingProxyPoolActive: "a"})
	if err != nil || !persist || !pool.Disabled || pool.Active != "" || route != "" || len(pool.Entries) != 2 || !pool.Entries[0].Enabled {
		t.Fatalf("startup restored disabled routing: pool=%+v route=%q err=%v", pool, route, err)
	}
	if err := m.SetGlobalProxyEnabled(true); err != nil {
		t.Fatal(err)
	}
	if m.proxyPool.disabled || m.GlobalProxy() != entries[0].RawURL || m.proxyPool.active != "a" {
		t.Fatal("explicit enable did not restore an available pool route")
	}
	if err := m.SetGlobalProxyEnabled(false); err != nil {
		t.Fatal(err)
	}
	if err := m.activateProxyID("b", "manual"); err != nil {
		t.Fatal(err)
	}
	if m.proxyPool.disabled || m.GlobalProxy() != entries[1].RawURL || m.proxyPool.active != "b" {
		t.Fatal("explicit node activation did not enable the requested route")
	}
}

func TestProxyGlobalDisabledPoolMutationsAndHealthStayDirect(t *testing.T) {
	for _, action := range []string{"import", "edit", "delete", "bulk-disable", "bulk-delete", "probe"} {
		t.Run(action, func(t *testing.T) {
			entries := toggleProxyEntries()
			m := &Manager{globalProxy: entries[0].RawURL, proxyPool: &proxyPoolState{entries: entries, active: "a"}}
			if err := m.SetGlobalProxyEnabled(false); err != nil {
				t.Fatal(err)
			}
			s := &Server{m: m, ctx: context.Background(), proxyPoolWake: make(chan struct{}, 1)}
			s.proxyProbe = func(context.Context, string, string, []string, time.Duration) egressProbeResult {
				return egressProbeResult{OK: true, ExitIP: "192.0.2.5"}
			}
			w := httptest.NewRecorder()
			switch action {
			case "import":
				s.importProxyPool(w, httptest.NewRequest(http.MethodPost, "/api/settings/proxy-pool/import?summary=1", strings.NewReader(`{"text":"http://c.fixture.invalid:8080"}`)))
			case "edit":
				r := httptest.NewRequest(http.MethodPut, "/api/settings/proxy-pool/a", strings.NewReader(`{"url":"http://edited.fixture.invalid:8080"}`))
				r.SetPathValue("id", "a")
				s.updateProxyPool(w, r)
			case "delete":
				r := httptest.NewRequest(http.MethodDelete, "/api/settings/proxy-pool/a", nil)
				r.SetPathValue("id", "a")
				s.deleteProxyPool(w, r)
			case "bulk-disable", "bulk-delete":
				s.bulkProxyPool(w, httptest.NewRequest(http.MethodPost, "/api/settings/proxy-pool/bulk", strings.NewReader(`{"action":"`+strings.TrimPrefix(action, "bulk-")+`","ids":["a"]}`)))
			case "probe":
				s.proxyPoolTickLocked(true)
			}
			if w.Code != http.StatusOK || m.GlobalProxy() != "" || m.proxyPool.active != "" || !m.proxyPool.disabled {
				t.Fatalf("%s restored disabled route: status=%d route=%q active=%q disabled=%v", action, w.Code, m.GlobalProxy(), m.proxyPool.active, m.proxyPool.disabled)
			}
			if s.reconcileProxyPoolAfterProbe(true) || s.handleProxyPoolFailure(errors.New("proxyconnect tcp: connection refused")) {
				t.Fatal("background recovery re-enabled disabled routing")
			}
		})
	}
}

func TestProxyGlobalToggleRejectsUnavailableNodeWithoutEnabling(t *testing.T) {
	entries := toggleProxyEntries()
	entries[0].Enabled = false
	entries[1].Health = "unhealthy-cooling"
	m := &Manager{proxyPool: &proxyPoolState{entries: entries, disabled: true, generation: 5}}
	if err := m.SetGlobalProxyEnabled(true); err == nil {
		t.Fatal("enable accepted a pool without available members")
	}
	if err := m.activateProxyID("missing", "manual"); err == nil {
		t.Fatal("activation accepted an unknown member")
	}
	if !m.proxyPool.disabled || m.GlobalProxy() != "" || m.proxyPool.active != "" || m.proxyPool.generation != 5 {
		t.Fatal("failed enable changed disabled state")
	}
}

func TestProxyGlobalTogglePostgresRestartAndTimedRotation(t *testing.T) {
	m := proxyTestManager(t)
	entries := toggleProxyEntries()
	m.proxyPool = &proxyPoolState{entries: entries, active: "a"}
	m.globalProxy = entries[0].RawURL
	if err := m.SetProxyPoolSettings(true, 3, true, "sequential", 3, 60, 30); err != nil {
		t.Fatal(err)
	}
	if err := m.SetGlobalProxyEnabled(false); err != nil {
		t.Fatal(err)
	}
	s := &Server{m: m, ctx: context.Background()}
	s.proxyProbe = func(context.Context, string, string, []string, time.Duration) egressProbeResult {
		return egressProbeResult{OK: true, ExitIP: "192.0.2.8"}
	}
	s.proxyPoolTickLocked(true)
	if m.GlobalProxy() != "" || m.proxyPool.active != "" {
		t.Fatal("enabled timed rotation overrode manual disable")
	}
	if err := m.initProxyPool(); err != nil {
		t.Fatal(err)
	}
	timed, _, onFailure, _ := m.proxyRotateConfig()
	if !m.proxyPool.disabled || m.GlobalProxy() != "" || len(m.proxyPoolEntries()) != len(entries) || !timed || !onFailure {
		t.Fatal("database reload lost disable intent, entries, or rotation preferences")
	}
	if err := m.SetGlobalProxyEnabled(true); err != nil {
		t.Fatal(err)
	}
	if err := m.initProxyPool(); err != nil {
		t.Fatal(err)
	}
	if m.proxyPool.disabled || m.GlobalProxy() != entries[0].RawURL {
		t.Fatal("database reload lost explicit re-enable")
	}
}

func TestProxyGlobalDisableInvalidatesInflightHealthRound(t *testing.T) {
	entries := toggleProxyEntries()[:1]
	m := &Manager{globalProxy: entries[0].RawURL, proxyPool: &proxyPoolState{entries: entries, active: "a"}}
	started, release, done := make(chan struct{}), make(chan struct{}, 1), make(chan struct{})
	defer close(release)
	s := &Server{m: m, ctx: context.Background()}
	s.proxyProbe = func(context.Context, string, string, []string, time.Duration) egressProbeResult {
		close(started)
		<-release
		return egressProbeResult{OK: true, ExitIP: "192.0.2.8"}
	}
	go func() { defer close(done); s.proxyPoolTick() }()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("health probe did not start")
	}
	if err := m.SetGlobalProxyEnabled(false); err != nil {
		t.Fatal(err)
	}
	release <- struct{}{}
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("superseded health round did not return")
	}
	if !m.proxyPool.disabled || m.GlobalProxy() != "" || m.proxyPool.active != "" || !m.proxyPool.entries[0].LastCheckedAt.IsZero() {
		t.Fatal("late probe published stale health or re-enabled the route")
	}
}

func TestProxyGlobalToggleSettingsAPI(t *testing.T) {
	m := proxyTestManager(t)
	entries := toggleProxyEntries()
	m.proxyPool = &proxyPoolState{entries: entries, active: "a"}
	m.globalProxy = entries[0].RawURL
	s := &Server{m: m}
	for _, body := range []string{`{"global_proxy_enabled":false}`, `{"global_proxy_enabled":true}`, `{"global_proxy":""}`} {
		w := httptest.NewRecorder()
		s.putSettings(w, httptest.NewRequest(http.MethodPut, "/api/settings", strings.NewReader(body)))
		if w.Code != http.StatusOK {
			t.Fatalf("settings %s: status=%d body=%s", body, w.Code, w.Body.String())
		}
		var snapshot struct {
			Enabled bool             `json:"global_proxy_enabled"`
			Proxy   string           `json:"global_proxy"`
			Active  string           `json:"proxy_pool_active_id"`
			Entries []ProxyPoolEntry `json:"proxy_pool"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &snapshot); err != nil {
			t.Fatal(err)
		}
		wantEnabled := strings.Contains(body, ":true")
		if snapshot.Enabled != wantEnabled || (snapshot.Proxy != "") != wantEnabled || (snapshot.Active != "") != wantEnabled || len(snapshot.Entries) != len(entries) {
			t.Fatalf("inconsistent settings response after %s: %+v", body, snapshot)
		}
	}
	w := httptest.NewRecorder()
	s.putSettings(w, httptest.NewRequest(http.MethodPut, "/api/settings", strings.NewReader(`{"global_proxy_enabled":true,"global_proxy":""}`)))
	if w.Code != http.StatusBadRequest || m.GlobalProxy() != "" || !m.proxyPool.disabled {
		t.Fatal("conflicting settings request changed the route")
	}
}
