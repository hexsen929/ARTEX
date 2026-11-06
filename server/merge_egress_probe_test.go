package server

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/Autumn-27/artex/db"
)

func TestMergeSavedProbeStorageFailureDoesNotSendOrPublish(t *testing.T) {
	// Opening and closing a database/sql pool does not dial its DSN. The closed
	// pool deterministically rejects reads without touching a running database.
	conn, err := sql.Open("pgx", "postgres://fixture:fixture@127.0.0.1:1/fixture?sslmode=disable")
	if err != nil {
		t.Fatal(err)
	}
	if err := conn.Close(); err != nil {
		t.Fatal(err)
	}
	entries := proxyScaleEntries(1)
	m := &Manager{pg: &db.DB{DB: conn}, globalProxy: entries[0].RawURL,
		proxyPool: &proxyPoolState{entries: entries, active: entries[0].ID}}
	s := &Server{m: m}
	s.proxyProbe = func(context.Context, string, string, []string, time.Duration) egressProbeResult {
		t.Error("a failed settings read must not send a public/default probe")
		return egressProbeResult{OK: true}
	}
	if probes, err := m.egressProbesChecked(); err == nil || len(probes) != 0 {
		t.Fatal("storage failure was interpreted as an empty/default selection")
	}
	if _, err := m.EgressProbeURLChecked(); err == nil || strings.Contains(err.Error(), "postgres") {
		t.Fatal("checked getter omitted or exposed the storage failure")
	}
	for _, handler := range []struct {
		name string
		fn   http.HandlerFunc
	}{
		{"manual", s.testEgressProxy},
		{"single", s.testProxyPoolEntry},
		{"diagnostic", s.diagnoseProxyPoolEntry},
	} {
		t.Run(handler.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodPost, "/fixture", strings.NewReader(`{}`))
			r.SetPathValue("id", entries[0].ID)
			w := httptest.NewRecorder()
			handler.fn(w, r)
			if w.Code != http.StatusServiceUnavailable {
				t.Fatalf("storage failure HTTP %d", w.Code)
			}
		})
	}
	s.proxyPoolTickLocked(true)
	if !s.handleProxyPoolFailure(errors.New("proxyconnect tcp: fixture failure")) {
		t.Fatal("settings failure was not consumed before proxy-failure handling")
	}
	if !reflect.DeepEqual(m.proxyPoolEntries(), entries) || m.GlobalProxy() != entries[0].RawURL {
		t.Fatal("settings failure changed health or routing")
	}
}

func TestMergeSavedProbeInvalidValueRemainsRepairableWithoutNetwork(t *testing.T) {
	m := proxyTestManager(t)
	preserveEgressProbeSetting(t, m)
	const invalid = "http://127.0.0.1:8787/private-setting"
	if err := m.pg.SetSetting(settingEgressProbeURL, invalid); err != nil {
		t.Fatal(err)
	}
	raw, err := m.EgressProbeURLChecked()
	if err != nil || raw != invalid {
		t.Fatal("invalid legacy draft was hidden instead of remaining repairable")
	}
	if probes, err := m.egressProbesChecked(); err == nil || probes != nil {
		t.Fatal("invalid legacy service selected public defaults")
	}
	s := &Server{m: m}
	s.proxyProbe = func(context.Context, string, string, []string, time.Duration) egressProbeResult {
		t.Error("invalid saved service must not be probed")
		return egressProbeResult{OK: true}
	}
	w := httptest.NewRecorder()
	s.testEgressProxy(w, httptest.NewRequest(http.MethodPost, "/fixture", strings.NewReader(`{}`)))
	if w.Code != http.StatusServiceUnavailable || strings.Contains(w.Body.String(), "private-setting") {
		t.Fatal("invalid saved service was sent or exposed in an error")
	}
}

func TestMergeSavedProbeChangeDiscardsWholeRoundAndKeepsForce(t *testing.T) {
	m := proxyTestManager(t)
	preserveEgressProbeSetting(t, m)
	const previous, next = "http://10.0.0.5/ip", "https://probe.fixture.invalid/ip"
	if err := m.pg.SetSetting(settingEgressProbeURL, previous); err != nil {
		t.Fatal(err)
	}
	entries := proxyScaleEntries(1)
	m.globalProxy = entries[0].RawURL
	m.proxyPool = &proxyPoolState{entries: entries, active: entries[0].ID}
	s := &Server{m: m, proxyPoolWake: make(chan struct{}, 1)}
	calls := 0
	s.proxyProbe = func(_ context.Context, _, _ string, probes []string, _ time.Duration) egressProbeResult {
		calls++
		if !reflect.DeepEqual(probes, []string{previous}) {
			t.Error("round did not use its configured probe service")
		}
		if err := m.pg.SetSetting(settingEgressProbeURL, next); err != nil {
			t.Error(err)
		}
		s.invalidateEgressProbeSelection()
		return egressProbeResult{Error: "fixture failed old service", ErrorCode: "timeout"}
	}
	s.proxyPoolTickLocked(true)
	if calls != 1 || !reflect.DeepEqual(entries, m.proxyPoolEntries()) || !m.proxyPool.forceHealth || len(s.proxyPoolWake) != 1 {
		t.Fatal("superseded service changed health or lost its forced background retry")
	}
}

func TestMergeSavedProbeDiagnosticAvoidsPublicFallbackAndTrustClaims(t *testing.T) {
	for _, custom := range []string{"http://10.0.0.5/ip", "https://probe.fixture.invalid/ip"} {
		t.Run(strings.Split(custom, ":")[0], func(t *testing.T) {
			m := proxyTestManager(t)
			preserveEgressProbeSetting(t, m)
			if err := m.pg.SetSetting(settingEgressProbeURL, custom); err != nil {
				t.Fatal(err)
			}
			entries := proxyScaleEntries(1)
			m.globalProxy = entries[0].RawURL
			m.proxyPool = &proxyPoolState{entries: entries, active: entries[0].ID}
			s := &Server{m: m}
			calls := 0
			s.proxyProbe = func(_ context.Context, _, _ string, probes []string, _ time.Duration) egressProbeResult {
				calls++
				if !reflect.DeepEqual(probes, []string{custom}) {
					t.Error("configured diagnosis contacted a default public echo")
				}
				return egressProbeResult{OK: true, ExitIP: "192.0.2.5", Probes: []egressAttempt{}}
			}
			r := httptest.NewRequest(http.MethodPost, "/fixture", nil)
			r.SetPathValue("id", entries[0].ID)
			w := httptest.NewRecorder()
			s.diagnoseProxyPoolEntry(w, r)
			var payload struct {
				Mode          string            `json:"probe_mode"`
				URL           string            `json:"probe_url"`
				HTTPS         egressProbeResult `json:"https"`
				HTTP          egressProbeResult `json:"http"`
				HTTPSkipped   bool              `json:"http_skipped"`
				HealthUpdated bool              `json:"health_updated"`
			}
			if err := json.Unmarshal(w.Body.Bytes(), &payload); err != nil {
				t.Fatal(err)
			}
			if w.Code != http.StatusOK || calls != 1 || payload.Mode != "configured" || payload.URL != custom || !payload.HTTPSkipped || payload.HTTP.OK || !payload.HTTPS.OK || payload.HealthUpdated {
				t.Fatal("configured diagnosis inferred HTTPS trust, ran another echo, or updated health")
			}
			if !reflect.DeepEqual(entries, m.proxyPoolEntries()) || m.GlobalProxy() != entries[0].RawURL {
				t.Fatal("diagnosis changed health or route")
			}
		})
	}
}
