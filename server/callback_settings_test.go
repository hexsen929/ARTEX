package server

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/Autumn-27/artex/config"
	"github.com/Autumn-27/artex/db"
	"github.com/Autumn-27/artex/tunnel"
)

func callbackSettingsFixture(t *testing.T) (*Server, func(string, string, bool) *httptest.ResponseRecorder, string) {
	t.Helper()
	t.Setenv("ARTEX_CALLBACK_ADDR", "")
	m, err := NewManager(t.TempDir(), "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = m.Close() })
	previous, existed, err := m.pg.GetSetting(config.CallbackAddrSetting)
	if err != nil {
		t.Fatal(err)
	}
	if err := m.pg.DeleteSetting(config.CallbackAddrSetting); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if existed {
			_ = m.pg.SetSetting(config.CallbackAddrSetting, previous)
		} else {
			_ = m.pg.DeleteSetting(config.CallbackAddrSetting)
		}
	})
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(`{"callback_addr":"file.callback.test:443"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ARTEX_CONFIG", path)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	s := newTestServer(t, ctx, m, t.TempDir(), t.TempDir(), t.TempDir())
	token, err := signJWT(s.jwtKey)
	if err != nil {
		t.Fatal(err)
	}
	handler := s.Handler()
	request := func(method, body string, authenticated bool) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, "/api/settings", bytes.NewBufferString(body))
		req.Header.Set("Content-Type", "application/json")
		if authenticated {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		out := httptest.NewRecorder()
		handler.ServeHTTP(out, req)
		return out
	}
	return s, request, path
}

func callbackPayload(t *testing.T, response *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	if response.Code != http.StatusOK {
		t.Fatalf("settings HTTP %d: %s", response.Code, response.Body)
	}
	var payload map[string]any
	if err := json.Unmarshal(response.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	return payload
}

func TestCallbackSettingsApplyLiveAndClearWithoutChangingExistingPlan(t *testing.T) {
	s, request, _ := callbackSettingsFixture(t)
	if got := request(http.MethodPut, `{"callback_addr":"new.callback.test"}`, false).Code; got != http.StatusUnauthorized {
		t.Fatalf("unauthenticated settings write: %d", got)
	}
	initial := callbackPayload(t, request(http.MethodGet, "", true))
	if initial["callback_addr"] != "file.callback.test:443" || initial["callback_addr_source"] != "config" || initial["callback_addr_locked"] != false {
		t.Fatalf("initial source: %#v", initial)
	}
	oldHost, err := s.callbackHost()
	if err != nil {
		t.Fatal(err)
	}
	// An existing plan owns its address; live settings only affect new requests.
	existing, err := tunnel.PlanSocks(&tunnel.PlanOpts{
		TaskID: 1, ViaSessionID: 1, CallbackHost: oldHost,
		PortMin: 20000, PortMax: 20100, DataDir: t.TempDir(),
		CanListen: func(int) bool { return true },
	})
	if err != nil {
		t.Fatal(err)
	}
	beforePlan, _ := json.Marshal(existing)
	for _, address := range []string{"First.Callback.Test:8443", "[2001:db8::15]:443", ""} {
		body, _ := json.Marshal(map[string]any{"callback_addr": address})
		payload := callbackPayload(t, request(http.MethodPut, string(body), true))
		want, _ := config.ValidateCallbackAddr(address)
		if payload["callback_addr"] != want || payload["callback_addr_source"] != "settings" || payload["callback_addr_locked"] != false {
			t.Fatalf("saved response: %#v", payload)
		}
		// Fresh Server and repeated runtime reads use the persisted value without
		// NewManager, applyLLM, listener creation or process restart.
		fresh := &Server{m: s.m}
		if want == "" {
			if _, err := fresh.callbackHost(); err == nil {
				t.Fatal("explicit clear fell back to the configured file")
			}
		} else {
			host, err := fresh.callbackHost()
			wantHost, _ := config.CallbackHostname(want)
			if err != nil || host != wantHost {
				t.Fatalf("runtime host=%q err=%v want=%q", host, err, wantHost)
			}
		}
		afterPlan, _ := json.Marshal(existing)
		if !bytes.Equal(beforePlan, afterPlan) {
			t.Fatalf("existing plan was rewritten: %+v", existing)
		}
	}
}

func TestCallbackSettingsValidateBeforeMutationAndRespectEnvironmentLock(t *testing.T) {
	s, request, _ := callbackSettingsFixture(t)
	beforeWorkers := s.m.Workers()
	if response := request(http.MethodPut, `{"callback_addr":"http://invalid.test","workers":7}`, true); response.Code != http.StatusBadRequest {
		t.Fatalf("invalid address status=%d: %s", response.Code, response.Body)
	}
	if _, found, err := s.m.pg.GetSetting(config.CallbackAddrSetting); err != nil || found {
		t.Fatalf("invalid callback was written: %v %v", found, err)
	}
	if s.m.Workers() != beforeWorkers {
		t.Fatal("invalid callback request mutated unrelated settings")
	}
	t.Setenv("ARTEX_CALLBACK_ADDR", "ENV.Callback.Test:443")
	payload := callbackPayload(t, request(http.MethodGet, "", true))
	if payload["callback_addr"] != "env.callback.test:443" || payload["callback_addr_source"] != "env" || payload["callback_addr_locked"] != true {
		t.Fatalf("environment source: %#v", payload)
	}
	response := request(http.MethodPut, `{"callback_addr":"","workers":9}`, true)
	if response.Code != http.StatusConflict || s.m.Workers() != beforeWorkers {
		t.Fatalf("locked patch mutated settings: %d workers=%d", response.Code, s.m.Workers())
	}
	// Unrelated changes remain available under a managed callback environment.
	if got := request(http.MethodPut, `{}`, true).Code; got != http.StatusOK {
		t.Fatalf("unrelated settings blocked: %d", got)
	}
}

func TestCallbackSettingsClosedStoreDoesNotFallback(t *testing.T) {
	t.Setenv("ARTEX_CALLBACK_ADDR", "")
	closed, err := sql.Open("pgx", "postgres://callback-fixture.invalid/unused")
	if err != nil {
		t.Fatal(err)
	}
	if err := closed.Close(); err != nil {
		t.Fatal(err)
	}
	s := &Server{m: &Manager{pg: &db.DB{DB: closed}}}
	for _, method := range []string{http.MethodGet, http.MethodPut} {
		response := httptest.NewRecorder()
		request := httptest.NewRequest(method, "/api/settings", bytes.NewBufferString(`{"callback_addr":"valid.callback.test"}`))
		if method == http.MethodGet {
			s.getSettings(response, request)
		} else {
			s.putSettings(response, request)
		}
		if response.Code != http.StatusInternalServerError {
			t.Fatalf("closed store %s status=%d: %s", method, response.Code, response.Body)
		}
	}
	if _, err := s.callbackHost(); err == nil {
		t.Fatal("closed store fell back to the configured file")
	}
}

func TestExplicitHTTPTunnelDoesNotRequireCallbackConfiguration(t *testing.T) {
	s := &Server{}
	if host, err := s.callbackHostForTunnel(tunnel.AdapterSuo5); err != nil || host != "" {
		t.Fatalf("HTTP tunnel requires callback storage: host=%q err=%v", host, err)
	}
	for _, adapter := range []string{"auto", tunnel.AdapterChisel} {
		if _, err := s.callbackHostForTunnel(adapter); err == nil {
			t.Fatalf("%s silently ignored missing callback settings", adapter)
		}
	}
}

func TestCallbackSettingsMalformedOldValueCanBeRepaired(t *testing.T) {
	s, request, path := callbackSettingsFixture(t)
	for _, source := range []string{"config", "settings"} {
		if source == "config" {
			if err := os.WriteFile(path, []byte(`{"callback_addr":"https://old.invalid.test"}`), 0o600); err != nil {
				t.Fatal(err)
			}
		} else if err := s.m.pg.SetSetting(config.CallbackAddrSetting, "old;invalid"); err != nil {
			t.Fatal(err)
		}
		payload := callbackPayload(t, request(http.MethodGet, "", true))
		if payload["callback_addr_error"] == nil || payload["callback_addr_source"] != source {
			t.Fatalf("invalid saved value was hidden: %#v", payload)
		}
		if _, err := s.callbackHost(); err == nil {
			t.Fatal("invalid old value used by the runtime")
		}
		if got := request(http.MethodPut, `{"callback_addr":"repaired.callback.test"}`, true).Code; got != http.StatusOK {
			t.Fatalf("valid repair blocked: %d", got)
		}
		payload = callbackPayload(t, request(http.MethodPut, `{"callback_addr":""}`, true))
		if payload["callback_addr"] != "" || payload["callback_addr_error"] != nil {
			t.Fatalf("clear did not repair: %#v", payload)
		}
		if err := s.m.pg.DeleteSetting(config.CallbackAddrSetting); err != nil {
			t.Fatal(err)
		}
	}
}
