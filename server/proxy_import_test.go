package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

func TestParseProxyBatchWebsiteFormatsAndProtocolGroups(t *testing.T) {
	text := "\ufeff192.0.2.1:8080\r\n" +
		"还有socks5198.51.100.1:4145 #us\r\n198.51.100.2:4145 #us\n" +
		"socks5://198.51.100.1:4145 #us\nshsocks5\n[2001:db8::1]:1080 #ru\n" +
		"https\nproxy.invalid:8443 #label\nhttp://other.invalid:8080|My proxy|note #literal"
	entries, issues := ParseProxyBatch(text)
	expected := []string{"http://192.0.2.1:8080", "socks5://198.51.100.1:4145", "socks5://198.51.100.2:4145", "socks5://[2001:db8::1]:1080", "https://proxy.invalid:8443", "http://other.invalid:8080"}
	if len(entries) != len(expected) || len(issues) != 1 || issues[0].Kind != "duplicate" || issues[0].Line != 4 {
		t.Fatalf("entries=%d issues=%+v", len(entries), issues)
	}
	for i, value := range expected {
		if entries[i].URL != value || entries[i].Health != "unknown" {
			t.Fatalf("entry %d: %q", i, entries[i].URL)
		}
	}
	if entries[1].Remark != "us" || entries[3].Remark != "ru" || entries[5].Name != "My proxy" || entries[5].Remark != "note #literal" {
		t.Fatal("country labels or existing metadata were lost")
	}
}

func TestParseProxyBatchCredentialsCommentsAndMalformedSecrets(t *testing.T) {
	authenticated := "http://" + "user:" + "p%23word%40value" + "@" + "proxy.invalid:8080"
	text := authenticated + " #us|name|note\n" + authenticated + "\n" +
		"http://" + "PRIVATE_USER:PRIVATE_PASSWORD" + "@" + "[bad\n" +
		"ftp://" + "PRIVATE_FTP:PRIVATE_SECRET" + "@" + "host:1"
	entries, issues := ParseProxyBatch(text)
	if len(entries) != 1 || len(issues) != 3 || entries[0].Name != "name" || entries[0].Remark != "note；us" {
		t.Fatalf("entry count=%d issue count=%d", len(entries), len(issues))
	}
	u, err := url.Parse(entries[0].URL)
	if err != nil {
		t.Fatal(err)
	}
	password, _ := u.User.Password()
	if password != "p#word@value" {
		t.Fatal("credential escape changed")
	}
	encoded, _ := json.Marshal(issues)
	for _, secret := range []string{"PRIVATE_USER", "PRIVATE_PASSWORD", "PRIVATE_FTP", "PRIVATE_SECRET", "p%23word", "p#word"} {
		if strings.Contains(string(encoded), secret) {
			t.Fatal("an import issue echoed credentials")
		}
	}
}

func TestParseProxyBatchDoesNotGuessProtocolFromPortOrHostnamePrefix(t *testing.T) {
	entries, issues := ParseProxyBatch("192.0.2.1:4145 socks5.example:1080\nsocks5h://198.51.100.1:1080\n198.51.100.2:80\nhttp\n192.0.2.2:10808")
	want := []string{"http://192.0.2.1:4145", "http://socks5.example:1080", "socks5://198.51.100.1:1080", "socks5://198.51.100.2:80", "http://192.0.2.2:10808"}
	if len(entries) != len(want) || len(issues) != 0 {
		t.Fatalf("entries=%d issues=%d", len(entries), len(issues))
	}
	for i, value := range want {
		if entries[i].URL != value {
			t.Fatalf("entry %d: %s", i, entries[i].URL)
		}
	}
}

func TestParseProxyBatchLargeAuthenticatedWhitespaceBlockKeepsDistinctIdentities(t *testing.T) {
	var text strings.Builder
	for i := range 4500 {
		fmt.Fprintf(&text, "http://fixture-user-%d:fixture-password@proxy-%d.invalid:3129 ", i, i%4244)
	}
	entries, issues := ParseProxyBatch(text.String())
	if len(entries) != 4500 || len(issues) != 0 {
		t.Fatalf("entries=%d issues=%d", len(entries), len(issues))
	}
	ids := map[string]bool{}
	for _, entry := range entries {
		if ids[entry.ID] || !strings.HasSuffix(entry.ID, "-1") {
			t.Fatal("same-line IDs collided or lost their source line")
		}
		ids[entry.ID] = true
	}
}

func TestProxyPoolImportCountsAndNoopPreserveRouteAndHealthQueue(t *testing.T) {
	for _, mode := range []string{"duplicate", "invalid", "partial"} {
		t.Run(mode, func(t *testing.T) {
			entry := proxyScaleEntries(1)[0]
			m := &Manager{globalProxy: entry.RawURL, proxyPool: &proxyPoolState{entries: []proxyPoolEntry{entry}, active: entry.ID, generation: 7}}
			s := &Server{m: m, proxyPoolWake: make(chan struct{}, 1)}
			text := entry.RawURL + " #tag\n" + entry.RawURL
			added, duplicate, invalid := 0, 2, 0
			if mode == "invalid" {
				text, duplicate, invalid = "INVALID_SECRET_WITHOUT_ENDPOINT", 0, 1
			} else if mode == "partial" {
				text, added, duplicate, invalid = entry.RawURL+"\nsocks5198.51.100.10:4145 #us\nINVALID_SECRET_WITHOUT_ENDPOINT", 1, 1, 1
			}
			body, _ := json.Marshal(map[string]string{"text": text})
			w := httptest.NewRecorder()
			s.importProxyPool(w, httptest.NewRequest(http.MethodPost, "/api/settings/proxy-pool/import?summary=1", strings.NewReader(string(body))))
			var response struct {
				OK          bool `json:"ok"`
				Added       int  `json:"imported"`
				Duplicates  int  `json:"skipped_duplicates"`
				Invalid     int  `json:"invalid"`
				ProbeQueued bool `json:"probe_queued"`
			}
			if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil {
				t.Fatal(err)
			}
			if w.Code != http.StatusOK || !response.OK || response.Added != added || response.Duplicates != duplicate || response.Invalid != invalid || response.ProbeQueued != (added > 0) {
				t.Fatalf("bad import counts: %s", w.Body.String())
			}
			if m.GlobalProxy() != entry.RawURL || m.proxyPool.active != entry.ID || len(m.proxyPool.entries) != 1+added || len(s.proxyPoolWake) != added {
				t.Fatal("import changed the route or queued unexpected health work")
			}
			if m.proxyPool.generation != uint64(7+added) {
				t.Fatal("no-op import invalidated an existing health round")
			}
			if strings.Contains(w.Body.String(), "fixture-secret") || strings.Contains(w.Body.String(), "fixture-user") || strings.Contains(w.Body.String(), "INVALID_SECRET") {
				t.Fatal("import acknowledgement leaked credentials or malformed input")
			}
		})
	}
}
