package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"
)

func callProxyBulk(t *testing.T, s *Server, request proxyPoolBulkRequest) (*httptest.ResponseRecorder, map[string]any) {
	t.Helper()
	body, err := json.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	w := httptest.NewRecorder()
	s.bulkProxyPool(w, httptest.NewRequest(http.MethodPost, "/api/settings/proxy-pool/bulk", strings.NewReader(string(body))))
	var response map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	return w, response
}

func TestProxyPoolBulkLargeDeleteAndDisable(t *testing.T) {
	for _, action := range []string{"delete", "disable"} {
		t.Run(action, func(t *testing.T) {
			entries := proxyScaleEntries(10001)
			m := &Manager{globalProxy: entries[0].RawURL, proxyPool: &proxyPoolState{entries: entries, active: entries[0].ID}}
			s := &Server{m: m}
			ids := make([]string, 0, len(entries))
			for _, entry := range entries[1:] {
				ids = append(ids, entry.ID)
			}
			ids = append(ids, ids[0]) // Duplicate selection cannot inflate changes.
			w, response := callProxyBulk(t, s, proxyPoolBulkRequest{Action: action, IDs: ids})
			if w.Code != http.StatusOK || response["changed"] != float64(10000) || response["skipped"] != float64(0) || response["active_changed"] != false {
				t.Fatalf("unexpected response: status=%d %s", w.Code, w.Body.String())
			}
			if w.Body.Len() > 400 || strings.Contains(w.Body.String(), "fixture-secret") || strings.Contains(w.Body.String(), "entries") {
				t.Fatal("batch response is unbounded or contains stored credentials")
			}
			remaining := m.proxyPoolEntries()
			if action == "delete" && len(remaining) != 1 {
				t.Fatalf("remaining=%d", len(remaining))
			}
			if action == "disable" {
				if len(remaining) != 10001 {
					t.Fatal("disable removed records")
				}
				for _, entry := range remaining[1:] {
					if entry.Enabled || entry.Health != "disabled" {
						t.Fatal("selected member remained enabled")
					}
				}
			}
			if m.GlobalProxy() != entries[0].RawURL || m.proxyPool.active != entries[0].ID || m.proxyPool.generation != 1 {
				t.Fatal("inactive batch changed the active route or failed to supersede old probes")
			}
		})
	}
}

func TestProxyPoolBulkCleanupRechecksHealthAndProtectsCurrent(t *testing.T) {
	entries := proxyScaleEntries(8)
	for i := range entries {
		entries[i].Health, entries[i].ConsecutiveFailures, entries[i].LastError, entries[i].LastCheckedAt = "unhealthy-cooling", 3, "fixture failed", time.Now()
	}
	entries[2].Health, entries[2].LastCheckedAt = "unknown", time.Time{}
	entries[3].Health = "healthy" // Re-enabled/recovered member with old metadata.
	entries[4].ConsecutiveFailures = 2
	entries[5].Enabled, entries[5].Health = false, "disabled"
	entries[6].LastCheckedAt = time.Time{}
	entries[7].ConsecutiveFailures, entries[7].LastError = 0, ""
	m := &Manager{globalProxy: entries[0].RawURL, proxyPool: &proxyPoolState{entries: entries, active: entries[0].ID}}
	ids := []string{"missing"}
	for _, entry := range entries {
		ids = append(ids, entry.ID)
	}
	w, response := callProxyBulk(t, &Server{m: m}, proxyPoolBulkRequest{Action: "delete", IDs: ids, OnlyUnhealthy: true, AllowActive: true, ExpectedActiveID: entries[0].ID})
	if w.Code != http.StatusOK || response["changed"] != float64(2) || response["skipped"] != float64(7) {
		t.Fatalf("unexpected cleanup result: %s", w.Body.String())
	}
	for _, entry := range m.proxyPoolEntries() {
		if entry.ID == entries[1].ID || entry.ID == entries[5].ID {
			t.Fatal("confirmed inactive failures were not removed")
		}
	}
	if len(m.proxyPoolEntries()) != 6 || m.GlobalProxy() != entries[0].RawURL || m.proxyPool.active != entries[0].ID {
		t.Fatal("cleanup removed an unchecked/current/recovered member or changed the route")
	}
	// The same stale cleanup snapshot is harmless and makes no new generation.
	w, response = callProxyBulk(t, &Server{m: m}, proxyPoolBulkRequest{Action: "delete", IDs: ids, OnlyUnhealthy: true})
	if w.Code != http.StatusOK || response["changed"] != float64(0) || m.proxyPool.generation != 1 {
		t.Fatal("no-op cleanup changed the pool")
	}
}

func TestProxyPoolBulkActiveConsentAndFallback(t *testing.T) {
	for _, action := range []string{"delete", "disable"} {
		t.Run(action, func(t *testing.T) {
			entries := proxyScaleEntries(2)
			m := &Manager{globalProxy: entries[0].RawURL, proxyPool: &proxyPoolState{entries: entries, active: entries[0].ID}}
			s := &Server{m: m}
			request := proxyPoolBulkRequest{Action: action, IDs: []string{entries[0].ID}, AllowActive: true, ExpectedActiveID: "stale-active"}
			w, _ := callProxyBulk(t, s, request)
			if w.Code != http.StatusConflict || m.proxyPool.generation != 0 || !reflect.DeepEqual(entries, m.proxyPoolEntries()) {
				t.Fatal("a stale confirmation changed the active route")
			}
			request.ExpectedActiveID = entries[0].ID
			w, response := callProxyBulk(t, s, request)
			if w.Code != http.StatusOK || response["active_changed"] != true || m.GlobalProxy() != entries[1].RawURL || m.proxyPool.active != entries[1].ID {
				t.Fatalf("explicit current-member mutation did not move route: %s", w.Body.String())
			}
			request.IDs, request.ExpectedActiveID = []string{entries[1].ID}, entries[1].ID
			w, _ = callProxyBulk(t, s, request)
			if w.Code != http.StatusOK || m.GlobalProxy() != "" || m.proxyPool.active != "" {
				t.Fatal("last usable member remained the runtime route")
			}
		})
	}
}

func TestProxyPoolBulkCleanupUsesSavedThresholdAndRejectsInvalidSetting(t *testing.T) {
	m := proxyTestManager(t)
	entries := proxyScaleEntries(2)
	entries[1].Health, entries[1].ConsecutiveFailures, entries[1].LastCheckedAt, entries[1].LastError = "unhealthy-cooling", 3, time.Now(), "fixture failure"
	m.proxyPool, m.globalProxy = &proxyPoolState{entries: entries, active: entries[0].ID}, entries[0].RawURL
	s := &Server{m: m}
	request := proxyPoolBulkRequest{Action: "delete", IDs: []string{entries[1].ID}, OnlyUnhealthy: true}
	for _, tc := range []struct {
		threshold string
		status    int
		changed   float64
	}{
		{"5", http.StatusOK, 0},
		{"invalid", http.StatusConflict, 0},
		{"3", http.StatusOK, 1},
	} {
		if _, err := m.pg.Exec(`INSERT INTO settings(key,value) VALUES($1,$2) ON CONFLICT(key) DO UPDATE SET value=EXCLUDED.value`, settingProxyHealthThreshold, tc.threshold); err != nil {
			t.Fatal(err)
		}
		w, response := callProxyBulk(t, s, request)
		if w.Code != tc.status || (w.Code == http.StatusOK && response["changed"] != tc.changed) {
			t.Fatalf("threshold=%q status=%d response=%s", tc.threshold, w.Code, w.Body.String())
		}
		if tc.changed == 0 && !reflect.DeepEqual(entries, m.proxyPoolEntries()) {
			t.Fatal("unknown or higher configured threshold deleted a member")
		}
	}
}

func TestProxyPoolBulkRejectsMalformedRequests(t *testing.T) {
	for _, body := range []string{`{}`, `{"action":"delete","ids":[]}`, `{"action":"enable","ids":["a"]}`, `{"action":"disable","ids":["a"],"only_unhealthy":true}`, `{"action":"delete","ids":[""]}`, `{"action":"delete","ids":["a"],"unknown":true}`, `{"action":"delete","ids":["a"]} {}`} {
		s := &Server{m: &Manager{proxyPool: &proxyPoolState{entries: proxyScaleEntries(2)}}}
		w := httptest.NewRecorder()
		s.bulkProxyPool(w, httptest.NewRequest(http.MethodPost, "/api/settings/proxy-pool/bulk", strings.NewReader(body)))
		if w.Code != http.StatusBadRequest || len(s.m.proxyPoolEntries()) != 2 || s.m.proxyPool.generation != 0 {
			t.Fatalf("accepted malformed request: %s", body)
		}
	}
}

func TestProxyPoolBulkOneCommitAndAtomicRejection(t *testing.T) {
	m := proxyTestManager(t)
	entries := proxyScaleEntries(1000)
	m.proxyPool, m.globalProxy = &proxyPoolState{entries: entries, active: entries[0].ID}, entries[0].RawURL
	if err := m.persistProxyPoolLocked(); err != nil {
		t.Fatal(err)
	}
	name := fmt.Sprintf("artex_proxy_bulk_%d", time.Now().UnixNano())
	if _, err := m.pg.Exec(`CREATE TABLE ` + name + ` (n integer NOT NULL); INSERT INTO ` + name + ` VALUES(0)`); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = m.pg.Exec(`DROP TABLE IF EXISTS ` + name) })
	if _, err := m.pg.Exec(`CREATE FUNCTION ` + name + `() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN IF NEW.key='proxy_pool_entries' THEN UPDATE ` + name + ` SET n=n+1; END IF; RETURN NEW; END $$`); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = m.pg.Exec(`DROP FUNCTION IF EXISTS ` + name + `()`) })
	if _, err := m.pg.Exec(`CREATE TRIGGER ` + name + ` BEFORE INSERT OR UPDATE ON settings FOR EACH ROW EXECUTE FUNCTION ` + name + `()`); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = m.pg.Exec(`DROP TRIGGER IF EXISTS ` + name + ` ON settings`) })
	ids := make([]string, 0, 500)
	for _, entry := range entries[1:501] {
		ids = append(ids, entry.ID)
	}
	s := &Server{m: m}
	w, _ := callProxyBulk(t, s, proxyPoolBulkRequest{Action: "delete", IDs: ids})
	var writes int
	if err := m.pg.QueryRow(`SELECT n FROM ` + name).Scan(&writes); err != nil || writes != 1 || w.Code != http.StatusOK {
		t.Fatalf("writes=%d status=%d err=%v", writes, w.Code, err)
	}
	before := m.proxyPoolEntries()
	stored, _, err := m.pg.GetSetting(settingProxyPool)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.pg.Exec(`CREATE OR REPLACE FUNCTION ` + name + `() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN IF NEW.key='proxy_pool_entries' THEN RAISE EXCEPTION 'fixture rejected batch'; END IF; RETURN NEW; END $$`); err != nil {
		t.Fatal(err)
	}
	w, _ = callProxyBulk(t, s, proxyPoolBulkRequest{Action: "disable", IDs: []string{entries[0].ID, entries[999].ID}, AllowActive: true, ExpectedActiveID: entries[0].ID})
	after, _, err := m.pg.GetSetting(settingProxyPool)
	if err != nil || w.Code != http.StatusInternalServerError || stored != after || !reflect.DeepEqual(before, m.proxyPoolEntries()) || m.proxyPool.active != entries[0].ID || m.GlobalProxy() != entries[0].RawURL || m.proxyPool.generation != 1 {
		t.Fatal("a rejected batch changed memory, durable state, active route or generation")
	}
}
