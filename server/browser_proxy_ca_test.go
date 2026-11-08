package server

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestExecutionBrowserTrustPinsOnlyRecordingCA(t *testing.T) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	ca := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "fixture recording CA"}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign}
	der, err := x509.CreateCertificate(rand.Reader, ca, ca, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	certPath := filepath.Join(t.TempDir(), "ca.pem")
	if err := os.WriteFile(certPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0600); err != nil {
		t.Fatal(err)
	}
	parsed, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	hash := sha256.Sum256(parsed.RawSubjectPublicKeyInfo)
	want := "--ignore-certificate-errors-spki-list=" + base64.StdEncoding.EncodeToString(hash[:])
	path := filepath.Join(t.TempDir(), "browser.json")
	if err := writeBrowserMCPProxyConfig(path, "http://127.0.0.1:8788", "fixture", "secret", certPath); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var cfg map[string]any
	if err := json.Unmarshal(raw, &cfg); err != nil {
		t.Fatal(err)
	}
	launch := cfg["browser"].(map[string]any)["launchOptions"].(map[string]any)
	args := launch["args"].([]any)
	if len(args) != 1 || args[0] != want {
		t.Fatal("incorrect scoped CA trust", args)
	}
	for _, forbidden := range []string{"ignoreHTTPSErrors", "--ignore-certificate-errors\"", "--no-sandbox"} {
		if strings.Contains(string(raw), forbidden) {
			t.Fatal("blanket browser security override", forbidden)
		}
	}
	if _, err := browserProxyCAPin(filepath.Join(t.TempDir(), "missing")); err == nil {
		t.Fatal("missing CA trusted")
	}
}
