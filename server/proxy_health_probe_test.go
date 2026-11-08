package server

import (
	"context"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func preserveHealthProbeSetting(t *testing.T, m *Manager) {
	t.Helper()
	old, existed, err := m.pg.GetSetting(settingProxyHealthURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if existed {
			if err := m.pg.SetSetting(settingProxyHealthURL, old); err != nil {
				t.Error(err)
			}
		} else {
			_, _ = m.pg.Exec("DELETE FROM settings WHERE key=$1", settingProxyHealthURL)
		}
	})
}

func TestProxyConnectivityUsesHTTPSWithoutIPBody(t *testing.T) {
	methods := []string{}
	var methodsMu sync.Mutex
	var status atomic.Int32
	status.Store(http.StatusNoContent)
	target := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/slow" {
			<-r.Context().Done()
			return
		}
		methodsMu.Lock()
		methods = append(methods, r.Method)
		methodsMu.Unlock()
		code := int(status.Load())
		w.WriteHeader(code)
		if code == 200 {
			_, _ = io.WriteString(w, "ordinary webpage without IP")
		}
	}))
	defer target.Close()
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodConnect {
			t.Error("probe did not use CONNECT")
			w.WriteHeader(405)
			return
		}
		remote, err := net.DialTimeout("tcp", target.Listener.Addr().String(), time.Second)
		if err != nil {
			w.WriteHeader(502)
			return
		}
		defer remote.Close()
		client, buffered, err := http.NewResponseController(w).Hijack()
		if err != nil {
			return
		}
		defer client.Close()
		_, _ = fmt.Fprint(buffered, "HTTP/1.1 200 Connection Established\r\n\r\n")
		_ = buffered.Flush()
		done := make(chan struct{})
		go func() { defer close(done); _, _ = io.Copy(remote, buffered); _ = remote.Close() }()
		_, _ = io.Copy(client, remote)
		_ = client.Close()
		<-done
	}))
	defer proxy.Close()
	ca := filepath.Join(t.TempDir(), "fixture.pem")
	if err := os.WriteFile(ca, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: target.Certificate().Raw}), 0600); err != nil {
		t.Fatal(err)
	}
	for _, code := range []int{204, 200} {
		status.Store(int32(code))
		res := probeHTTP(t.Context(), proxy.URL, ca, []string{target.URL}, time.Second, true)
		if !res.OK || res.ExitIP != "" || res.StatusCode != code || res.Kind != "connectivity" || !res.ProtocolRecognized {
			t.Fatal("non-IP HTTPS rejected", res)
		}
	}
	methodsMu.Lock()
	if !reflect.DeepEqual(methods, []string{"HEAD", "HEAD"}) {
		t.Fatal("health fetched IP/body instead of lightweight request", methods)
	}
	methodsMu.Unlock()
	fallback := probeHTTP(t.Context(), proxy.URL, ca, []string{target.URL + "/slow", target.URL}, 250*time.Millisecond, true)
	if !fallback.OK || len(fallback.Probes) != 2 || fallback.Probes[0].ErrorCode != "timeout" {
		t.Fatal("first timeout consumed the fallback budget", fallback)
	}
	res := probeHTTP(t.Context(), proxy.URL, "", []string{target.URL}, time.Second, true)
	if res.OK || res.ErrorCode != "certificate_untrusted" {
		t.Fatal("untrusted TLS became healthy", res)
	}
}

func TestProxyConnectivityHEADFallbackAndRedirectBoundary(t *testing.T) {
	for _, code := range []int{405, 501, 403, 429, 302} {
		t.Run(fmt.Sprint(code), func(t *testing.T) {
			methods := []string{}
			target := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				methods = append(methods, r.Method)
				if r.Method == "HEAD" {
					w.Header().Set("Location", "https://never.fixture.invalid/")
					w.WriteHeader(code)
					return
				}
				w.WriteHeader(200)
				_, _ = io.WriteString(w, "health page")
			}))
			defer target.Close()
			client := target.Client()
			client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
			status, err := probeConnectivityOne(t.Context(), client, target.URL)
			fallback := code == 405 || code == 501
			if (err == nil) != fallback {
				t.Fatal(status, err)
			}
			want := []string{"HEAD"}
			if fallback {
				want = append(want, "GET")
			}
			if !reflect.DeepEqual(methods, want) {
				t.Fatal("unexpected fallback request", methods)
			}
		})
	}
}

func TestProxyConnectivityConfigAndIPQueryAreIndependent(t *testing.T) {
	s, request, _ := callbackSettingsFixture(t)
	preserveEgressProbeSetting(t, s.m)
	preserveHealthProbeSetting(t, s.m)
	_, _ = s.m.pg.Exec("DELETE FROM settings WHERE key IN ($1,$2)", settingEgressProbeURL, settingProxyHealthURL)
	probes, err := s.m.healthProbesChecked()
	if err != nil || !reflect.DeepEqual(probes, defaultHealthProbes) {
		t.Fatal(probes, err)
	}
	generation := s.m.proxyPool.generation
	if w := request(http.MethodPut, `{"egress_probe_url":"http://10.0.0.5/ip"}`, true); w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	if s.m.proxyPool.generation != generation {
		t.Fatal("IP query setting invalidated proxy health")
	}
	probes, err = s.m.healthProbesChecked()
	if err != nil || !reflect.DeepEqual(probes, defaultHealthProbes) {
		t.Fatal("health inherited IP service", probes, err)
	}
	if w := request(http.MethodPut, `{"proxy_health_check_url":"https://health.fixture.invalid/ok"}`, true); w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	if s.m.proxyPool.generation != generation+1 || s.m.EgressProbeURL() != "http://10.0.0.5/ip" {
		t.Fatal("health save changed IP settings or lost invalidation")
	}
	for _, bad := range []string{"http://health.fixture.invalid", "https://127.0.0.1", "https://name:secret@health.fixture.invalid", "https://health.fixture.invalid/#fragment"} {
		body, _ := json.Marshal(map[string]any{"proxy_health_check_url": bad, "workers": 9})
		before := s.m.Workers()
		w := request(http.MethodPut, string(body), true)
		if w.Code != 400 || s.m.Workers() != before {
			t.Fatal("invalid health URL changed settings", w.Code)
		}
	}
}

func TestProxyConnectivityNoDefaultIPQueryAndNoHealthMutation(t *testing.T) {
	s := proxyHealthServer()
	called := 0
	s.proxyProbe = func(context.Context, string, string, []string, time.Duration) egressProbeResult {
		called++
		return egressProbeResult{OK: true, ExitIP: "192.0.2.8", Probes: []egressAttempt{}}
	}
	before := s.m.proxyPoolEntries()
	for _, body := range []string{`{}`, `{"probe_url":""}`} {
		w := httptest.NewRecorder()
		s.testEgressProxy(w, httptest.NewRequest("POST", "/proxy/test", strings.NewReader(body)))
		if w.Code < 400 || called != 0 {
			t.Fatal("unconfigured IP lookup issued network requests", w.Code, called)
		}
	}
	w := httptest.NewRecorder()
	s.testEgressProxy(w, httptest.NewRequest("POST", "/proxy/test", strings.NewReader(`{"global_proxy":"socks5://proxy.fixture.invalid:1080","probe_url":"https://echo.fixture.invalid/ip","resume":true}`)))
	if w.Code != 200 || called != 2 || !reflect.DeepEqual(before, s.m.proxyPoolEntries()) {
		t.Fatal("IP query changed health", w.Code, called)
	}
}

func TestProxyConnectivityFailureDoesNotRotateBeforeThreshold(t *testing.T) {
	m := proxyTestManager(t)
	if err := m.SetProxyPoolSettings(false, 30, true, "sequential", 3, 60, 30); err != nil {
		t.Fatal(err)
	}
	m.globalProxy = "http://current.fixture.invalid:80"
	m.proxyPool = &proxyPoolState{active: "current", entries: []proxyPoolEntry{
		{ProxyPoolEntry: ProxyPoolEntry{ID: "current", Enabled: true, Health: "healthy"}, RawURL: m.globalProxy},
		{ProxyPoolEntry: ProxyPoolEntry{ID: "standby", Enabled: true, Health: "healthy"}, RawURL: "http://standby.fixture.invalid:80"},
	}}
	s := &Server{m: m, proxyProbe: func(context.Context, string, string, []string, time.Duration) egressProbeResult {
		return egressProbeResult{Error: "timeout", ErrorCode: "timeout"}
	}}
	for i := 1; i <= 3; i++ {
		s.proxyFailureCheckedAt = time.Time{}
		if !s.handleProxyPoolFailure(fmt.Errorf("socks dial error")) {
			t.Fatal("failure confirmation not consumed")
		}
		if i < 3 && m.proxyPoolActiveID() != "current" {
			t.Fatal("isolated failure rotated working route", i)
		}
	}
	if m.proxyPoolActiveID() != "standby" {
		t.Fatal("confirmed failure did not switch to standby")
	}
}
