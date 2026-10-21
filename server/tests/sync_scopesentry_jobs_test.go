//go:build ignore

package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Autumn-27/artex/db"
)

func setSSJobSource(t *testing.T, s *Server, url string, headers json.RawMessage) {
	t.Helper()
	previous, err := s.findMCPByName(scopeSentryMCPName)
	if err != nil {
		t.Fatal(err)
	}
	m := &db.MCPServer{Name: scopeSentryMCPName, Transport: "http", URL: url, Enabled: true, Env: headers}
	if previous != nil {
		m.ID = previous.ID
	}
	id, err := s.m.pg.SaveMCP(m)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if previous != nil {
			_, _ = s.m.pg.SaveMCP(previous)
		} else {
			_ = s.m.pg.DeleteMCP(id)
		}
	})
}

func ssJobRun(t *testing.T, s *Server, req ssSyncReq) ssSyncResult {
	t.Helper()
	req.Background = true
	if err := req.normalize(); err != nil {
		t.Fatal(err)
	}
	result := newSSSyncResult()
	raw, _ := json.Marshal(req)
	initial, _ := json.Marshal(result)
	run, err := s.m.pg.CreateSyncRun(t.Context(), nil, raw, initial)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = s.m.pg.Exec(`DELETE FROM sync_runs WHERE id=$1`, run.ID) })
	result.RunID, result.StartedAt = run.ID, run.StartedAt
	s.ssJobMu.Lock()
	s.executeSSJob(req, result)
	return ssJobResult(t, s, run.ID)
}

func ssJobResult(t *testing.T, s *Server, id int64) ssSyncResult {
	t.Helper()
	run, err := s.m.pg.GetSyncRun(t.Context(), id)
	if err != nil || run == nil {
		t.Fatalf("run=%v err=%v", run, err)
	}
	normalizeSSHistoryResult(run)
	var result ssSyncResult
	if err := json.Unmarshal(run.Result, &result); err != nil {
		t.Fatal(err)
	}
	return result
}

func TestSSBackgroundLargeSyncAndResume(t *testing.T) {
	s := ssTestServer(t)
	name := fmt.Sprintf("job-fixture-%d", time.Now().UnixNano())
	var fail atomic.Bool
	fail.Store(true)
	var firstCalls atomic.Int32
	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var call struct {
			ID     int    `json:"id"`
			Method string `json:"method"`
			Params struct {
				Name      string         `json:"name"`
				Arguments map[string]any `json:"arguments"`
			} `json:"params"`
		}
		if err := json.NewDecoder(r.Body).Decode(&call); err != nil {
			t.Error(err)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if call.Method == "notifications/initialized" {
			w.WriteHeader(202)
			return
		}
		var result any = map[string]any{}
		if call.Method == "tools/call" {
			page := int(call.Params.Arguments["pageIndex"].(float64))
			if page == 1 {
				firstCalls.Add(1)
			}
			if page == 2 && fail.Load() {
				http.Error(w, "fixture temporary failure", 503)
				return
			}
			items := []any{}
			for i := (page - 1) * 500; i < min(page*500, 5001); i++ {
				items = append(items, map[string]any{"_id": fmt.Sprint(i), "name": name})
			}
			raw, _ := json.Marshal(map[string]any{"list": items})
			result = map[string]any{"content": []any{map[string]any{"type": "text", "text": string(raw)}}}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": call.ID, "result": result})
	}))
	defer source.Close()
	setSSJobSource(t, s, source.URL, nil)
	t.Cleanup(func() { _, _ = s.m.pg.Exec(`DELETE FROM assets WHERE type='app' AND app_name=$1`, name) })
	req := ssSyncReq{Dimension: "task", Targets: []string{"fixture"}, AssetTypes: []string{"app"}, PageSize: 500, Background: true}
	result := ssJobRun(t, s, req)
	if result.Status != "partial" || result.Stats.Written != 500 {
		t.Fatalf("first=%+v", result)
	}
	fail.Store(false)
	if err := s.m.pg.ResumeSyncRun(t.Context(), result.RunID); err != nil {
		t.Fatal(err)
	}
	result.Status = "running"
	result.Errors = nil
	s.ssJobMu.Lock()
	s.executeSSJob(req, result)
	result = ssJobResult(t, s, result.RunID)
	if result.Status != "success" || result.Stats.Written != 5001 || result.Stats.Fetched != 5001 || result.Stats.Created != 1 || result.Stats.Unchanged != 5000 || firstCalls.Load() != 1 {
		t.Fatalf("resumed=%+v page1 calls=%d", result, firstCalls.Load())
	}
}

func TestSSLiveSource(t *testing.T) {
	key := os.Getenv("ARTEX_TEST_SCOPESENTRY_KEY")
	if key == "" {
		t.Skip("explicit local MCP test key required")
	}
	s := ssTestServer(t)
	headers, _ := json.Marshal(map[string]string{"X-API-Key": key})
	setSSJobSource(t, s, "http://127.0.0.1:8082/mcp", headers)
	s.ctx = context.Background()
	result := ssJobRun(t, s, ssSyncReq{Dimension: "task", Targets: []string{"pvzol"}, AssetTypes: []string{"subdomain", "service", "app"}})
	if result.Status != "success" || result.Stats.Fetched != result.Stats.Written || len(result.Targets) != 3 {
		t.Fatalf("result=%+v", result)
	}
	t.Logf("live source: fetched=%d written=%d unique=%d", result.Stats.Fetched, result.Stats.Written, result.UniqueAssets.Total)
	empty := ssJobRun(t, s, ssSyncReq{Dimension: "task", Targets: []string{fmt.Sprintf("artex-empty-validation-%d", time.Now().UnixNano())}, AssetTypes: []string{"subdomain", "service", "app"}})
	if empty.Status != "success" || empty.Stats.Fetched != 0 {
		t.Fatalf("empty result=%+v", empty)
	}
}

func TestSSResumePartialPageUsesSavedPayload(t *testing.T) {
	s := ssTestServer(t)
	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var call struct {
			ID     int    `json:"id"`
			Method string `json:"method"`
		}
		_ = json.NewDecoder(r.Body).Decode(&call)
		w.Header().Set("Content-Type", "application/json")
		if call.Method == "tools/call" {
			t.Error("saved terminal page must not be fetched again")
		}
		if call.Method == "notifications/initialized" {
			w.WriteHeader(202)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": call.ID, "result": map[string]any{}})
	}))
	defer source.Close()
	setSSJobSource(t, s, source.URL, nil)
	req := ssSyncReq{Dimension: "task", Targets: []string{"saved"}, AssetTypes: []string{"app"}, PageSize: 100, Background: true}
	raw, _ := json.Marshal(req)
	run, err := s.m.pg.CreateSyncRun(t.Context(), nil, raw, json.RawMessage(`{"targets":[]}`))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = s.m.pg.Exec(`DELETE FROM sync_runs WHERE id=$1`, run.ID) })
	name := fmt.Sprintf("saved-page-%d", run.ID)
	first, _ := json.Marshal(map[string]string{"name": name})
	second, _ := json.Marshal(map[string]string{"name": name + "-second"})
	t.Cleanup(func() {
		_, _ = s.m.pg.Exec(`DELETE FROM assets WHERE app_name=$1 OR app_name=$2`, name, name+"-second")
	})
	p := &db.SyncPage{Items: []json.RawMessage{first, second}, Count: 2, Last: true}
	if err := s.m.pg.SaveSyncPage(t.Context(), run.ID, "saved", "app", 1, p); err != nil {
		t.Fatal(err)
	}
	if err := s.storeSSItem(t.Context(), &db.SyncObservation{RunID: run.ID, Target: "saved", AssetType: "app", ItemIndex: 0, Raw: first}); err != nil {
		t.Fatal(err)
	}
	result := newSSSyncResult()
	result.RunID, result.StartedAt = run.ID, run.StartedAt
	s.ssJobMu.Lock()
	s.executeSSJob(req, result)
	result = ssJobResult(t, s, run.ID)
	if result.Status != "success" || result.Stats.Fetched != 2 || result.Stats.Created != 2 || result.Stats.Written != 2 {
		t.Fatalf("result=%+v", result)
	}
}

func TestSSRetryOnlyFailedRecords(t *testing.T) {
	s := ssTestServer(t)
	run, err := s.m.pg.CreateSyncRun(t.Context(), nil, json.RawMessage(`{"dimension":"task","targets":["retry"],"asset_types":["app"]}`), json.RawMessage(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = s.m.pg.Exec(`DELETE FROM sync_runs WHERE id=$1`, run.ID) })
	name := fmt.Sprintf("retry-only-%d", run.ID)
	raw, _ := json.Marshal(map[string]string{"name": name})
	t.Cleanup(func() { _, _ = s.m.pg.Exec(`DELETE FROM assets WHERE app_name=$1`, name) })
	for i, status := range []string{"written", "failed", "skipped"} {
		if err := s.m.pg.RecordSyncObservation(t.Context(), &db.SyncObservation{RunID: run.ID, Target: "retry", AssetType: "app", ItemIndex: i, Status: status, Raw: raw}); err != nil {
			t.Fatal(err)
		}
	}
	result := ssJobRun(t, s, ssSyncReq{Dimension: "task", Targets: []string{"retry"}, AssetTypes: []string{"app"}, RetryRunID: run.ID})
	if result.Status != "success" || result.Stats.Fetched != 2 || result.Stats.Written != 2 || result.Stats.Created != 1 || result.Stats.Unchanged != 1 {
		t.Fatalf("retry=%+v", result)
	}
}
