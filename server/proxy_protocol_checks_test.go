package server

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
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

func TestProxyProtocolSOCKS4ResponseRecognizedButNotRoutable(t *testing.T) {
	for _, replyCode := range []byte{90, 91, 92, 93, 0} {
		listener, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		done := make(chan struct{})
		go func() {
			defer close(done)
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			defer conn.Close()
			_ = conn.SetDeadline(time.Now().Add(time.Second))
			request := make([]byte, 23)
			if _, err := io.ReadFull(conn, request[:9]); err != nil {
				return
			}
			if request[0] != 4 || request[1] != 1 || request[8] != 0 {
				return
			}
			_, _ = conn.Write([]byte{0, replyCode, 0, 0, 0, 0, 0, 0})
		}()
		entry := proxyPoolEntry{ProxyPoolEntry: ProxyPoolEntry{ID: "socks4", ProtocolAuto: true, Enabled: true}, RawURL: "http://" + listener.Addr().String()}
		s := &Server{proxyProbe: func(context.Context, string, string, []string, time.Duration) egressProbeResult {
			return egressProbeResult{Error: "fixture transport failure", ErrorCode: "proxy_transport", Probes: []egressAttempt{}}
		}}
		result, raw := s.probePoolEntry(t.Context(), entry)
		_ = listener.Close()
		<-done
		if result.OK || raw != "" || len(result.ProtocolChecks) != 4 || result.ProtocolChecks[3].Supported {
			t.Fatal(result, raw)
		}
		if result.ProtocolChecks[3].Recognized != (replyCode != 0) {
			t.Fatal("invalid SOCKS4 classification", replyCode, result)
		}
		if replyCode != 0 && result.ErrorCode != "unsupported_proxy_protocol" {
			t.Fatal("SOCKS4 support was implied", result)
		}
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
	if w.Code != 200 || result.HTTPS.OK || !result.HTTP.OK || result.HTTP.Via != "socks5" || result.HealthUpdated || len(result.HTTPS.ProtocolChecks) != 4 {
		t.Fatal(w.Body.String())
	}
	stored := s.m.proxyPoolEntries()[0]
	if !stored.LastCheckedAt.IsZero() || len(stored.ProtocolChecks) != 0 || stored.DetectedProtocol != "" || s.m.GlobalProxy() != "" {
		t.Fatal("diagnosis mutated health", stored)
	}
}

func TestProxyProtocolCancellationJoinsEveryCandidate(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	started := make(chan struct{}, 8)
	var joined sync.WaitGroup
	joined.Add(8)
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
	for range 8 {
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

func TestProxyProtocolManualHTTPUseSurvivesChecksAndRestart(t *testing.T) {
	entry := proxyPoolEntry{ProxyPoolEntry: ProxyPoolEntry{ID: "limited", ProtocolAuto: true, Enabled: true}, RawURL: "http://proxy.invalid:8080"}
	s := capabilityFixtureServer(entry)
	result, raw := s.probePoolEntry(t.Context(), entry)
	s.m.proxyPool.mu.Lock()
	_, err := s.m.applyProxyProbeBatchLocked([]proxyBatchProbe{{entry: entry, result: result, complete: true, resolvedURL: raw}}, 1)
	s.m.proxyPool.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	if proxyCleanupEligible(s.m.proxyPoolEntries()[0].ProxyPoolEntry, 1) {
		t.Fatal("HTTP-capable node selected for failed-node cleanup")
	}
	if err := s.m.activateProxyProtocol(entry.ID, proxyProtocolSelection{Protocol: "socks5"}); err == nil {
		t.Fatal("unverified HTTPS route activated")
	}
	if err := s.m.activateProxyProtocol(entry.ID, proxyProtocolSelection{Protocol: "socks4", HTTPOnly: true}); err == nil {
		t.Fatal("unsupported protocol activated")
	}
	if err := s.m.activateProxyProtocol(entry.ID, proxyProtocolSelection{Protocol: "socks5", HTTPOnly: true}); err != nil {
		t.Fatal(err)
	}
	stored := s.m.proxyPoolEntries()[0]
	if !stored.HTTPOnly || stored.ProtocolAuto || stored.Health != "http-only" || s.m.GlobalProxy() != "socks5://proxy.invalid:8080" {
		t.Fatal(stored)
	}
	res, raw := s.probePoolEntry(t.Context(), stored)
	if !res.OK || raw != "" || res.Via != "socks5" || len(res.ProtocolChecks) != 1 || res.ProtocolChecks[0].HTTPS.OK {
		t.Fatal("HTTP health lost or HTTPS incorrectly passed", res)
	}
	s.m.proxyPool.mu.Lock()
	confirmed, err := s.m.applyProxyProbeBatchLocked([]proxyBatchProbe{{entry: stored, result: res, complete: true}}, 1)
	automatic := s.m.selectHealthyLocked("")
	s.m.proxyPool.mu.Unlock()
	if err != nil || confirmed || automatic != "" || s.m.proxyPoolEntries()[0].Health != "http-only" {
		t.Fatal("limited route promoted to general HTTPS health", confirmed, automatic, err)
	}
	if s.reconcileProxyPoolAfterProbe(false) || s.m.GlobalProxy() != stored.RawURL {
		t.Fatal("HTTP health check discarded manually selected route")
	}
	persisted, _ := json.Marshal(proxyPoolPersist{Entries: s.m.proxyPoolEntries(), Active: entry.ID})
	pool, global, _, err := decodeProxyPoolStartup(map[string]string{settingProxyPool: string(persisted)})
	if err != nil || global != stored.RawURL || !pool.Entries[0].HTTPOnly {
		t.Fatal("manual HTTP route lost on restart", err)
	}
	// Losing HTTP connectivity must still cool the route; this is not an
	// override that keeps a dead proxy healthy indefinitely.
	s.m.proxyPool.mu.Lock()
	_, err = s.m.applyProxyProbeBatchLocked([]proxyBatchProbe{{entry: s.m.proxyPool.entries[0], result: egressProbeResult{Error: "timeout", ErrorCode: "timeout"}, complete: true}}, 1)
	s.m.proxyPool.mu.Unlock()
	if err != nil || s.m.proxyPoolEntries()[0].Health != "unhealthy-cooling" {
		t.Fatal("dead HTTP route stayed healthy", err)
	}
}

func TestProxyProtocolManualSelectionRejectsStaleAndMalformedRequests(t *testing.T) {
	entry := proxyPoolEntry{ProxyPoolEntry: ProxyPoolEntry{ID: "stale", ProtocolAuto: true, Enabled: true, LastCheckedAt: time.Now().Add(-6 * time.Minute), ProtocolChecks: []proxyProtocolCheck{{Protocol: "http", Supported: true, HTTP: proxyCapabilityCheck{Checked: true, OK: true, IP: "192.0.2.9"}}}}, RawURL: "http://proxy.invalid:80"}
	m := &Manager{proxyPool: &proxyPoolState{entries: []proxyPoolEntry{entry}, disabled: true}}
	if err := m.activateProxyProtocol(entry.ID, proxyProtocolSelection{Protocol: "http", HTTPOnly: true}); err == nil || m.GlobalProxy() != "" || !m.proxyPool.disabled {
		t.Fatal("stale result changed route")
	}
	for _, body := range []string{`{"http_only":true}`, `{"protocol":"http","extra":true}`, `{} {}`, `{`} {
		r := httptest.NewRequest("POST", "/activate", strings.NewReader(body))
		if _, err := decodeProxyProtocolSelection(httptest.NewRecorder(), r); err == nil {
			t.Fatal("invalid selection accepted", body)
		}
	}
}

func TestProxyProtocolAutoSelectsHTTPAndPrefersHTTPSWhenAvailable(t *testing.T) {
	entry := proxyPoolEntry{ProxyPoolEntry: ProxyPoolEntry{ID: "auto-http", ProtocolAuto: true, Enabled: true}, RawURL: "http://proxy.invalid:8080"}
	s := capabilityFixtureServer(entry)
	result, raw := s.probePoolEntry(t.Context(), entry)
	if !result.OK || !result.HTTPOnly || raw != "socks5://proxy.invalid:8080" || result.ProtocolChecks[2].HTTPS.OK {
		t.Fatal("HTTP-capable protocol was not selected independently", result, raw)
	}
	s.m.proxyPool.mu.Lock()
	_, err := s.m.applyProxyProbeBatchLocked([]proxyBatchProbe{{entry: entry, result: result, complete: true, resolvedURL: raw}}, 1)
	automatic := s.m.selectHealthyLocked("")
	stored := s.m.proxyPool.entries[0]
	s.m.proxyPool.mu.Unlock()
	if err != nil || automatic != entry.ID || stored.DetectedProtocol != "socks5" || !stored.HTTPOnly || stored.Health != "http-only" {
		t.Fatal("HTTP route was left locked", automatic, stored, err)
	}
	full := proxyPoolEntry{ProxyPoolEntry: ProxyPoolEntry{ID: "full", Enabled: true, Health: "healthy"}, RawURL: "http://full.invalid:80"}
	s.m.proxyPool.mu.Lock()
	s.m.proxyPool.entries = append(s.m.proxyPool.entries, full)
	preferred := s.m.selectHealthyLocked(stored.ID)
	noDowngrade := s.m.selectHealthyLocked(full.ID)
	s.m.proxyPool.mu.Unlock()
	if preferred != full.ID || noDowngrade != full.ID {
		t.Fatal("rotation preferred HTTP over HTTPS", preferred, noDowngrade)
	}
	// A working supported protocol wins even while others fail.
	probe := s.proxyProbe
	s.proxyProbe = func(ctx context.Context, raw, ca string, probes []string, timeout time.Duration) egressProbeResult {
		if strings.HasPrefix(raw, "https://") && strings.HasPrefix(probes[0], "https://") {
			return egressProbeResult{OK: true, ExitIP: "192.0.2.10", Probes: []egressAttempt{}}
		}
		return probe(ctx, raw, ca, probes, timeout)
	}
	result, raw = s.probePoolEntry(t.Context(), entry)
	if !result.OK || result.HTTPOnly || raw != "https://proxy.invalid:8080" {
		t.Fatal("one candidate's failure vetoed a good HTTPS route", result, raw)
	}
}

func TestProxyProtocolNegativeControlBlocksTransparentHTTPFalsePositive(t *testing.T) {
	s := &Server{
		proxySOCKS4Probe: func(context.Context, string) (bool, string) { return false, "fixture" },
		proxyProbe: func(_ context.Context, raw, _ string, probes []string, _ time.Duration) egressProbeResult {
			if strings.HasPrefix(raw, "http://") && strings.HasPrefix(probes[0], "http://") {
				return egressProbeResult{OK: true, ExitIP: "192.0.2.11", Probes: []egressAttempt{}}
			}
			return egressProbeResult{Error: "CONNECT 被拒绝", ErrorCode: "proxy_connect_rejected", Probes: []egressAttempt{}}
		},
	}
	result, raw := s.probePoolEntry(t.Context(), proxyPoolEntry{ProxyPoolEntry: ProxyPoolEntry{ProtocolAuto: true}, RawURL: "http://proxy.invalid:80"})
	if result.OK || raw != "" || !result.ProtocolChecks[0].HTTP.OK || !result.ProtocolChecks[0].AutoSelectionBlocked {
		t.Fatal("transparent HTTP false positive became a proxy route", result, raw)
	}
}
