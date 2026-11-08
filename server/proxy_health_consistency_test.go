package server

import (
	"context"
	"crypto/x509"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Autumn-27/artex/traffic"
)

func proxyHealthServer() *Server {
	entry := proxyPoolEntry{ProxyPoolEntry: ProxyPoolEntry{ID: "active", Enabled: true, Health: "healthy"}, RawURL: "socks5://proxy.fixture.invalid:1080"}
	return &Server{m: &Manager{globalProxy: entry.RawURL, tasks: map[string]*Task{}, proxyPool: &proxyPoolState{active: entry.ID, entries: []proxyPoolEntry{entry}}}}
}

func TestProxyHealthSuccessConsumesFailureWithoutSecondProbe(t *testing.T) {
	s := proxyHealthServer()
	var calls atomic.Int32
	s.proxyProbe = func(context.Context, string, string, []string, time.Duration) egressProbeResult {
		if calls.Add(1) > 1 {
			return egressProbeResult{Error: "a second probe would fail", ErrorCode: "timeout"}
		}
		return egressProbeResult{OK: true, ExitIP: "192.0.2.5"}
	}
	s.handleProxyTrafficFailure(errors.New("socks target connection refused"))
	if calls.Load() != 1 || s.m.proxyPoolEntries()[0].ConsecutiveFailures != 0 {
		t.Fatal("healthy route was rechecked and marked down", calls.Load())
	}
}

func TestProxyHealthConcurrentCallbacksShareOneConfirmation(t *testing.T) {
	s := proxyHealthServer()
	entered, release, done := make(chan struct{}), make(chan struct{}), make(chan struct{})
	var calls atomic.Int32
	s.proxyProbe = func(context.Context, string, string, []string, time.Duration) egressProbeResult {
		if calls.Add(1) == 1 {
			close(entered)
		}
		<-release
		return egressProbeResult{Error: "connection refused", ErrorCode: "connection_refused"}
	}
	go func() { defer close(done); s.handleProxyTrafficFailure(errors.New("socks refused")) }()
	t.Cleanup(func() {
		select {
		case <-release:
		default:
			close(release)
		}
		<-done
	})
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("probe did not start")
	}
	var wg sync.WaitGroup
	for range 30 {
		wg.Add(1)
		go func() { defer wg.Done(); s.handleProxyTrafficFailure(errors.New("socks refused")) }()
	}
	wg.Wait()
	close(release)
	<-done
	if calls.Load() != 1 || s.m.proxyPoolEntries()[0].ConsecutiveFailures != 1 {
		t.Fatal("callback storm multiplied failed confirmations", calls.Load())
	}
}

func TestProxyHealthDuplicateURLUpdatesOnlyActiveMember(t *testing.T) {
	s := proxyHealthServer()
	duplicate := s.m.proxyPool.entries[0]
	duplicate.ID = "inactive-duplicate"
	s.m.proxyPool.entries = append([]proxyPoolEntry{duplicate}, s.m.proxyPool.entries...)
	s.proxyProbe = func(context.Context, string, string, []string, time.Duration) egressProbeResult {
		return egressProbeResult{Error: "connection refused", ErrorCode: "connection_refused"}
	}
	s.handleProxyTrafficFailure(errors.New("socks refused"))
	entries := s.m.proxyPoolEntries()
	if entries[0].ConsecutiveFailures != 0 || entries[1].ConsecutiveFailures != 1 {
		t.Fatal("failure written by URL instead of active ID", entries)
	}
	s.m.proxyPool.mu.Lock()
	next := s.m.selectHealthyLocked("active")
	s.m.proxyPool.mu.Unlock()
	if next != "active" {
		t.Fatal("duplicate URL treated as a different egress", next)
	}
}

func TestProxyHealthLateFailureCannotReplaceNewerSuccess(t *testing.T) {
	s := proxyHealthServer()
	s.proxyProbe = func(context.Context, string, string, []string, time.Duration) egressProbeResult {
		s.m.markProxyProbe("active", egressProbeResult{OK: true, ExitIP: "192.0.2.8"})
		return egressProbeResult{Error: "old failure", ErrorCode: "timeout"}
	}
	s.handleProxyTrafficFailure(errors.New("socks refused"))
	stored := s.m.proxyPoolEntries()[0]
	if stored.EgressIP != "192.0.2.8" || stored.ConsecutiveFailures != 0 || stored.LastError != "" {
		t.Fatal("old confirmation overwrote newer success", stored)
	}
}

func TestProxyHealthEchoRejectionDoesNotProveOutage(t *testing.T) {
	s := proxyHealthServer()
	entry := s.m.proxyPoolEntries()[0]
	result := egressProbeResult{Error: "echo rejected", ErrorCode: "multiple_probe_failures", Probes: []egressAttempt{{ErrorCode: "timeout"}, {ErrorCode: "echo_http_error"}}}
	for range 5 {
		s.m.proxyPool.mu.Lock()
		_, err := s.m.applyProxyProbeBatchLocked([]proxyBatchProbe{{entry: entry, result: result, complete: true}}, 3)
		s.m.proxyPool.mu.Unlock()
		if err != nil {
			t.Fatal(err)
		}
	}
	stored := s.m.proxyPoolEntries()[0]
	if stored.Health != "healthy" || stored.ConsecutiveFailures != 0 || stored.LastError == "" {
		t.Fatal("echo service response became global route outage", stored)
	}
}

func TestProxyHealthSingleFailureHonorsPauseThreshold(t *testing.T) {
	m := proxyTestManager(t)
	task, err := m.CreateTask("proxy threshold", "fixture", nil, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _, _ = m.DeleteTask(task.ID, DeleteTaskOptions{}) }()
	m.globalProxy = "socks5://proxy.fixture.invalid:1080"
	m.proxyPool = &proxyPoolState{active: "active", entries: []proxyPoolEntry{{ProxyPoolEntry: ProxyPoolEntry{ID: "active", Enabled: true, Health: "healthy"}, RawURL: m.globalProxy}}}
	s := &Server{m: m}
	s.proxyProbe = func(context.Context, string, string, []string, time.Duration) egressProbeResult {
		return egressProbeResult{Error: "timeout", ErrorCode: "timeout"}
	}
	s.handleProxyTrafficFailure(errors.New("socks failure"))
	if task.lifecycleSnapshot().Paused {
		t.Fatal("single failure bypassed configured threshold")
	}
}

func TestProxyHealthSettingsUsesStrictUpstreamAndActualCapture(t *testing.T) {
	tr, err := traffic.Open(t.TempDir(), "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer tr.Close()
	m := &Manager{globalProxy: "socks5://proxy.fixture.invalid:1080", traffic: tr, trafficOn: true}
	s := &Server{m: m}
	for _, failCapture := range []bool{false, true} {
		calls := 0
		s.proxyProbe = func(_ context.Context, raw, ca string, _ []string, _ time.Duration) egressProbeResult {
			calls++
			if raw == m.GlobalProxy() {
				if ca != "" {
					t.Fatal("upstream check used capture trust")
				}
				if !failCapture {
					code, label := classifyEgressProbeError(x509.UnknownAuthorityError{})
					return egressProbeResult{Error: label, ErrorCode: code, Probes: []egressAttempt{}}
				}
			}
			if raw == m.ProxyAddr() {
				if ca != m.ProxyCACert() {
					t.Fatal("capture CA missing")
				}
				if failCapture {
					return egressProbeResult{Error: "capture auth failed", Probes: []egressAttempt{}}
				}
			}
			return egressProbeResult{OK: true, ExitIP: "192.0.2.4", Probes: []egressAttempt{}}
		}
		w := httptest.NewRecorder()
		s.testProxyHealth(w, httptest.NewRequest("POST", "/proxy/test", strings.NewReader(`{"global_proxy":"socks5://proxy.fixture.invalid:1080"}`)))
		var report egressReport
		if err := json.Unmarshal(w.Body.Bytes(), &report); err != nil {
			t.Fatal(err)
		}
		if report.OK || calls != 3 || len(report.Checks) != 3 || report.Checks[2].Key != "capture" {
			t.Fatal("inconsistent paths reported healthy", w.Body.String(), calls)
		}
	}
}
