package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func proxyScaleEntries(n int) []proxyPoolEntry {
	entries := make([]proxyPoolEntry, n)
	for i := range entries {
		entries[i] = proxyPoolEntry{
			ProxyPoolEntry: ProxyPoolEntry{ID: fmt.Sprintf("scale-%d", i), Enabled: true, Health: "unknown"},
			RawURL:         fmt.Sprintf("http://fixture-user:fixture-secret@proxy-%d.invalid:8080", i),
		}
	}
	return entries
}

func TestProxyPoolLargeImportReturnsBeforeNetworkProbe(t *testing.T) {
	m := &Manager{proxyPool: &proxyPoolState{}}
	s := &Server{m: m, proxyPoolWake: make(chan struct{}, 1)}
	s.proxyProbe = func(context.Context, string, string, []string, time.Duration) egressProbeResult {
		t.Error("import request must not perform a network probe")
		return egressProbeResult{Error: "fixture failure"}
	}
	var text strings.Builder
	for _, entry := range proxyScaleEntries(1000) {
		text.WriteString(entry.RawURL + "\n")
	}
	body, err := json.Marshal(map[string]string{"text": text.String()})
	if err != nil {
		t.Fatal(err)
	}
	w := httptest.NewRecorder()
	s.importProxyPool(w, httptest.NewRequest(http.MethodPost, "/api/settings/proxy-pool/import?summary=1", strings.NewReader(string(body))))
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d", w.Code)
	}
	var payload map[string]json.RawMessage
	if err := json.Unmarshal(w.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	if string(payload["count"]) != "1000" || string(payload["probe_queued"]) != "true" || payload["entries"] != nil {
		t.Fatalf("unexpected summary fields: %v", reflect.ValueOf(payload).MapKeys())
	}
	if string(payload["errors"]) != "[]" {
		t.Fatal("successful import errors must be a JSON array")
	}
	if strings.Contains(w.Body.String(), "fixture-secret") || strings.Contains(w.Body.String(), "fixture-user") {
		t.Fatal("summary response leaked credentials")
	}
	if len(s.proxyPoolWake) != 1 || len(m.proxyPool.entries) != 1000 || m.proxyPool.active == "" {
		t.Fatal("import did not commit and queue one background round")
	}
}

func TestProxyPoolTestAllIsQueuedAndCoalesced(t *testing.T) {
	m := &Manager{proxyPool: &proxyPoolState{entries: proxyScaleEntries(1000)}}
	s := &Server{m: m, proxyPoolWake: make(chan struct{}, 1)}
	s.proxyProbe = func(context.Context, string, string, []string, time.Duration) egressProbeResult {
		t.Error("test-all request must not perform a network probe")
		return egressProbeResult{}
	}
	for range 3 {
		w := httptest.NewRecorder()
		s.testAllProxyPool(w, httptest.NewRequest(http.MethodPost, "/api/settings/proxy-pool/test-all?summary=1", nil))
		if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"probe_queued":true`) || strings.Contains(w.Body.String(), `"entries"`) {
			t.Fatalf("test-all did not return a bounded queued response: status=%d", w.Code)
		}
	}
	if len(s.proxyPoolWake) != 1 || !m.proxyPool.forceHealth {
		t.Fatal("repeated test-all requests were not coalesced")
	}
	s.proxyReconnect.closed = true
	w := httptest.NewRecorder()
	s.testAllProxyPool(w, httptest.NewRequest(http.MethodPost, "/api/settings/proxy-pool/test-all", nil))
	if w.Code != http.StatusServiceUnavailable {
		t.Fatal("closed server accepted a new health round")
	}
}

func TestProxyPoolRoundUsesBoundedWorkersAndPublishesAfterCompletion(t *testing.T) {
	entries := proxyScaleEntries(1000)
	// The active member is deliberately last in storage; it must be in the
	// first bounded group, not wait behind a thousand unhealthy candidates.
	activeEntry := entries[len(entries)-1]
	m := &Manager{globalProxy: activeEntry.RawURL, proxyPool: &proxyPoolState{active: activeEntry.ID, entries: entries}}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s := &Server{m: m, proxyCtx: ctx, proxyPoolProbeSem: make(chan struct{}, proxyPoolMaxProbeConcurrency)}
	gate, started := make(chan struct{}), make(chan struct{}, proxyPoolMaxProbeConcurrency)
	var active, peak, calls atomic.Int32
	var activeStarted atomic.Bool
	s.proxyProbe = func(ctx context.Context, raw string, _ string, _ []string, _ time.Duration) egressProbeResult {
		if raw == activeEntry.RawURL {
			activeStarted.Store(true)
		}
		n := active.Add(1)
		defer active.Add(-1)
		for old := peak.Load(); n > old && !peak.CompareAndSwap(old, n); old = peak.Load() {
		}
		if calls.Add(1) <= proxyPoolMaxProbeConcurrency {
			started <- struct{}{}
		}
		select {
		case <-gate:
		case <-ctx.Done():
		}
		return egressProbeResult{OK: true, ExitIP: "203.0.113.5", LatencyMS: 1}
	}
	baseline := runtime.NumGoroutine()
	done := make(chan proxyPoolTickOutcome, 1)
	go func() { done <- s.proxyPoolTick() }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("health workers did not stop")
		}
	})
	for range proxyPoolMaxProbeConcurrency {
		select {
		case <-started:
		case <-time.After(5 * time.Second):
			t.Fatal("workers did not start")
		}
	}
	if extra := runtime.NumGoroutine() - baseline; extra > 50 {
		t.Fatalf("created goroutines proportional to pool size: extra=%d", extra)
	}
	if !activeStarted.Load() {
		t.Fatal("active route was not prioritized in the first worker group")
	}
	snapshot, _, _ := m.ProxyPoolSnapshot()
	for _, entry := range snapshot {
		if entry.Health != "unknown" || !entry.LastCheckedAt.IsZero() {
			t.Fatal("partial round was published before every probe completed")
		}
	}
	close(gate)
	select {
	case outcome := <-done:
		if !outcome.confirmed {
			t.Fatal("successful active-member probe was not confirmed")
		}
		// Leave a completion for the cleanup join.
		done <- outcome
	case <-time.After(5 * time.Second):
		t.Fatal("health round did not finish")
	}
	if calls.Load() != 1000 || peak.Load() > proxyPoolMaxProbeConcurrency {
		t.Fatalf("calls=%d peak=%d", calls.Load(), peak.Load())
	}
	snapshot, _, _ = m.ProxyPoolSnapshot()
	for _, entry := range snapshot {
		if entry.Health != "healthy" || entry.LastCheckedAt.IsZero() || strings.Contains(entry.URL, "fixture-secret") {
			t.Fatal("complete round was not published safely")
		}
	}
}

func TestProxyPoolCanceledOrSupersededRoundDoesNotPublish(t *testing.T) {
	for _, mode := range []string{"cancel", "edit"} {
		t.Run(mode, func(t *testing.T) {
			entries := proxyScaleEntries(20)
			m := &Manager{globalProxy: entries[0].RawURL, proxyPool: &proxyPoolState{active: entries[0].ID, entries: entries}}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			s := &Server{m: m, proxyCtx: ctx}
			gate, started := make(chan struct{}), make(chan struct{})
			var once sync.Once
			var calls atomic.Int32
			s.proxyProbe = func(ctx context.Context, _ string, _ string, _ []string, _ time.Duration) egressProbeResult {
				calls.Add(1)
				once.Do(func() { close(started) })
				select {
				case <-gate:
				case <-ctx.Done():
				}
				return egressProbeResult{Error: "fixture proxy down"}
			}
			done := make(chan struct{})
			go func() { defer close(done); s.proxyPoolTick() }()
			t.Cleanup(func() { cancel(); <-done })
			select {
			case <-started:
			case <-time.After(5 * time.Second):
				t.Fatal("probe did not start")
			}
			if mode == "cancel" {
				cancel()
			} else {
				r := httptest.NewRequest(http.MethodPut, "/api/settings/proxy-pool/scale-0", strings.NewReader(`{"url":"http://new-fixture.invalid:8080"}`))
				r.SetPathValue("id", "scale-0")
				w := httptest.NewRecorder()
				s.updateProxyPool(w, r)
				if w.Code != http.StatusOK {
					t.Fatalf("edit status=%d", w.Code)
				}
			}
			close(gate)
			select {
			case <-done:
			case <-time.After(5 * time.Second):
				t.Fatal("round did not finish after cancel/edit")
			}
			for _, entry := range m.proxyPoolEntries() {
				if entry.ConsecutiveFailures != 0 || !entry.LastCheckedAt.IsZero() {
					t.Fatal("canceled/old endpoint result changed health")
				}
			}
			if calls.Load() > proxyPoolMaxProbeConcurrency {
				t.Fatalf("obsolete round drained all old jobs: calls=%d", calls.Load())
			}
		})
	}
}

func TestProxyPoolSingleProbeDoesNotWaitBehindBatch(t *testing.T) {
	m := &Manager{proxyPool: &proxyPoolState{entries: proxyScaleEntries(1000)}}
	s := &Server{m: m}
	s.proxyPoolTickMu.Lock()
	defer s.proxyPoolTickMu.Unlock()
	r := httptest.NewRequest(http.MethodPost, "/api/settings/proxy-pool/scale-0/test", nil)
	r.SetPathValue("id", "scale-0")
	w := httptest.NewRecorder()
	s.testProxyPoolEntry(w, r)
	if w.Code != http.StatusConflict {
		t.Fatalf("single probe queued behind running batch: status=%d", w.Code)
	}
}

func TestProxyPoolImportCASDoesNotReplaceConcurrentEdit(t *testing.T) {
	entries := proxyScaleEntries(2)
	m := &Manager{globalProxy: entries[0].RawURL, proxyPool: &proxyPoolState{active: entries[0].ID, entries: entries}}
	snapshot := m.proxyPoolEntries()
	generation := m.proxyPool.generation
	s := &Server{m: m}
	r := httptest.NewRequest(http.MethodPut, "/api/settings/proxy-pool/scale-1", strings.NewReader(`{"remark":"keep this edit"}`))
	r.SetPathValue("id", "scale-1")
	w := httptest.NewRecorder()
	s.updateProxyPool(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("edit status=%d", w.Code)
	}
	snapshot = append(snapshot, proxyScaleEntries(3)[2])
	if err := m.setProxyPoolEntries(snapshot, entries[0].ID, generation); !errors.Is(err, errProxyPoolChanged) {
		t.Fatalf("stale import snapshot accepted: %v", err)
	}
	current := m.proxyPoolEntries()
	if len(current) != 2 || current[1].Remark != "keep this edit" {
		t.Fatal("stale import replaced concurrent edit")
	}
}

func TestProxyPoolManualActivationInvalidatesPendingImportAndProbe(t *testing.T) {
	entries := proxyScaleEntries(2)
	for i := range entries {
		entries[i].Health = "healthy"
	}
	m := &Manager{globalProxy: entries[0].RawURL, proxyPool: &proxyPoolState{active: entries[0].ID, entries: entries}}
	snapshot := m.proxyPoolEntries()
	generation := m.proxyPool.generation
	if err := m.activateProxyID(entries[1].ID, "manual"); err != nil {
		t.Fatal(err)
	}
	if m.proxyPool.generation != generation+1 || m.proxyPool.active != entries[1].ID {
		t.Fatal("manual activation did not invalidate the old configuration")
	}
	if err := m.setProxyPoolEntries(snapshot, entries[0].ID, generation); !errors.Is(err, errProxyPoolChanged) {
		t.Fatal("pending import overwrote a newer manual activation")
	}
	if err := m.activateProxyID(entries[1].ID, "manual"); err != nil || m.proxyPool.generation != generation+1 {
		t.Fatal("unchanged manual activation advanced configuration generation")
	}
}

func TestProxyPoolReconcileRejectsOldConfigurationGeneration(t *testing.T) {
	entries := proxyScaleEntries(2)
	entries[0].Health = "unhealthy-cooling"
	entries[1].Health = "healthy"
	m := &Manager{globalProxy: entries[0].RawURL, proxyPool: &proxyPoolState{active: entries[0].ID, entries: entries, generation: 3}}
	s := &Server{m: m}
	if s.reconcileProxyPoolAfterProbe(true, 2) || m.proxyPool.active != entries[0].ID || m.GlobalProxy() != entries[0].RawURL {
		t.Fatal("old health generation changed current route")
	}
	if !s.reconcileProxyPoolAfterProbe(false, 3) || m.proxyPool.active != entries[1].ID {
		t.Fatal("matching health generation did not preserve normal reconciliation")
	}
}

func TestProxyPoolSingleProbeCanceledOrEditedDoesNotPublish(t *testing.T) {
	for _, mode := range []string{"cancel", "edit"} {
		t.Run(mode, func(t *testing.T) {
			entries := proxyScaleEntries(2)
			m := &Manager{globalProxy: entries[0].RawURL, proxyPool: &proxyPoolState{active: entries[0].ID, entries: entries}}
			s := &Server{m: m}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			gate, started := make(chan struct{}), make(chan struct{})
			s.proxyProbe = func(ctx context.Context, _ string, _ string, _ []string, _ time.Duration) egressProbeResult {
				close(started)
				select {
				case <-gate:
				case <-ctx.Done():
				}
				return egressProbeResult{OK: true}
			}
			r := httptest.NewRequest(http.MethodPost, "/api/settings/proxy-pool/scale-0/test", nil).WithContext(ctx)
			r.SetPathValue("id", "scale-0")
			w := httptest.NewRecorder()
			done := make(chan struct{})
			go func() { defer close(done); s.testProxyPoolEntry(w, r) }()
			t.Cleanup(func() { cancel(); <-done })
			select {
			case <-started:
			case <-time.After(5 * time.Second):
				t.Fatal("single probe did not start")
			}
			if mode == "cancel" {
				cancel()
			} else {
				edit := httptest.NewRequest(http.MethodPut, "/api/settings/proxy-pool/scale-0", strings.NewReader(`{"url":"http://new-fixture.invalid:8080"}`))
				edit.SetPathValue("id", "scale-0")
				out := httptest.NewRecorder()
				s.updateProxyPool(out, edit)
				if out.Code != http.StatusOK {
					t.Fatalf("edit status=%d", out.Code)
				}
			}
			close(gate)
			select {
			case <-done:
			case <-time.After(5 * time.Second):
				t.Fatal("single probe did not finish")
			}
			if w.Code == http.StatusOK || !m.proxyPoolEntries()[0].LastCheckedAt.IsZero() {
				t.Fatal("canceled/edited single probe published stale success")
			}
		})
	}
}

func TestProxyPoolSupersededManualRoundKeepsForceIntent(t *testing.T) {
	entries := proxyScaleEntries(8)
	m := &Manager{globalProxy: entries[0].RawURL, proxyPool: &proxyPoolState{active: entries[0].ID, entries: entries}}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s := &Server{m: m, proxyCtx: ctx, proxyPoolWake: make(chan struct{}, 1)}
	gate, started := make(chan struct{}), make(chan struct{})
	var once sync.Once
	s.proxyProbe = func(ctx context.Context, _ string, _ string, _ []string, _ time.Duration) egressProbeResult {
		once.Do(func() { close(started) })
		select {
		case <-gate:
		case <-ctx.Done():
		}
		return egressProbeResult{OK: true}
	}
	if !s.queueProxyPoolHealth(true) {
		t.Fatal("manual health round did not queue")
	}
	<-s.proxyPoolWake // The existing supervisor has consumed its wake.
	done := make(chan struct{})
	go func() { defer close(done); s.proxyPoolTick() }()
	t.Cleanup(func() { cancel(); <-done })
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("manual round did not start")
	}
	r := httptest.NewRequest(http.MethodPut, "/api/settings/proxy-pool/scale-0", strings.NewReader(`{"remark":"fixture edit"}`))
	r.SetPathValue("id", "scale-0")
	w := httptest.NewRecorder()
	s.updateProxyPool(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("edit status=%d", w.Code)
	}
	close(gate)
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("superseded round did not exit")
	}
	m.proxyPool.mu.Lock()
	force := m.proxyPool.forceHealth
	m.proxyPool.mu.Unlock()
	if !force || len(s.proxyPoolWake) != 1 {
		t.Fatal("superseded manual round lost its force intent")
	}
}

func TestProxyPoolBatchMakesOneDurableWriteAndRollsBackRejection(t *testing.T) {
	m := proxyTestManager(t)
	entries := proxyScaleEntries(1000)
	m.globalProxy = entries[0].RawURL
	m.proxyPool = &proxyPoolState{active: entries[0].ID, entries: entries}
	name := fmt.Sprintf("artex_proxy_scale_%d", time.Now().UnixNano())
	if _, err := m.pg.Exec(`CREATE TABLE ` + name + ` (n integer NOT NULL)`); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = m.pg.Exec(`DROP TABLE IF EXISTS ` + name) })
	if _, err := m.pg.Exec(`INSERT INTO ` + name + ` VALUES(0)`); err != nil {
		t.Fatal(err)
	}
	if _, err := m.pg.Exec(`CREATE FUNCTION ` + name + `() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN IF NEW.key='proxy_pool_entries' THEN UPDATE ` + name + ` SET n=n+1; END IF; RETURN NEW; END $$`); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = m.pg.Exec(`DROP FUNCTION IF EXISTS ` + name + `()`) })
	if _, err := m.pg.Exec(`CREATE TRIGGER ` + name + ` BEFORE INSERT OR UPDATE ON settings FOR EACH ROW EXECUTE FUNCTION ` + name + `()`); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = m.pg.Exec(`DROP TRIGGER IF EXISTS ` + name + ` ON settings`) })
	results := make([]proxyBatchProbe, len(entries))
	for i, entry := range entries {
		results[i] = proxyBatchProbe{entry: entry, complete: true, result: egressProbeResult{OK: true, ExitIP: "203.0.113.5"}}
	}
	m.proxyPool.mu.Lock()
	confirmed, err := m.applyProxyProbeBatchLocked(results, defaultProxyHealthThreshold)
	m.proxyPool.mu.Unlock()
	if err != nil || !confirmed {
		t.Fatalf("batch commit failed: %v", err)
	}
	var writes int
	if err := m.pg.QueryRow(`SELECT n FROM ` + name).Scan(&writes); err != nil || writes != 1 {
		t.Fatalf("pool writes=%d err=%v", writes, err)
	}
	before := m.proxyPoolEntries()
	if _, err := m.pg.Exec(`CREATE OR REPLACE FUNCTION ` + name + `() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN IF NEW.key='proxy_pool_entries' THEN RAISE EXCEPTION 'fixture reject health'; END IF; RETURN NEW; END $$`); err != nil {
		t.Fatal(err)
	}
	for i := range results {
		results[i].result = egressProbeResult{Error: "fixture failed probe"}
	}
	m.proxyPool.mu.Lock()
	confirmed, err = m.applyProxyProbeBatchLocked(results, defaultProxyHealthThreshold)
	m.proxyPool.mu.Unlock()
	if err == nil || confirmed || !reflect.DeepEqual(before, m.proxyPoolEntries()) {
		t.Fatal("rejected batch published partial health or recovery")
	}
}
