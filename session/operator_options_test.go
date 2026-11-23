package session

import (
	"context"
	"encoding/base64"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestOperatorXORFramingAndSecretRestore(t *testing.T) {
	key := "0123456789abcdef"
	b, err := customXORBody(key, "<?php echo 'fixture';?>")
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := base64.StdEncoding.DecodeString(string(b))
	for i := range raw {
		raw[i] ^= key[(i+1)&15]
	}
	if string(raw) != "junk| echo 'fixture';" {
		t.Fatal(string(raw))
	}
	if _, err = customXORBody("short", "fixture"); err == nil {
		t.Fatal("short key accepted")
	}
	sh, err := NewHTTPShell("http://fixture.invalid/x.php", key, "phpxor")
	if err != nil {
		t.Fatal(err)
	}
	restored, err := RestoreHTTPShell(9, sh.Secret())
	if err != nil || restored.Lang() != "phpxor" {
		t.Fatal(restored, err)
	}
}
func TestOperatorHTTPGETTransportPreservesQuery(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "GET" || r.URL.Query().Get("existing") != "yes" || r.URL.Query().Get("cmd") != "fixture" {
			t.Errorf("bad request %s %s", r.Method, r.URL)
		}
		io.WriteString(w, "response")
	}))
	defer server.Close()
	sh, err := NewHTTPShell(server.URL+"?existing=yes", "cmd", "php", HTTPOptions{Method: "GET"})
	if err != nil {
		t.Fatal(err)
	}
	out, _, err := sh.postH(context.Background(), map[string][]string{"cmd": {"fixture"}}, 0)
	if err != nil || !strings.Contains(out, "response") {
		t.Fatal(out, err)
	}
}
