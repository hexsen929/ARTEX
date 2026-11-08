package server

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestProxyProtocolHTTPResponsesDoNotProveHTTPSOrProxyIdentity(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		status     int
		recognized bool
		code       string
	}{
		{"echo_only", `{"ip":"192.0.2.25"}`, 405, false, "proxy_connect_rejected"},
		{"empty_200", "", 400, false, "proxy_connect_rejected"},
		{"authentication", "", 407, true, "proxy_authentication"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodConnect {
					w.WriteHeader(tc.status)
					return
				}
				_, _ = io.WriteString(w, tc.body)
			}))
			defer proxy.Close()
			https := probeEgress(t.Context(), proxy.URL, "", []string{"https://echo.invalid/ip"}, time.Second)
			plain := probeEgress(t.Context(), proxy.URL, "", []string{"http://echo.invalid/ip"}, time.Second)
			if https.OK || https.ProtocolRecognized != tc.recognized || https.ErrorCode != tc.code {
				t.Fatal(https)
			}
			if plain.ProtocolRecognized || plain.OK != (tc.body != "") {
				t.Fatal("HTTP echo falsely proved proxy identity", plain)
			}
		})
	}
}

func TestProxyProtocolSOCKS4RemainsUnsupportedWithoutExternalProbe(t *testing.T) {
	var calls atomic.Int32
	s := &Server{proxySOCKS4Probe: func(context.Context, string) (bool, string) {
		t.Fatal("unsupported protocol sent external probe")
		return false, ""
	}, proxyProbe: func(context.Context, string, string, []string, time.Duration) egressProbeResult {
		calls.Add(1)
		return egressProbeResult{Error: "fixture", ErrorCode: "proxy_transport", Probes: []egressAttempt{}}
	}}
	result, raw := s.probePoolEntry(t.Context(), proxyPoolEntry{ProxyPoolEntry: ProxyPoolEntry{ProtocolAuto: true}, RawURL: "http://proxy.invalid:8080"})
	if result.OK || raw != "" || len(result.ProtocolChecks) != 4 || result.ProtocolChecks[3].Supported || result.ProtocolChecks[3].Recognized || calls.Load() != 3 {
		t.Fatal(result, raw, calls)
	}
}

func capabilityFixtureServer(entry proxyPoolEntry) *Server {
	return &Server{
		m:                &Manager{proxyPool: &proxyPoolState{entries: []proxyPoolEntry{entry}, disabled: true}},
		proxySOCKS4Probe: func(context.Context, string) (bool, string) { return false, "无 SOCKS4 响应" },
		proxyProbe: func(ctx context.Context, raw, ca string, probes []string, timeout time.Duration) egressProbeResult {
			if strings.HasPrefix(raw, "socks5://") {
				if strings.HasPrefix(probes[0], "http://") {
					return egressProbeResult{OK: true, ProtocolRecognized: true, Via: "socks5", ExitIP: "192.0.2.9", Probes: []egressAttempt{}}
				}
				return egressProbeResult{ProtocolRecognized: true, Via: "socks5", Error: "TLS 证书链不受信任", ErrorCode: "certificate_untrusted", Probes: []egressAttempt{{URL: "https://echo.invalid/ip", Error: "TLS 证书链不受信任", ErrorCode: "certificate_untrusted"}}}
			}
			return egressProbeResult{Error: "代理连接被拒绝", ErrorCode: "connection_refused", Probes: []egressAttempt{}}
		},
	}
}

func TestProxyProtocolRecognitionAndFailureDetailsPersistWithoutRouteActivation(t *testing.T) {
	entry := proxyPoolEntry{ProxyPoolEntry: ProxyPoolEntry{ID: "capabilities", ProtocolAuto: true, Enabled: true, Health: "unknown"}, RawURL: "http://user:fixture-secret@proxy.invalid:8080"}
	s := capabilityFixtureServer(entry)
	probe := s.proxyProbe
	s.proxyProbe = func(ctx context.Context, raw, ca string, probes []string, timeout time.Duration) egressProbeResult {
		if strings.HasPrefix(probes[0], "http://") {
			return egressProbeResult{Error: "HTTP 不可用", ErrorCode: "proxy_transport", Probes: []egressAttempt{}}
		}
		return probe(ctx, raw, ca, probes, timeout)
	}
	result, raw := s.probePoolEntry(t.Context(), entry)
	if result.OK || raw != "" || result.ErrorCode != "certificate_untrusted" || result.Via != "socks5" {
		t.Fatal(result, raw)
	}
	s.m.proxyPool.mu.Lock()
	confirmed, err := s.m.applyProxyProbeBatchLocked([]proxyBatchProbe{{entry: entry, result: result, complete: true, resolvedURL: raw}}, 3)
	s.m.proxyPool.mu.Unlock()
	if err != nil || confirmed {
		t.Fatal(err, confirmed)
	}
	stored := s.m.proxyPoolEntries()[0]
	if len(stored.ProtocolChecks) != 4 || !stored.ProtocolChecks[2].Recognized || stored.ProtocolChecks[2].HTTP.OK || stored.ProtocolChecks[2].HTTPS.OK || proxyEntryUsable(stored) || s.m.GlobalProxy() != "" {
		t.Fatal(stored)
	}
	if stored.ProtocolChecks[0].HTTPS.ErrorCode != "connection_refused" || stored.LastErrorCode != "certificate_untrusted" {
		t.Fatal("candidate reasons lost", stored)
	}
	public, _, _ := s.m.ProxyPoolSnapshot()
	publicJSON, _ := json.Marshal(public)
	if strings.Contains(string(publicJSON), "fixture-secret") {
		t.Fatal("credential in public checks")
	}
	persisted, _ := json.Marshal(proxyPoolPersist{Entries: []proxyPoolEntry{stored}, Disabled: true})
	pool, global, _, err := decodeProxyPoolStartup(map[string]string{settingProxyPool: string(persisted)})
	if err != nil || global != "" || len(pool.Entries[0].ProtocolChecks) != 4 || proxyEntryUsable(pool.Entries[0]) {
		t.Fatal("restart lost eligibility boundary", err)
	}
}

func TestProxyProtocolAutoDiagnosisUsesAllCandidatesAndDoesNotMutate(t *testing.T) {
	entry := proxyPoolEntry{ProxyPoolEntry: ProxyPoolEntry{ID: "auto", ProtocolAuto: true, Enabled: true}, RawURL: "http://proxy.invalid:8080"}
	s := capabilityFixtureServer(entry)
	w := httptest.NewRecorder()
	r := httptest.NewRequest("POST", "/diagnose", nil)
	r.SetPathValue("id", entry.ID)
	s.diagnoseProxyPoolEntry(w, r)
	var result struct {
		HTTPS, HTTP   egressProbeResult
		HealthUpdated bool `json:"health_updated"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if w.Code != 200 || result.HTTPS.OK || result.HTTP.OK || result.HTTP.ErrorCode != "not_requested" || result.HealthUpdated || len(result.HTTPS.ProtocolChecks) != 4 {
		t.Fatal(w.Body.String())
	}
	stored := s.m.proxyPoolEntries()[0]
	if !stored.LastCheckedAt.IsZero() || len(stored.ProtocolChecks) != 0 || stored.DetectedProtocol != "" || s.m.GlobalProxy() != "" {
		t.Fatal("diagnosis mutated health", stored)
	}
}

func TestProxyProtocolCancellationJoinsEveryCandidate(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	started := make(chan struct{}, 3)
	var joined sync.WaitGroup
	joined.Add(3)
	s := &Server{
		proxyProbe: func(ctx context.Context, _, _ string, _ []string, _ time.Duration) egressProbeResult {
			started <- struct{}{}
			defer joined.Done()
			<-ctx.Done()
			return egressProbeResult{Error: "探测已取消", ErrorCode: "cancelled", Probes: []egressAttempt{}}
		},
		proxySOCKS4Probe: func(ctx context.Context, _ string) (bool, string) {
			started <- struct{}{}
			defer joined.Done()
			<-ctx.Done()
			return false, "探测已取消"
		},
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		s.probePoolEntry(ctx, proxyPoolEntry{ProxyPoolEntry: ProxyPoolEntry{ProtocolAuto: true}, RawURL: "http://proxy.invalid:8080"})
	}()
	for range 3 {
		select {
		case <-started:
		case <-time.After(time.Second):
			t.Fatal("candidate did not start")
		}
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("candidate ignored cancellation")
	}
	joined.Wait()
}

func TestProxyProtocolAutoSelectsCompleteProtocolWithoutCandidateVeto(t *testing.T) {
	for _, selected := range []string{"http", "https", "socks5"} {
		t.Run(selected, func(t *testing.T) {
			entry := proxyPoolEntry{ProxyPoolEntry: ProxyPoolEntry{ID: "complete", ProtocolAuto: true, Enabled: true}, RawURL: "http://proxy.invalid:8080"}
			s := &Server{
				m:                &Manager{proxyPool: &proxyPoolState{entries: []proxyPoolEntry{entry}, disabled: true}},
				proxySOCKS4Probe: func(context.Context, string) (bool, string) { return false, "fixture" },
				proxyProbe: func(_ context.Context, raw, _ string, _ []string, _ time.Duration) egressProbeResult {
					if raw == selected+"://proxy.invalid:8080" {
						return egressProbeResult{OK: true, ExitIP: "192.0.2.10", Probes: []egressAttempt{}}
					}
					return egressProbeResult{Error: "fixture failure", ErrorCode: "proxy_transport", Probes: []egressAttempt{}}
				},
			}
			res, raw := s.probePoolEntry(t.Context(), entry)
			if !res.OK || raw != selected+"://proxy.invalid:8080" {
				t.Fatal("failed candidates vetoed a complete protocol", res, raw)
			}
			s.m.proxyPool.mu.Lock()
			_, err := s.m.applyProxyProbeBatchLocked([]proxyBatchProbe{{entry: entry, result: res, complete: true, resolvedURL: raw}}, 1)
			s.m.proxyPool.mu.Unlock()
			if err != nil || s.m.proxyPoolEntries()[0].DetectedProtocol != selected {
				t.Fatal("selected protocol was not saved", err)
			}
			if s.reconcileProxyPoolAfterProbe(false) || s.m.GlobalProxy() != "" {
				t.Fatal("automatic detection enabled a disabled global proxy")
			}
			s.m.proxyPool.disabled = false
			if !s.reconcileProxyPoolAfterProbe(false) || s.m.GlobalProxy() != raw {
				t.Fatal("complete protocol was not automatically activated")
			}
		})
	}
}

func TestProxyProtocolHTTPSHealthDoesNotDependOnPlainHTTPEcho(t *testing.T) {
	for _, available := range []string{"http://", "https://"} {
		s := &Server{proxyProbe: func(_ context.Context, raw, _ string, probes []string, _ time.Duration) egressProbeResult {
			if raw == "http://proxy.invalid:8080" && strings.HasPrefix(probes[0], available) {
				return egressProbeResult{Kind: "connectivity", OK: true, StatusCode: 204, Probes: []egressAttempt{}}
			}
			return egressProbeResult{Error: "fixture failure", ErrorCode: "proxy_transport", Probes: []egressAttempt{}}
		}}
		res, raw := s.probePoolEntry(t.Context(), proxyPoolEntry{ProxyPoolEntry: ProxyPoolEntry{ProtocolAuto: true}, RawURL: "http://proxy.invalid:8080"})
		if res.OK != (available == "https://") || (res.OK && raw == "") || res.ProtocolChecks[0].HTTP.Checked {
			t.Fatal("IP/plain HTTP affected TLS health", available, res, raw)
		}
	}
}

func TestProxyProtocolNoIPLookupOrNegativeControlRequests(t *testing.T) {
	s := &Server{proxyProbe: func(_ context.Context, raw, _ string, probes []string, _ time.Duration) egressProbeResult {
		if strings.Contains(raw, "203.0.113.1") {
			t.Error("sent a negative-control request")
		}
		for _, probe := range probes {
			if !strings.HasPrefix(probe, "https://") || strings.Contains(probe, "ipify") || strings.Contains(probe, "ifconfig") {
				t.Error("sent non-health request", probe)
			}
		}
		return egressProbeResult{Kind: "connectivity", OK: true, StatusCode: 204, Probes: []egressAttempt{}}
	}}
	res, raw := s.probePoolEntry(t.Context(), proxyPoolEntry{ProxyPoolEntry: ProxyPoolEntry{ProtocolAuto: true}, RawURL: "http://proxy.invalid:8080"})
	if !res.OK || raw == "" || res.ExitIP != "" {
		t.Fatal(res, raw)
	}
}
