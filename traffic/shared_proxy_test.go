package traffic

import (
	"context"
	"crypto/sha256"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// All network traffic in this file stays on loopback. The parent recorder need
// not listen: child listeners feed its sink/index/body store directly.
func sharedProxyAddress(t *testing.T) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := listener.Addr().String()
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
	return address
}

func startSharedProxy(t *testing.T, child *Traffic) {
	t.Helper()
	result := make(chan error, 1)
	go func() { result <- child.Start() }()
	t.Cleanup(func() {
		_ = child.Close()
		select {
		case <-result:
		case <-time.After(3 * time.Second):
			t.Error("shared proxy listener did not stop")
		}
	})
	deadline := time.Now().Add(3 * time.Second)
	for {
		select {
		case err := <-result:
			// Keep the channel readable by cleanup even after startup failed.
			result <- err
			t.Fatalf("shared proxy listener exited before readiness: %v", err)
		default:
		}
		connection, err := net.DialTimeout("tcp", child.addr, 50*time.Millisecond)
		if err == nil {
			_ = connection.Close()
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("shared proxy did not become ready: %v", err)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func waitSharedProxyCount(t *testing.T, recorder *Traffic, want int) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for {
		count, err := recorder.Count()
		if err != nil {
			t.Fatalf("shared recorder Count: %v", err)
		}
		if count == want {
			return
		}
		if count > want || time.Now().After(deadline) {
			t.Fatalf("shared recorder count=%d, want %d", count, want)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestSharedProxyRecordsIntoParentAndPreservesEvidenceReads(t *testing.T) {
	t.Setenv("ARTEX_PROXY_AUTH", "shared-fixture:fixture-password")
	parent, _ := openTraffic(t)
	runtimeDir := t.TempDir()
	child, err := parent.OpenProxy(runtimeDir, sharedProxyAddress(t))
	if err != nil {
		t.Fatal(err)
	}
	startSharedProxy(t, child)

	const marker = "shared-recorder-fixture-body"
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		if request.Header.Get("Proxy-Authorization") != "" {
			t.Error("proxy credentials reached the loopback origin")
		}
		w.Header().Set("Content-Type", "text/plain")
		_, _ = io.WriteString(w, marker)
	}))
	t.Cleanup(upstream.Close)
	proxyURL, err := url.Parse(child.ProxyAddr())
	if err != nil {
		t.Fatal(err)
	}
	transport := &http.Transport{Proxy: http.ProxyURL(proxyURL)}
	t.Cleanup(transport.CloseIdleConnections)
	client := &http.Client{Transport: transport, Timeout: 3 * time.Second}
	response, err := client.Get(upstream.URL + "/shared-fixture")
	if err != nil {
		t.Fatal(err)
	}
	body, readErr := io.ReadAll(response.Body)
	_ = response.Body.Close()
	if readErr != nil || response.StatusCode != http.StatusOK || string(body) != marker {
		t.Fatalf("proxy response: code=%d body=%q err=%v", response.StatusCode, body, readErr)
	}
	waitSharedProxyCount(t, parent, 1)
	if parent.DB() != child.DB() {
		t.Fatal("child created an independent index instead of sharing the recorder")
	}
	if _, err := os.Stat(filepath.Join(runtimeDir, "_index")); !os.IsNotExist(err) {
		t.Fatalf("child must not create a separate evidence index: %v", err)
	}
	rows, total, err := parent.Page(PageQuery{Path: "/shared-fixture", RespMin: -1, RespMax: -1}, 0, 10)
	if err != nil || total != 1 || len(rows) != 1 {
		t.Fatalf("parent query missed child exchange: total=%d rows=%d err=%v", total, len(rows), err)
	}
	id := rows[0].ID
	requestText, responseText, err := parent.Get(id)
	if err != nil || !strings.Contains(requestText, "GET /shared-fixture") || !strings.Contains(responseText, marker) {
		t.Fatalf("parent Get missed child wire data: request=%q response=%q err=%v", requestText, responseText, err)
	}
	if strings.Contains(requestText, "fixture-password") || strings.Contains(requestText, "Proxy-Authorization") {
		t.Fatal("proxy authentication was written into captured target headers")
	}
	if parent.fts {
		hits, err := parent.query("", "", marker, 0, 10)
		if err != nil || len(hits) != 1 || hits[0].ID != id {
			t.Fatalf("parent full-text query missed child response: hits=%v err=%v", hits, err)
		}
	}
	var snapshots int
	if err := parent.ReadEvidence(context.Background(), []string{id}, func(exchange EvidenceExchange) error {
		snapshots++
		captured, err := io.ReadAll(exchange.Response)
		if err != nil {
			return err
		}
		if string(captured) != marker || exchange.ID != id {
			t.Errorf("evidence reader missed child response: id=%q body=%q", exchange.ID, captured)
		}
		return nil
	}); err != nil || snapshots != 1 {
		t.Fatalf("parent evidence read: snapshots=%d err=%v", snapshots, err)
	}

	// Closing one task listener must leave old evidence and future parent writes
	// usable. The shared database belongs to the parent, not to child.Close.
	if err := child.Close(); err != nil {
		t.Fatal(err)
	}
	if _, _, err := parent.Get(id); err != nil {
		t.Fatalf("child Close removed parent evidence: %v", err)
	}
	parent.record(newFlow("fixture.example.test", "GET", "/after-child-close", nil, []byte("parent remains writable")))
	waitSharedProxyCount(t, parent, 2)
	rows, total, err = parent.Page(PageQuery{Path: "/after-child-close", RespMin: -1, RespMax: -1}, 0, 10)
	if err != nil || total != 1 || len(rows) != 1 {
		t.Fatalf("parent write after child Close: total=%d rows=%d err=%v", total, len(rows), err)
	}
}

func TestSharedProxyHasPrivateCAAndUpstream(t *testing.T) {
	parent, _ := openTraffic(t)
	if err := parent.SetUpstreamProxy("http://127.0.0.1:19001"); err != nil {
		t.Fatal(err)
	}
	child, err := parent.OpenProxy(t.TempDir(), "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = child.Close() })
	other, err := parent.OpenProxy(t.TempDir(), "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = other.Close() })
	paths := []string{parent.CACertPath(), child.CACertPath(), other.CACertPath()}
	certificates := map[[sha256.Size]byte]bool{}
	for i, path := range paths {
		for _, previous := range paths[:i] {
			if path == previous {
				t.Fatal("task listeners share a CA path")
			}
		}
		pem, err := os.ReadFile(path)
		if err != nil || len(pem) == 0 {
			t.Fatalf("missing CA %q: %v", path, err)
		}
		hash := sha256.Sum256(pem)
		if certificates[hash] {
			t.Fatal("task listeners reused another listener's root CA")
		}
		certificates[hash] = true
	}
	if parent.auth != nil && child.auth != parent.auth {
		t.Fatal("child proxy did not inherit the recorder authentication")
	}
	if err := child.SetUpstreamProxy("socks5://127.0.0.1:19002"); err != nil {
		t.Fatal(err)
	}
	if parent.UpstreamProxy() != "http://127.0.0.1:19001" || child.UpstreamProxy() != "socks5://127.0.0.1:19002" || other.UpstreamProxy() != "" {
		t.Fatal("changing one task upstream changed the global or another task route")
	}
	if err := child.SetUpstreamProxy(""); err != nil || parent.UpstreamProxy() != "http://127.0.0.1:19001" {
		t.Fatalf("clearing task upstream changed global route: %v", err)
	}
}

func TestSharedProxyRejectsUnavailableRecorder(t *testing.T) {
	var missing *Traffic
	for name, recorder := range map[string]*Traffic{"nil": missing, "uninitialized": {}} {
		t.Run(name, func(t *testing.T) {
			if child, err := recorder.OpenProxy(t.TempDir(), "127.0.0.1:0"); err == nil {
				if child != nil {
					_ = child.Close()
				}
				t.Fatal("unavailable parent recorder accepted a task listener")
			}
		})
	}
	parent, _ := openTraffic(t)
	if err := parent.Close(); err != nil {
		t.Fatal(err)
	}
	if child, err := parent.OpenProxy(t.TempDir(), "127.0.0.1:0"); err == nil {
		if child != nil {
			_ = child.Close()
		}
		t.Fatal("closed recorder accepted a new task listener")
	}
}
