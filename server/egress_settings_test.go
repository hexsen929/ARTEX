package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

func TestProxyEgressSettingsRemainLightweightWithLockedLargePool(t *testing.T) {
	s, _, _ := callbackSettingsFixture(t)
	previous, found, err := s.m.pg.GetSetting(settingShowEgressIP)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if found {
			_ = s.m.pg.SetSetting(settingShowEgressIP, previous)
		} else {
			_ = s.m.pg.DeleteSetting(settingShowEgressIP)
		}
	})
	if err := s.m.pg.SetSetting(settingShowEgressIP, "true"); err != nil {
		t.Fatal(err)
	}
	s.m.mu.Lock()
	u := &url.URL{Scheme: "http", Host: "proxy.invalid:8080", User: url.UserPassword("fixture-user", "fixture-secret")}
	s.m.globalProxy = u.String()
	s.m.mu.Unlock()
	entries := make([]proxyPoolEntry, 10000)
	s.m.proxyPool = &proxyPoolState{entries: entries}
	// A navbar read must not wait for health persistence's pool mutex.
	s.m.proxyPool.mu.Lock()
	locked := true
	defer func() {
		if locked {
			s.m.proxyPool.mu.Unlock()
		}
	}()
	token, err := signJWT(s.jwtKey)
	if err != nil {
		t.Fatal(err)
	}
	handler := s.Handler()
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		r := httptest.NewRequest(http.MethodGet, "/api/settings/egress", nil)
		r.Header.Set("Authorization", "Bearer "+token)
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		done <- w
	}()
	var w *httptest.ResponseRecorder
	select {
	case w = <-done:
	case <-time.After(2 * time.Second):
		s.m.proxyPool.mu.Unlock()
		locked = false
		<-done
		t.Fatal("egress settings blocked on the full proxy pool")
	}
	if w.Code != http.StatusOK {
		t.Fatalf("egress settings HTTP %d", w.Code)
	}
	var payload map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	if len(payload) != 2 || payload["show_egress_ip"] != true || payload["global_proxy"] != "http://***@proxy.invalid:8080" {
		t.Fatal("egress settings did not return exactly the two masked fields")
	}
	if strings.Contains(w.Body.String(), "fixture-secret") || w.Body.Len() > 200 {
		t.Fatal("egress settings leaked a credential or full-pool data")
	}
	unauthenticated := httptest.NewRecorder()
	handler.ServeHTTP(unauthenticated, httptest.NewRequest(http.MethodGet, "/api/settings/egress", nil))
	if unauthenticated.Code != http.StatusUnauthorized {
		t.Fatalf("egress settings accepted unauthenticated request: HTTP %d", unauthenticated.Code)
	}
}
