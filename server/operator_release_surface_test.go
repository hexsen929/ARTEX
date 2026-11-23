package server

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"testing"
)

// Exercise the actual application mux, not a mux assembled only from operator
// routes. This catches a main/release merge that retains the library on disk
// but forgets to expose its API in the shipped application.
func TestOperatorReleaseSurfaceUsesFullApplicationHandler(t *testing.T) {
	t.Setenv("ARTEX_KB_DIR", t.TempDir())
	m, err := NewManager(t.TempDir(), "")
	if err != nil {
		t.Skipf("postgres unavailable (%v) — skipping", err)
	}
	t.Cleanup(func() { _ = m.Close() })
	s := newTestServer(t, context.Background(), m, t.TempDir(), t.TempDir(), t.TempDir())
	token, err := signJWT(s.jwtKey, 0)
	if err != nil {
		t.Fatal(err)
	}
	h := s.Handler()
	for _, tc := range []struct {
		path, key string
	}{
		{"/api/kb/status", "total"},
		{"/api/kb/search?q=operator-release-synthetic-fixture", "ammo"},
		{"/api/kb/config", "preflight"},
		{"/api/operator/settings", "armor"},
		{"/api/c2/listeners", "items"},
		{"/api/webshell", "items"},
		{"/api/vps", "vps"},
	} {
		t.Run(tc.path, func(t *testing.T) {
			r := httptest.NewRequest("GET", tc.path, nil)
			r.Header.Set("Authorization", "Bearer "+token)
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			if w.Code != 200 {
				t.Fatalf("release module route %s: HTTP %d", tc.path, w.Code)
			}
			var payload map[string]json.RawMessage
			if err := json.Unmarshal(w.Body.Bytes(), &payload); err != nil {
				t.Fatalf("route returned no JSON object: %v", err)
			}
			if _, ok := payload[tc.key]; !ok {
				t.Fatalf("route %s lost field %q", tc.path, tc.key)
			}
		})
	}
}
