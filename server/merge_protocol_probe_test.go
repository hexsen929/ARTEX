package server

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestMergeProtocolProbeConfiguredServiceNeverContactsDefaults(t *testing.T) {
	for _, custom := range []string{"http://10.0.0.5/ip", "https://probe.fixture.invalid/ip"} {
		t.Run(custom, func(t *testing.T) {
			m := proxyTestManager(t)
			preserveEgressProbeSetting(t, m)
			if err := m.pg.SetSetting(settingEgressProbeURL, custom); err != nil {
				t.Fatal(err)
			}
			entry := proxyPoolEntry{ProxyPoolEntry: ProxyPoolEntry{ID: "auto-custom", Enabled: true, ProtocolAuto: true}, RawURL: "http://proxy.fixture.invalid:8080"}
			m.proxyPool = &proxyPoolState{entries: []proxyPoolEntry{entry}, disabled: true}
			s := &Server{m: m}
			var mu sync.Mutex
			calls := 0
			s.proxySOCKS4Probe = func(context.Context, string) (bool, string) {
				t.Error("custom selection sent public SOCKS4 CONNECT")
				return false, "unexpected"
			}
			s.proxyProbe = func(_ context.Context, raw, ca string, probes []string, _ time.Duration) egressProbeResult {
				mu.Lock()
				calls++
				mu.Unlock()
				if !reflect.DeepEqual(probes, []string{custom}) || !strings.Contains(raw, "proxy.fixture.invalid:8080") || ca != "" {
					t.Error("configured service boundary or TLS trust changed", raw, probes)
				}
				return egressProbeResult{OK: true, ExitIP: "192.0.2.9", Probes: []egressAttempt{}}
			}
			result, resolved := s.probePoolEntry(t.Context(), entry, []string{custom})
			if result.OK || resolved != "" || calls != 3 || len(result.ProtocolChecks) != 4 {
				t.Fatal("one configured website scheme became a complete proxy", result, resolved, calls)
			}
			for _, check := range result.ProtocolChecks[:3] {
				if check.HTTP.Checked != strings.HasPrefix(custom, "http://") || check.HTTPS.Checked != strings.HasPrefix(custom, "https://") {
					t.Fatal("unrequested scheme was checked", check)
				}
			}
			calls = 0
			w := httptest.NewRecorder()
			r := httptest.NewRequest("POST", "/diagnose", nil)
			r.SetPathValue("id", entry.ID)
			s.diagnoseProxyPoolEntry(w, r)
			var report struct {
				Mode        string `json:"probe_mode"`
				URL         string `json:"probe_url"`
				HTTPS, HTTP egressProbeResult
				HTTPSkipped bool `json:"http_skipped"`
			}
			if err := json.Unmarshal(w.Body.Bytes(), &report); err != nil {
				t.Fatal(err)
			}
			if w.Code != 200 || report.Mode != "configured" || report.URL != custom || !report.HTTPS.OK || !report.HTTPSkipped || report.HTTP.OK || calls != 3 || len(report.HTTPS.ProtocolChecks) != 4 {
				t.Fatal("auto diagnostic lost configured selection", w.Code, w.Body.String(), calls)
			}
			if !reflect.DeepEqual(m.proxyPoolEntries(), []proxyPoolEntry{entry}) || m.GlobalProxy() != "" {
				t.Fatal("read-only diagnosis changed the route")
			}
		})
	}
}

func TestMergeProtocolProbeServiceChangeDiscardsInFlightAutoRound(t *testing.T) {
	m := proxyTestManager(t)
	preserveEgressProbeSetting(t, m)
	const previous = "https://first.fixture.invalid/ip"
	if err := m.pg.SetSetting(settingEgressProbeURL, previous); err != nil {
		t.Fatal(err)
	}
	entry := proxyPoolEntry{ProxyPoolEntry: ProxyPoolEntry{ID: "auto-race", Enabled: true, ProtocolAuto: true}, RawURL: "http://proxy.fixture.invalid:8080"}
	m.proxyPool = &proxyPoolState{entries: []proxyPoolEntry{entry}, disabled: true}
	s := &Server{m: m, proxyPoolWake: make(chan struct{}, 1)}
	started, release := make(chan struct{}, 3), make(chan struct{})
	s.proxyProbe = func(ctx context.Context, _, _ string, probes []string, _ time.Duration) egressProbeResult {
		if !reflect.DeepEqual(probes, []string{previous}) {
			t.Error("round replaced its selected service")
		}
		started <- struct{}{}
		select {
		case <-release:
		case <-ctx.Done():
		}
		return egressProbeResult{OK: true, ExitIP: "192.0.2.10", Probes: []egressAttempt{}}
	}
	s.proxySOCKS4Probe = func(context.Context, string) (bool, string) {
		t.Error("unexpected public SOCKS4 check")
		return false, "fixture"
	}
	done := make(chan struct{})
	go func() { defer close(done); s.proxyPoolTickLocked(true) }()
	t.Cleanup(func() {
		select {
		case <-release:
		default:
			close(release)
		}
		<-done
	})
	for range 3 {
		select {
		case <-started:
		case <-time.After(3 * time.Second):
			t.Fatal("protocol candidates did not start")
		}
	}
	if err := m.pg.SetSetting(settingEgressProbeURL, "https://second.fixture.invalid/ip"); err != nil {
		t.Fatal(err)
	}
	s.invalidateEgressProbeSelection()
	close(release)
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("superseded round did not finish")
	}
	stored := m.proxyPoolEntries()[0]
	if !stored.LastCheckedAt.IsZero() || stored.DetectedProtocol != "" || len(stored.ProtocolChecks) != 0 || m.GlobalProxy() != "" || !m.proxyPool.forceHealth {
		t.Fatal("old service published health or lost retry", stored)
	}
	if m.proxyPool.probeRound.State != "superseded" {
		t.Fatal("progress claimed success for superseded service", m.proxyPool.probeRound.State)
	}
}
