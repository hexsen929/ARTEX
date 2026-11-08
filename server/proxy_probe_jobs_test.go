package server

import (
	"context"
	"encoding/json"
	"encoding/pem"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestProxyPoolAutoProtocolWithRealSOCKS5Handshake(t *testing.T) {
	echo := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(`{"ip":"192.0.2.44"}`)) }))
	defer echo.Close()
	ca := filepath.Join(t.TempDir(), "ca.pem")
	if err := os.WriteFile(ca, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: echo.Certificate().Raw}), 0600); err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	var wg sync.WaitGroup
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			wg.Add(1)
			go func() {
				defer wg.Done()
				defer conn.Close()
				_ = conn.SetDeadline(time.Now().Add(3 * time.Second))
				greeting := make([]byte, 2)
				if _, err := io.ReadFull(conn, greeting); err != nil || greeting[0] != 5 {
					return
				}
				if _, err := io.CopyN(io.Discard, conn, int64(greeting[1])); err != nil {
					return
				}
				_, _ = conn.Write([]byte{5, 0})
				header := make([]byte, 4)
				if _, err := io.ReadFull(conn, header); err != nil || header[0] != 5 || header[1] != 1 {
					return
				}
				n := 0
				switch header[3] {
				case 1:
					n = 4
				case 4:
					n = 16
				case 3:
					b := make([]byte, 1)
					if _, err := io.ReadFull(conn, b); err != nil {
						return
					}
					n = int(b[0])
				default:
					return
				}
				if _, err := io.CopyN(io.Discard, conn, int64(n+2)); err != nil {
					return
				}
				upstream, err := net.DialTimeout("tcp", echo.Listener.Addr().String(), time.Second)
				if err != nil {
					return
				}
				defer upstream.Close()
				_, _ = conn.Write([]byte{5, 0, 0, 1, 127, 0, 0, 1, 0, 1})
				copyDone := make(chan struct{})
				go func() { defer close(copyDone); _, _ = io.Copy(upstream, conn); _ = upstream.Close() }()
				_, _ = io.Copy(conn, upstream)
				_ = conn.Close()
				<-copyDone
			}()
		}
	}()
	defer func() { _ = listener.Close(); <-done; wg.Wait() }()
	s := &Server{proxyProbe: func(ctx context.Context, raw, _ string, _ []string, timeout time.Duration) egressProbeResult {
		if raw == "http://203.0.113.1:80" {
			return egressProbeResult{Error: "fixture control unavailable", Probes: []egressAttempt{}}
		}
		return probeEgress(ctx, raw, ca, []string{echo.URL}, timeout)
	}}
	entry := proxyPoolEntry{ProxyPoolEntry: ProxyPoolEntry{ProtocolAuto: true}, RawURL: "http://" + listener.Addr().String()}
	res, raw := s.probePoolEntry(t.Context(), entry)
	if !res.OK || res.ExitIP != "192.0.2.44" || !strings.HasPrefix(raw, "socks5://") {
		t.Fatal("real SOCKS5 proxy was not identified", res, raw)
	}
	// The same response with an untrusted TLS certificate must not qualify.
	s.proxyProbe = func(ctx context.Context, raw, _ string, _ []string, timeout time.Duration) egressProbeResult {
		if raw == "http://203.0.113.1:80" {
			return egressProbeResult{Error: "fixture control unavailable", Probes: []egressAttempt{}}
		}
		return probeEgress(ctx, raw, "", []string{echo.URL}, timeout)
	}
	res, raw = s.probePoolEntry(t.Context(), entry)
	if res.OK || raw != "" || res.ErrorCode != "certificate_untrusted" || !res.ProtocolRecognized || len(res.ProtocolChecks) != 4 {
		t.Fatal("detection bypassed TLS trust", res, raw)
	}
}

func TestProxyPoolProgressAndInteractiveResultWinsBatch(t *testing.T) {
	entries := proxyScaleEntries(30)
	m := &Manager{proxyPool: &proxyPoolState{entries: entries, disabled: true}}
	s := &Server{m: m, proxyPoolWake: make(chan struct{}, 1)}
	gate := make(chan struct{})
	started := make(chan struct{}, proxyPoolMaxProbeConcurrency)
	var mu sync.Mutex
	calls := map[string]int{}
	s.proxyProbe = func(ctx context.Context, raw, _ string, _ []string, _ time.Duration) egressProbeResult {
		mu.Lock()
		calls[raw]++
		n := calls[raw]
		mu.Unlock()
		if raw == entries[0].RawURL && n == 2 {
			return egressProbeResult{OK: true, ExitIP: "192.0.2.9"}
		}
		select {
		case started <- struct{}{}:
		default:
		}
		select {
		case <-gate:
		case <-ctx.Done():
		}
		return egressProbeResult{Error: "fixture failure", ErrorCode: "fixture"}
	}
	done := make(chan struct{})
	go func() { defer close(done); s.proxyPoolTick() }()
	defer func() {
		select {
		case <-gate:
		default:
			close(gate)
		}
		<-done
	}()
	for range proxyPoolMaxProbeConcurrency {
		select {
		case <-started:
		case <-time.After(5 * time.Second):
			t.Fatal("batch did not start")
		}
	}
	w := httptest.NewRecorder()
	s.getProxyProbeProgress(w, httptest.NewRequest("GET", "/api/settings/proxy-pool/progress?ids=scale-0,scale-1", nil))
	var progress proxyProbeRound
	if err := json.Unmarshal(w.Body.Bytes(), &progress); err != nil {
		t.Fatal(err)
	}
	if progress.Total != 30 || progress.Running != proxyPoolMaxProbeConcurrency || len(progress.Items) != 2 || strings.Contains(w.Body.String(), "fixture-secret") {
		t.Fatal(w.Body.String())
	}
	r := httptest.NewRequest("POST", "/test", nil)
	r.SetPathValue("id", "scale-0")
	w = httptest.NewRecorder()
	s.testProxyPoolEntry(w, r)
	if w.Code != 200 || m.proxyPoolEntries()[0].Health != "healthy" {
		t.Fatal("interactive probe blocked", w.Code, w.Body.String())
	}
	close(gate)
	<-done
	if entry := m.proxyPoolEntries()[0]; entry.Health != "healthy" || entry.EgressIP != "192.0.2.9" || entry.ConsecutiveFailures != 0 {
		t.Fatal("older batch replaced newer single result", entry)
	}
	w = httptest.NewRecorder()
	s.getProxyProbeProgress(w, httptest.NewRequest("GET", "/progress?ids=scale-0", nil))
	_ = json.Unmarshal(w.Body.Bytes(), &progress)
	if progress.State != "completed" || progress.Completed != 30 {
		t.Fatal(progress)
	}
}

func TestProxyPoolProgressCancelLeavesHealthUnchanged(t *testing.T) {
	m := &Manager{proxyPool: &proxyPoolState{entries: proxyScaleEntries(24), disabled: true}}
	s := &Server{m: m, proxyPoolWake: make(chan struct{}, 1)}
	started := make(chan struct{})
	var once sync.Once
	s.proxyProbe = func(ctx context.Context, _ string, _ string, _ []string, _ time.Duration) egressProbeResult {
		once.Do(func() { close(started) })
		<-ctx.Done()
		return egressProbeResult{Error: "cancelled"}
	}
	done := make(chan struct{})
	go func() { defer close(done); s.proxyPoolTick() }()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("no start")
	}
	w := httptest.NewRecorder()
	s.cancelProxyPoolTests(w, httptest.NewRequest("POST", "/cancel", nil))
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("cancel did not stop batch")
	}
	for _, e := range m.proxyPoolEntries() {
		if e.ConsecutiveFailures != 0 || !e.LastCheckedAt.IsZero() {
			t.Fatal("cancellation changed health")
		}
	}
	if m.proxyPool.probeRound.State != "cancelled" || m.proxyPool.probeRound.Running != 0 {
		t.Fatal(m.proxyPool.probeRound.State)
	}
	// A pending start that captured eligibility before cancellation must not
	// revive the round or overwrite the cancellation shown to the user.
	previous := m.proxyPool.probeRound
	ctx, round, queued := s.startProxyProbeRound(t.Context(), m.proxyPoolEntries(), m.proxyPool.generation, 0)
	defer s.finishProxyProbeRound(round, "superseded")
	if ctx.Err() == nil || len(queued) != 0 || m.proxyPool.probeRound != previous {
		t.Fatal("cancelled snapshot restarted the batch")
	}
}

func TestProxyPoolAutoProtocolPersistsOnlyVerifiedCandidate(t *testing.T) {
	parsed, issues := ParseProxyBatch("192.0.2.10:4145\nsocks://192.0.2.11:1080\n192.0.2.12:1080")
	if len(issues) != 0 || len(parsed) != 3 || !parsed[0].ProtocolAuto || parsed[1].ProtocolAuto || parsed[2].ProtocolAuto || !strings.HasPrefix(parsed[1].URL, "socks5://") {
		t.Fatal(parsed, issues)
	}
	entry := proxyPoolEntry{ProxyPoolEntry: parsed[0], RawURL: parsed[0].URL}
	m := &Manager{proxyPool: &proxyPoolState{entries: []proxyPoolEntry{entry}, disabled: true}}
	s := &Server{m: m}
	s.proxySOCKS4Probe = func(context.Context, string) (bool, string) { return false, "fixture" }
	var mu sync.Mutex
	seen := map[string]bool{}
	s.proxyProbe = func(ctx context.Context, raw, ca string, probes []string, timeout time.Duration) egressProbeResult {
		deadline, ok := ctx.Deadline()
		if !ok || time.Until(deadline) > proxyPoolProbeTimeout || (timeout != 5*time.Second && !(raw == "http://203.0.113.1:80" && timeout == 2*time.Second)) || ca != "" {
			t.Error("probe lost time/trust bounds")
		}
		mu.Lock()
		seen[strings.Split(raw, "://")[0]] = true
		mu.Unlock()
		if strings.HasPrefix(raw, "socks5://") {
			return egressProbeResult{OK: true, Via: "socks5", ExitIP: "192.0.2.5"}
		}
		return egressProbeResult{Error: "fixture", ErrorCode: "proxy_transport"}
	}
	if err := m.activateProxyID(entry.ID, "manual"); err == nil {
		t.Fatal("unidentified endpoint activated")
	}
	res, resolved := s.probePoolEntry(context.Background(), entry)
	if !res.OK || !strings.HasPrefix(resolved, "socks5://") || len(seen) != 3 {
		t.Fatal(res, resolved, seen)
	}
	m.proxyPool.mu.Lock()
	_, err := m.applyProxyProbeBatchLocked([]proxyBatchProbe{{entry: entry, result: res, resolvedURL: resolved, complete: true}}, 3)
	m.proxyPool.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	stored := m.proxyPoolEntries()[0]
	if stored.DetectedProtocol != "socks5" || stored.RawURL != resolved || m.GlobalProxy() != "" {
		t.Fatal("protocol resolution changed route or lost type", stored)
	}
	// Explicit protocol must not trigger guessing, even after failure.
	seen = map[string]bool{}
	entry.ProtocolAuto = false
	_, _ = s.probePoolEntry(context.Background(), entry)
	if len(seen) != 1 || !seen["http"] {
		t.Fatal("explicit protocol was changed", seen)
	}
}

func TestProxyPoolAutoImportWaitsForResolution(t *testing.T) {
	m := &Manager{proxyPool: &proxyPoolState{}}
	s := &Server{m: m, proxyPoolWake: make(chan struct{}, 1)}
	importBody := `{"text":"192.0.2.1:4145\nhttp://192.0.2.2:8080"}`
	w := httptest.NewRecorder()
	s.importProxyPool(w, httptest.NewRequest("POST", "/import?summary=1", strings.NewReader(importBody)))
	if w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	entries := m.proxyPoolEntries()
	if len(entries) != 2 || !entries[0].ProtocolAuto || m.GlobalProxy() != entries[1].RawURL {
		t.Fatal("pending protocol became active", entries, m.GlobalProxy())
	}
	if err := m.activateProxyID(entries[0].ID, "manual"); err == nil {
		t.Fatal("unresolved proxy selected")
	}
	// Explicit selection remains available for users who know the protocol.
	parsed, issues := parseProxyBatchWithProtocol("192.0.2.3:1080", "socks5")
	if len(issues) != 0 || len(parsed) != 1 || parsed[0].ProtocolAuto || parsed[0].URL != "socks5://192.0.2.3:1080" {
		t.Fatal(parsed, issues)
	}
	// Saved detection must survive restart and bare re-import must remain a no-op.
	entry := entries[0]
	res := egressProbeResult{OK: true, Via: "socks5", ExitIP: "192.0.2.6"}
	m.proxyPool.mu.Lock()
	_, err := m.applyProxyProbeBatchLocked([]proxyBatchProbe{{entry: entry, result: res, resolvedURL: "socks5://192.0.2.1:4145", complete: true}}, 3)
	m.proxyPool.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(proxyPoolPersist{Entries: m.proxyPoolEntries(), Active: entries[1].ID})
	pool, _, _, err := decodeProxyPoolStartup(map[string]string{settingProxyPool: string(raw)})
	if err != nil || pool.Entries[0].DetectedProtocol != "socks5" {
		t.Fatal("detected protocol lost on restart", err)
	}
	w = httptest.NewRecorder()
	s.importProxyPool(w, httptest.NewRequest("POST", "/import?summary=1", strings.NewReader(`{"text":"192.0.2.1:4145"}`)))
	if w.Code != 200 || len(m.proxyPoolEntries()) != 2 {
		t.Fatal("bare reimport duplicated resolved proxy", w.Body.String())
	}
}
