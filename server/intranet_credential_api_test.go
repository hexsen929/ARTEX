package server

import (
	"bytes"
	"context"
	"encoding/json"
	"log"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/Autumn-27/artex/db"
)

func TestCredentialSameOrigin(t *testing.T) {
	for _, tc := range []struct {
		origin, host, site string
		want               bool
	}{
		{"", "localhost:8787", "", true},
		{"http://localhost:8787", "localhost:8787", "same-origin", true},
		{"http://localhost:3000", "localhost:8787", "same-site", true},
		{"http://127.0.0.1:3000", "127.0.0.1:8787", "same-site", true},
		{"http://[::1]:3000", "[::1]:8787", "same-site", true},
		{"https://console.invalid", "console.invalid", "", true},
		{"http://localhost:3000", "127.0.0.1:8787", "", false},
		{"https://evil.invalid", "localhost:8787", "", false},
		{"null", "localhost:8787", "", false},
		{"http://user:pass@localhost:8787", "localhost:8787", "", false},
		{"http://localhost:8787", "localhost:8787", "cross-site", false},
		{"http://console.invalid:3000", "console.invalid:8787", "", false},
	} {
		r := httptest.NewRequest("POST", "http://"+tc.host+"/api/intranet/credentials/1/reveal", nil)
		r.Header.Set("Origin", tc.origin)
		r.Header.Set("Sec-Fetch-Site", tc.site)
		if got := credentialSameOrigin(r); got != tc.want {
			t.Errorf("origin %q host %q site %q got %v", tc.origin, tc.host, tc.site, got)
		}
	}
}

func TestCredentialDTOIsMaskedAndHasTraceability(t *testing.T) {
	raw, err := json.Marshal(credentialItem(&db.CredentialRecord{ID: 1, TaskID: 2, TaskName: "fixture-task", TaskAvailable: true, Secret: "private-fixture-secret", VerificationStatus: "unverified", VerificationHistory: []db.CredentialVerificationEvent{}}))
	if err != nil {
		t.Fatal(err)
	}
	text := string(raw)
	if strings.Contains(text, "private-fixture-secret") || strings.Contains(text, `"secret":`) {
		t.Fatal("list DTO leaked secret")
	}
	for _, want := range []string{`"task_name":"fixture-task"`, `"task_available":true`, `"verification_status":"unverified"`, `"secret_masked":"pr****et"`} {
		if !strings.Contains(text, want) {
			t.Errorf("missing %s", want)
		}
	}
}

func TestCredentialEndpointsRejectCrossSiteAndMalformedBodies(t *testing.T) {
	s := &Server{}
	handlers := []struct {
		name, method string
		handler      http.HandlerFunc
	}{
		{"reveal", "POST", s.intranetRevealCredential},
		{"verification", "PUT", s.intranetUpdateCredentialVerification},
		{"metadata", "PATCH", s.intranetUpdateCredentialMetadata},
	}
	for _, h := range handlers {
		t.Run(h.name, func(t *testing.T) {
			r := httptest.NewRequest(h.method, "http://localhost:8787/api/intranet/credentials/1/"+h.name, strings.NewReader(`{"purpose":"view"}`))
			r.SetPathValue("id", "1")
			r.Header.Set("Origin", "https://evil.invalid")
			r.Header.Set("Content-Type", "application/json")
			w := httptest.NewRecorder()
			h.handler(w, r)
			if w.Code != 403 || w.Header().Get("Cache-Control") != "no-store" || w.Header().Get("Pragma") != "no-cache" || w.Header().Get("X-Content-Type-Options") != "nosniff" {
				t.Fatalf("cross-site/cache protection failed: %d", w.Code)
			}
			for _, body := range []string{`{"secret":"do-not-echo"}`, `{} {}`, strings.Repeat(" ", 17<<10) + `{}`} {
				r := httptest.NewRequest(h.method, "http://localhost:8787/api/intranet/credentials/1/"+h.name, strings.NewReader(body))
				r.SetPathValue("id", "1")
				r.Header.Set("Content-Type", "application/json")
				w := httptest.NewRecorder()
				h.handler(w, r)
				if w.Code != 400 || strings.Contains(w.Body.String(), "do-not-echo") {
					t.Fatalf("invalid request was accepted/echoed: %d %s", w.Code, w.Body.String())
				}
			}
		})
	}
}

func TestCredentialSensitiveRoutesRequireBearer(t *testing.T) {
	s := &Server{jwtKey: []byte("credential-auth-fixture-key")}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/intranet/credentials/{id}/reveal", s.intranetRevealCredential)
	mux.HandleFunc("PUT /api/intranet/credentials/{id}/verification", s.intranetUpdateCredentialVerification)
	mux.HandleFunc("PATCH /api/intranet/credentials/{id}/metadata", s.intranetUpdateCredentialMetadata)
	handler := s.requireAuth(mux)
	token, err := signJWT(s.jwtKey)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ method, path, body string }{
		{"POST", "reveal", `{"purpose":"view"}`},
		{"PUT", "verification", `{"status":"verified","method":"manual","evidence":"fixture"}`},
		{"PATCH", "metadata", `{"target":"fixture.invalid"}`},
	} {
		for _, authenticated := range []bool{false, true} {
			r := httptest.NewRequest(tc.method, "http://localhost:8787/api/intranet/credentials/1/"+tc.path, strings.NewReader(tc.body))
			r.Header.Set("Content-Type", "application/json")
			if authenticated {
				r.Header.Set("Authorization", "Bearer "+token)
			}
			w := httptest.NewRecorder()
			handler.ServeHTTP(w, r)
			want := 401
			if authenticated {
				want = 503
			}
			if w.Code != want {
				t.Errorf("%s authenticated=%v status=%d want=%d body=%s", tc.path, authenticated, w.Code, want, w.Body.String())
			}
		}
	}
}

func TestCredentialManagementHTTPRoundtripAndAudit(t *testing.T) {
	pg := openIntranetTestDB(t)
	task, err := pg.CreateTask("credential HTTP fixture", "fixture", nil, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = pg.Exec(`DELETE FROM credentials WHERE task_id=$1`, task.ID)
		_, _ = pg.Exec(`DELETE FROM task_archives WHERE task_id=$1`, task.ID)
		_ = pg.DeleteTask(task.ID)
	})
	key := []byte("credential-http-fixture-key")
	store := db.NewCredentialStore(pg, key)
	id, err := store.Create(t.Context(), &db.CredentialRecord{TaskID: task.ID, Username: "fixture-user", CredType: db.CredToken, Secret: "fixture-secret-never-log", Source: "original source"})
	if err != nil {
		t.Fatal(err)
	}
	s := &Server{credStore: store, jwtKey: key}
	token, err := signJWT(key)
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/intranet/credentials/{id}/reveal", s.intranetRevealCredential)
	mux.HandleFunc("PUT /api/intranet/credentials/{id}/verification", s.intranetUpdateCredentialVerification)
	mux.HandleFunc("PATCH /api/intranet/credentials/{id}/metadata", s.intranetUpdateCredentialMetadata)
	handler := s.requireAuth(mux)
	var audit bytes.Buffer
	previous := log.Writer()
	log.SetOutput(&audit)
	t.Cleanup(func() { log.SetOutput(previous) })
	request := func(method, action, body string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, "http://localhost:8787/api/intranet/credentials/"+strconv.FormatInt(id, 10)+"/"+action, strings.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("Authorization", "Bearer "+token)
		r.Header.Set("Origin", "http://localhost:8787")
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		return w
	}
	for _, purpose := range []string{"view", "copy"} {
		w := request("POST", "reveal", `{"purpose":"`+purpose+`"}`)
		var result struct {
			Secret string `json:"secret"`
		}
		if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &result) != nil || result.Secret != "fixture-secret-never-log" || w.Header().Get("Cache-Control") != "no-store" {
			t.Fatal("reveal roundtrip failed")
		}
	}
	w := request("PUT", "verification", `{"status":"verified","method":"manual","evidence":"fixture-secret-never-log"}`)
	if w.Code != 400 || strings.Contains(w.Body.String(), "fixture-secret-never-log") {
		t.Fatal("raw secret evidence accepted/echoed")
	}
	for _, status := range []string{"verified", "failed", "unverified"} {
		w = request("PUT", "verification", `{"status":"`+status+`","method":"manual","evidence":"fixture evidence"}`)
		if w.Code != 200 || strings.Contains(w.Body.String(), `"secret":`) || strings.Contains(w.Body.String(), "fixture-secret-never-log") {
			t.Fatal("verification roundtrip leaked/failed")
		}
	}
	w = request("PATCH", "metadata", `{"target":"https://target.invalid/login?token=discard","domain":"CORP","source_url":"https://source.invalid/evidence?token=discard"}`)
	if w.Code != 200 || strings.Contains(w.Body.String(), "discard") || strings.Contains(w.Body.String(), `"secret":`) {
		t.Fatal("metadata roundtrip leaked/failed")
	}
	current, err := store.Get(context.Background(), id)
	if err != nil || current.Source != "original source" || current.Secret != "fixture-secret-never-log" || current.VerificationStatus != "unverified" || len(current.VerificationHistory) != 3 {
		t.Fatal("HTTP operations changed original data/lost history")
	}
	if strings.Contains(audit.String(), "fixture-secret-never-log") || strings.Contains(audit.String(), "fixture evidence") || !strings.Contains(audit.String(), "credential_reveal_copy") || !strings.Contains(audit.String(), "verification_contains_secret") {
		t.Fatal("audit leaked content or omitted operations")
	}
	var ciphertext string
	if err = pg.QueryRow(`SELECT secret FROM credentials WHERE id=$1`, id).Scan(&ciphertext); err != nil {
		t.Fatal(err)
	}
	s.credStore = db.NewCredentialStore(pg, []byte("deliberately-wrong-fixture-key"))
	w = request("POST", "reveal", `{"purpose":"view"}`)
	if w.Code != 500 || strings.Contains(w.Body.String(), ciphertext) || strings.Contains(w.Body.String(), "gcm1:") || strings.Contains(w.Body.String(), "fixture-secret-never-log") {
		t.Fatal("decryption error echoed ciphertext/plaintext")
	}
	if strings.Contains(audit.String(), ciphertext) || strings.Contains(audit.String(), "fixture-secret-never-log") || !strings.Contains(audit.String(), "read_error") {
		t.Fatal("decryption error leaked into audit or was unaudited")
	}
	s.credStore = store
	if err = pg.SetPaused(task.ID, true); err != nil {
		t.Fatal(err)
	}
	if _, err = pg.QueueTaskArchive(task.ID); err != nil {
		t.Fatal(err)
	}
	w = request("POST", "reveal", `{"purpose":"view"}`)
	if w.Code != 409 || strings.Contains(w.Body.String(), "fixture-secret-never-log") {
		t.Fatal("reveal crossed archive queue")
	}
}
