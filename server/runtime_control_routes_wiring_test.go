package server

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Autumn-27/artex/db"
)

func runtimeControlRouteFixture(t *testing.T) (*Server, func(string, string, string, bool) *httptest.ResponseRecorder) {
	t.Helper()
	s, _ := controlsWiringFixture(t)
	token, err := signJWT(s.jwtKey)
	if err != nil {
		t.Fatal(err)
	}
	handler := s.Handler()
	return s, func(method, key, body string, authenticated bool) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, "/api/settings/runtime-controls/"+key, bytes.NewBufferString(body))
		r.Header.Set("Content-Type", "application/json")
		if authenticated {
			r.Header.Set("Authorization", "Bearer "+token)
		}
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		return w
	}
}

func runtimeControlResponse(t *testing.T, w *httptest.ResponseRecorder, key string, want any) {
	t.Helper()
	if w.Code != http.StatusOK {
		t.Fatalf("control response HTTP %d: %s", w.Code, w.Body)
	}
	var value map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &value); err != nil {
		t.Fatal(err)
	}
	if len(value) != 1 || value[key] != want {
		t.Fatalf("single control response=%#v, want only %s=%v", value, key, want)
	}
}

func TestRuntimeControlRoutesDefaultsDoNotPersistAndRequireAuthentication(t *testing.T) {
	s, request := runtimeControlRouteFixture(t)
	for _, tc := range []struct {
		key   string
		value any
	}{
		{settingRoEEnforcement, "warn"},
		{settingFindingVerifyMode, "auto"},
		{settingLLMRecordsRetentionDays, float64(30)},
	} {
		if got := request(http.MethodGet, tc.key, "", false); got.Code != http.StatusUnauthorized {
			t.Fatalf("unauthenticated control read accepted: %d", got.Code)
		}
		runtimeControlResponse(t, request(http.MethodGet, tc.key, "", true), tc.key, tc.value)
		if _, found, err := s.m.pg.GetSetting(tc.key); err != nil || found {
			t.Fatalf("GET persisted default %s", tc.key)
		}
	}
	if got := request(http.MethodPut, settingRoEEnforcement, `{"value":"off"}`, false); got.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated policy update accepted: %d", got.Code)
	}
	for _, key := range []string{"unknown", "auth.password_hash", "workers", settingNotifyEnabled} {
		for _, method := range []string{http.MethodGet, http.MethodPut} {
			if got := request(method, key, `{"value":"off"}`, true); got.Code != http.StatusNotFound {
				t.Fatalf("non-whitelisted key %s accepted by %s: %d", key, method, got.Code)
			}
		}
	}
}

func TestRuntimeControlRoutesRepairMultipleBadPeersIndependently(t *testing.T) {
	s, request := runtimeControlRouteFixture(t)
	for key, value := range map[string]string{settingRoEEnforcement: "bad-roe", settingFindingVerifyMode: "bad-verify", settingLLMRecordsRetentionDays: "bad-days"} {
		if err := s.m.pg.SetSetting(key, value); err != nil {
			t.Fatal(err)
		}
	}
	fingerprint := func() string {
		t.Helper()
		var hash string
		if err := s.m.pg.QueryRow(`SELECT md5(COALESCE(string_agg(row_to_json(s)::text,'|' ORDER BY key),'')) FROM settings s WHERE key NOT IN($1,$2,$3)`, settingRoEEnforcement, settingFindingVerifyMode, settingLLMRecordsRetentionDays).Scan(&hash); err != nil {
			t.Fatal(err)
		}
		return hash
	}
	before := fingerprint()
	runtimeControlResponse(t, request(http.MethodPut, settingRoEEnforcement, `{"value":"strict"}`, true), settingRoEEnforcement, "strict")
	runtimeControlResponse(t, request(http.MethodGet, settingRoEEnforcement, "", true), settingRoEEnforcement, "strict")
	if value, _, _ := s.m.pg.GetSetting(settingFindingVerifyMode); value != "bad-verify" {
		t.Fatal("repairing RoE changed a peer verification setting")
	}
	if value, _, _ := s.m.pg.GetSetting(settingLLMRecordsRetentionDays); value != "bad-days" {
		t.Fatal("repairing RoE changed a peer retention setting")
	}
	// The complete snapshot remains strict instead of inventing peer defaults.
	w := httptest.NewRecorder()
	s.getSettings(w, httptest.NewRequest(http.MethodGet, "/api/settings", nil))
	if w.Code != http.StatusInternalServerError {
		t.Fatal("full Settings snapshot silently accepted malformed peers")
	}
	runtimeControlResponse(t, request(http.MethodPut, settingLLMRecordsRetentionDays, `{"value":0}`, true), settingLLMRecordsRetentionDays, float64(0))
	if value, _, _ := s.m.pg.GetSetting(settingFindingVerifyMode); value != "bad-verify" {
		t.Fatal("repairing retention changed a peer verification setting")
	}
	runtimeControlResponse(t, request(http.MethodPut, settingFindingVerifyMode, `{"value":"off"}`, true), settingFindingVerifyMode, "off")
	if fingerprint() != before {
		t.Fatal("single-key repairs changed unrelated settings")
	}
	w = httptest.NewRecorder()
	s.getSettings(w, httptest.NewRequest(http.MethodGet, "/api/settings", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("independent repairs did not restore complete snapshot: %d %s", w.Code, w.Body)
	}
}

func TestRuntimeControlRoutesRejectAmbiguousBodyAndWrongValueTypes(t *testing.T) {
	s, request := runtimeControlRouteFixture(t)
	if err := s.m.pg.SetSetting(settingRoEEnforcement, "strict"); err != nil {
		t.Fatal(err)
	}
	for _, body := range []string{
		``, `null`, `[]`, `true`, `{}`, `{"value":null}`, `{"value":""}`, `{"value":"WARN"}`,
		`{"value":true}`, `{"value":0}`, `{"value":[]}`, `{"value":{}}`,
		`{"value":"warn","workers":9}`, `{"value":"warn","value":"off"}`, `{"value":"warn"} {}`,
		`{"value":"` + strings.Repeat("x", 5000) + `"}`,
	} {
		if got := request(http.MethodPut, settingRoEEnforcement, body, true); got.Code != http.StatusBadRequest {
			t.Fatalf("ambiguous body accepted: %d %s", got.Code, got.Body)
		}
		if value, _, _ := s.m.pg.GetSetting(settingRoEEnforcement); value != "strict" {
			t.Fatal("invalid single-control request changed saved policy")
		}
	}
	for _, body := range []string{`{"value":null}`, `{"value":-1}`, `{"value":106752}`, `{"value":1.5}`, `{"value":"1"}`} {
		if got := request(http.MethodPut, settingLLMRecordsRetentionDays, body, true); got.Code != http.StatusBadRequest {
			t.Fatal("invalid retention accepted")
		}
	}
	if got := request(http.MethodPut, settingFindingVerifyMode, `{"value":"strict"}`, true); got.Code != http.StatusBadRequest {
		t.Fatal("unrelated policy enum accepted as verification mode")
	}
}

func TestRuntimeControlRoutesStorageFailureCannotReportSuccess(t *testing.T) {
	closed, err := sql.Open("pgx", "postgres://runtime-control-fixture.invalid/unused")
	if err != nil {
		t.Fatal(err)
	}
	_ = closed.Close()
	s := &Server{m: &Manager{pg: &db.DB{DB: closed}}}
	for _, method := range []string{http.MethodGet, http.MethodPut} {
		w := httptest.NewRecorder()
		r := httptest.NewRequest(method, "/unused", bytes.NewBufferString(`{"value":"strict"}`))
		r.SetPathValue("key", settingRoEEnforcement)
		if method == http.MethodGet {
			s.getRuntimeControl(w, r)
		} else {
			s.putRuntimeControl(w, r)
		}
		if w.Code != http.StatusInternalServerError || strings.Contains(w.Body.String(), `"roe_enforcement":"strict"`) {
			t.Fatalf("storage failure produced confirmed success: %d %s", w.Code, w.Body)
		}
	}
}
