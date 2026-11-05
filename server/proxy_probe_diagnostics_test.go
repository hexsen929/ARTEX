package server

import (
	"context"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestProxyProbeErrorClassificationsAreSpecificAndDoNotEchoPeerData(t *testing.T) {
	for _, tc := range []struct {
		err  error
		code string
	}{
		{x509.CertificateInvalidError{Cert: &x509.Certificate{}, Reason: x509.Expired}, "certificate_time_invalid"},
		{x509.CertificateInvalidError{Cert: &x509.Certificate{}, Reason: x509.NotAuthorizedToSign}, "certificate_invalid"},
		{x509.UnknownAuthorityError{}, "certificate_untrusted"},
		{x509.HostnameError{Certificate: &x509.Certificate{}, Host: "PRIVATE_HOST"}, "certificate_hostname"},
		{context.Canceled, "cancelled"},
		{context.DeadlineExceeded, "timeout"},
		{syscall.ECONNREFUSED, "connection_refused"},
		{&net.DNSError{Err: "PRIVATE_DNS", Name: "PRIVATE_HOST"}, "dns_failed"},
		{errors.New("proxyconnect tcp: Proxy Authentication Required"), "proxy_authentication"},
		{errors.New("username/password authentication failed PRIVATE_PASSWORD"), "proxy_authentication"},
		{errors.New("HTTP 403"), "echo_http_error"},
		{errors.New("无法从响应中解析出 IP PRIVATE_BODY"), "invalid_ip_response"},
		{errors.New("tls: PRIVATE_CHAIN"), "tls_handshake"},
		{errors.New("PRIVATE_PASSWORD 4077"), "proxy_transport"},
	} {
		t.Run(tc.code, func(t *testing.T) {
			code, label := classifyEgressProbeError(&url.Error{Op: "Get", URL: "https://private.invalid/?token=PRIVATE_TOKEN", Err: tc.err})
			if code != tc.code || label == "" || strings.Contains(label, "PRIVATE_") || strings.Contains(label, "private.invalid") {
				t.Fatalf("code=%q label=%q", code, label)
			}
		})
	}
}

func TestProxyProbeFailureSummaryPreservesSpecificReason(t *testing.T) {
	code, label := summarizeEgressProbeFailures([]egressAttempt{
		{ErrorCode: "certificate_time_invalid", Error: "TLS 证书已过期或尚未生效"},
		{ErrorCode: "certificate_time_invalid", Error: "TLS 证书已过期或尚未生效"},
	})
	if code != "certificate_time_invalid" || strings.Count(label, "TLS 证书") != 1 {
		t.Fatalf("summary lost or repeated reason: %s %s", code, label)
	}
	code, label = summarizeEgressProbeFailures([]egressAttempt{{ErrorCode: "timeout", Error: "超时"}, {ErrorCode: "certificate_untrusted", Error: "证书链不受信任"}})
	if code != "multiple_probe_failures" || !strings.Contains(label, "超时") || !strings.Contains(label, "证书链") {
		t.Fatal("mixed reasons were collapsed")
	}
}

func TestProxyProbeUntrustedHTTPSFailsWithSpecificPersistableReason(t *testing.T) {
	var requests int
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		_, _ = io.WriteString(w, `{"ip":"192.0.2.5"}`)
	}))
	defer srv.Close()
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodConnect {
			http.Error(w, "CONNECT required", http.StatusMethodNotAllowed)
			return
		}
		upstream, err := net.Dial("tcp", strings.TrimPrefix(srv.URL, "https://"))
		if err != nil {
			http.Error(w, "fixture dial failed", http.StatusBadGateway)
			return
		}
		client, rw, err := http.NewResponseController(w).Hijack()
		if err != nil {
			_ = upstream.Close()
			return
		}
		defer client.Close()
		defer upstream.Close()
		_, _ = fmt.Fprint(rw, "HTTP/1.1 200 Connection Established\r\n\r\n")
		_ = rw.Flush()
		go func() { _, _ = io.Copy(upstream, client); _ = upstream.Close() }()
		_, _ = io.Copy(client, upstream)
	}))
	defer proxy.Close()
	result := probeEgress(context.Background(), proxy.URL, "", []string{srv.URL}, 2*time.Second)
	if result.OK || result.ErrorCode != "certificate_untrusted" || !strings.Contains(result.Error, "证书链不受信任") || requests != 0 {
		t.Fatalf("untrusted TLS was not rejected with its actual reason: %+v", result)
	}
	entry := proxyScaleEntries(1)[0]
	m := &Manager{proxyPool: &proxyPoolState{entries: []proxyPoolEntry{entry}}}
	m.proxyPool.mu.Lock()
	_, err := m.applyProxyProbeBatchLocked([]proxyBatchProbe{{entry: entry, complete: true, result: result}}, 3)
	m.proxyPool.mu.Unlock()
	if err != nil || m.proxyPool.entries[0].LastErrorCode != "certificate_untrusted" || m.proxyPool.entries[0].ConsecutiveFailures != 1 {
		t.Fatal("pool discarded the specific TLS reason")
	}
}

func TestProxyPoolDiagnosticsSeparateHTTPWithoutPublishingHealthOrRoute(t *testing.T) {
	entry := proxyScaleEntries(1)[0]
	entry.LastError, entry.LastErrorCode, entry.ConsecutiveFailures, entry.Health = "old failure", "certificate_untrusted", 1, "unknown"
	m := &Manager{globalProxy: entry.RawURL, proxyPool: &proxyPoolState{entries: []proxyPoolEntry{entry}, active: entry.ID, generation: 8}}
	s := &Server{m: m}
	calls := 0
	s.proxyProbe = func(ctx context.Context, raw, ca string, probes []string, timeout time.Duration) egressProbeResult {
		calls++
		if raw != entry.RawURL || ca != "" || timeout != proxyPoolProbeTimeout {
			t.Fatal("diagnostic lost proxy identity or changed trust")
		}
		if calls == 1 {
			if !reflect.DeepEqual(probes, defaultEgressProbes) {
				t.Fatal("HTTPS health addresses were replaced")
			}
			return egressProbeResult{Error: "TLS 证书链不受信任", ErrorCode: "certificate_untrusted", Probes: []egressAttempt{}}
		}
		if len(probes) != 1 || probes[0] != "http://api.ipify.org?format=json" {
			t.Fatal("HTTP diagnosis did not use its separate echo")
		}
		return egressProbeResult{OK: true, ExitIP: "192.0.2.5", Probes: []egressAttempt{}}
	}
	r := httptest.NewRequest(http.MethodPost, "/api/settings/proxy-pool/scale-0/diagnose", nil)
	r.SetPathValue("id", entry.ID)
	w := httptest.NewRecorder()
	s.diagnoseProxyPoolEntry(w, r)
	var result struct {
		ID            string            `json:"id"`
		HTTPS         egressProbeResult `json:"https"`
		HTTP          egressProbeResult `json:"http"`
		HealthUpdated bool              `json:"health_updated"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if w.Code != http.StatusOK || result.ID != entry.ID || result.HTTPS.OK || !result.HTTP.OK || result.HealthUpdated || calls != 2 {
		t.Fatalf("bad diagnostic response: %d %s", w.Code, w.Body.String())
	}
	if !reflect.DeepEqual(m.proxyPoolEntries(), []proxyPoolEntry{entry}) || m.proxyPool.generation != 8 || m.GlobalProxy() != entry.RawURL || len(m.proxyPool.logs) != 0 {
		t.Fatal("HTTP-only success changed health or runtime route")
	}
	if strings.Contains(w.Body.String(), "fixture-secret") || strings.Contains(w.Body.String(), "fixture-user") {
		t.Fatal("diagnosis returned stored credentials")
	}
}

func TestProxyPoolDiagnosticRejectsCanceledOrChangedSnapshots(t *testing.T) {
	for _, mode := range []string{"edit", "cancel", "shutdown", "batch-busy"} {
		t.Run(mode, func(t *testing.T) {
			entries := proxyScaleEntries(1)
			m := &Manager{proxyPool: &proxyPoolState{entries: entries}}
			s := &Server{m: m}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			calls := 0
			s.proxyProbe = func(context.Context, string, string, []string, time.Duration) egressProbeResult {
				calls++
				if mode == "edit" {
					m.proxyPool.mu.Lock()
					m.proxyPool.generation++
					m.proxyPool.mu.Unlock()
				} else if mode == "shutdown" {
					s.proxyReconnect.mu.Lock()
					s.proxyReconnect.closed = true
					s.proxyReconnect.mu.Unlock()
				} else {
					cancel()
				}
				return egressProbeResult{OK: true, ExitIP: "192.0.2.5"}
			}
			if mode == "batch-busy" {
				s.proxyPoolTickMu.Lock()
				defer s.proxyPoolTickMu.Unlock()
			}
			r := httptest.NewRequest(http.MethodPost, "/api/settings/proxy-pool/scale-0/diagnose", nil).WithContext(ctx)
			r.SetPathValue("id", entries[0].ID)
			w := httptest.NewRecorder()
			s.diagnoseProxyPoolEntry(w, r)
			if w.Code != http.StatusConflict || calls > 1 || !reflect.DeepEqual(entries, m.proxyPoolEntries()) {
				t.Fatalf("stale diagnostic accepted: status=%d calls=%d", w.Code, calls)
			}
		})
	}
}
