//go:build ignore

package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Autumn-27/artex/db"
)

func ssTestServer(t *testing.T) *Server {
	t.Helper()
	dsn := os.Getenv("ARTEX_PG_DSN")
	if dsn == "" {
		t.Skip("ARTEX_PG_DSN is required for sync integration tests")
	}
	pg, err := db.Open(dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = pg.Close() })
	return &Server{m: &Manager{pg: pg}}
}

func TestSSRunRetainsEarlierPagesAndHistoryWithoutSource(t *testing.T) {
	s := ssTestServer(t)
	name := fmt.Sprintf("sync-handler-%d", time.Now().UnixNano())
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
			http.Error(w, err.Error(), 400)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if call.Method == "notifications/initialized" {
			w.WriteHeader(202)
			return
		}
		result := any(map[string]any{})
		if call.Method == "tools/call" {
			if call.Params.Name != "list_assets" || call.Params.Arguments["search"] != "task==\"fixture\"" {
				t.Errorf("unexpected source request: %+v", call.Params)
			}
			page := int(call.Params.Arguments["pageIndex"].(float64))
			if page == 3 {
				http.Error(w, "fixture source stopped", 503)
				return
			}
			items := []any{map[string]any{"description": "missing name"}}
			if page == 1 {
				items = []any{map[string]any{"name": name, "_id": "original-asset"}}
			}
			payload, _ := json.Marshal(map[string]any{"list": items})
			result = map[string]any{"content": []any{map[string]any{"type": "text", "text": string(payload)}}}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": call.ID, "result": result})
	}))
	t.Cleanup(source.Close)
	previous, err := s.findMCPByName(scopeSentryMCPName)
	if err != nil {
		t.Fatal(err)
	}
	configured := db.MCPServer{Name: scopeSentryMCPName, Transport: "http", URL: source.URL, Enabled: true}
	if previous != nil {
		configured.ID = previous.ID
	}
	sourceID, err := s.m.pg.SaveMCP(&configured)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if previous != nil {
			_, _ = s.m.pg.SaveMCP(previous)
		} else {
			_ = s.m.pg.DeleteMCP(sourceID)
		}
	})
	t.Cleanup(func() { _, _ = s.m.pg.Exec(`DELETE FROM assets WHERE type='app' AND app_name=$1`, name) })
	w := httptest.NewRecorder()
	r := httptest.NewRequest("POST", "/api/sync/scopesentry/run", strings.NewReader(`{"dimension":"task","targets":["fixture"],"target_ids":{"fixture":"stable-task"},"asset_types":["app"],"page_size":1}`))
	s.syncSSRun(w, r)
	var result ssSyncResult
	if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil || w.Code != 200 {
		t.Fatalf("status=%d body=%s error=%v", w.Code, w.Body, err)
	}
	t.Cleanup(func() { _, _ = s.m.pg.Exec(`DELETE FROM sync_runs WHERE id=$1`, result.RunID) })
	if result.RunID == 0 || result.Status != "partial" || result.Stats.Fetched != 2 || result.Stats.Written != 1 || result.Stats.Created != 1 || result.Stats.Skipped != 1 || result.UniqueAssets.Total != 1 {
		t.Fatalf("result=%+v", result)
	}
	if len(result.Targets) != 1 || !strings.Contains(result.Targets[0].Error, "第 3 页") {
		t.Fatalf("targets=%+v", result.Targets)
	}
	source.Close()
	history := httptest.NewRecorder()
	hr := httptest.NewRequest("GET", "/api/sync/scopesentry/history/1?include_raw=1", nil)
	hr.SetPathValue("id", strconv.FormatInt(result.RunID, 10))
	s.syncSSHistoryDetail(history, hr)
	var detail struct {
		Run   db.SyncRun           `json:"run"`
		Items []db.SyncObservation `json:"items"`
		Total int                  `json:"total"`
	}
	if err := json.Unmarshal(history.Body.Bytes(), &detail); err != nil || history.Code != 200 || detail.Total != 2 {
		t.Fatalf("history status=%d body=%s error=%v", history.Code, history.Body, err)
	}
	if detail.Run.Status != "partial" || detail.Items[0].SourceTargetID != "stable-task" || detail.Items[0].SourceAssetID != "original-asset" || detail.Items[0].Outcome != "created" || len(detail.Items[0].Raw) == 0 {
		t.Fatalf("detail=%+v", detail)
	}
}

func TestSSDerivedIPFailureRollsBackSourceItem(t *testing.T) {
	s := ssTestServer(t)
	run, err := s.m.pg.CreateSyncRun(t.Context(), nil, json.RawMessage(`{}`), json.RawMessage(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = s.m.pg.Exec(`DELETE FROM sync_runs WHERE id=$1`, run.ID) })
	host := fmt.Sprintf("rollback-%d.test", time.Now().UnixNano())
	raw, _ := json.Marshal(map[string]any{"host": host, "ip": []string{"192.0.2.179", "invalid-hostname"}})
	item := &db.SyncObservation{RunID: run.ID, Target: "test", AssetType: "subdomain", Raw: raw}
	if err := s.storeSSItem(t.Context(), item); err != nil {
		t.Fatal(err)
	}
	if item.Status != "failed" || item.AssetID != nil || item.Outcome != "" || len(item.AssetChanges) != 0 || item.DerivedIPWritten != 0 || item.DerivedIPFailed != 1 {
		t.Fatalf("item=%+v", item)
	}
	var count int
	if err := s.m.pg.QueryRow(`SELECT count(*) FROM assets WHERE domain=$1 OR (type='ip' AND ip='192.0.2.179')`, host).Scan(&count); err != nil || count != 0 {
		t.Fatalf("rolled back assets=%d error=%v", count, err)
	}
	items, total, err := s.m.pg.ListSyncObservations(t.Context(), run.ID, 20, 0, true)
	if err != nil || total != 1 || items[0].Status != "failed" || !strings.Contains(items[0].Reason, "invalid-hostname") {
		t.Fatalf("history=%+v error=%v", items, err)
	}
}

type ssPageFixture struct {
	total         int
	calls         int
	failPage      int
	err           error
	response      string
	countResponse string
}

func (f *ssPageFixture) Call(_ context.Context, tool string, input any) (string, error) {
	if tool == "count_assets" && f.countResponse != "" {
		return f.countResponse, nil
	}
	if tool != "list_assets" {
		return "", fmt.Errorf("unexpected tool %s", tool)
	}
	f.calls++
	args := input.(map[string]any)
	page, size := args["pageIndex"].(int), args["pageSize"].(int)
	if page == f.failPage {
		return "", f.err
	}
	if f.response != "" {
		return f.response, nil
	}
	items := []json.RawMessage{}
	for index := (page - 1) * size; index < min(page*size, f.total); index++ {
		items = append(items, json.RawMessage(fmt.Sprintf(`{"id":"%d"}`, index)))
	}
	raw, _ := json.Marshal(map[string]any{"list": items})
	return string(raw), nil
}

func TestSSNullPageRequiresVerifiedBoundary(t *testing.T) {
	for _, tc := range []struct {
		name, count string
		wantErr     bool
	}{
		{"empty", `{"total":0}`, false},
		{"nonempty", `{"total":1}`, true},
		{"missing", `{}`, true},
		{"negative", `{"total":-1}`, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fixture := &ssPageFixture{response: `{"list":null}`, countResponse: tc.count}
			items, truncated, err := (&Server{}).ssPageAll(t.Context(), fixture, "asset", nil, 100)
			if (err != nil) != tc.wantErr || len(items) != 0 || truncated {
				t.Fatalf("items=%v truncated=%v err=%v", items, truncated, err)
			}
		})
	}
}

func TestSSPageAllDistinguishesExactCap(t *testing.T) {
	for _, total := range []int{0, 1, 4999, 5000, 5001, 6000} {
		for _, pageSize := range []int{100, 333, 500} {
			t.Run(fmt.Sprintf("%d_%d", total, pageSize), func(t *testing.T) {
				fixture := &ssPageFixture{total: total}
				items, truncated, err := (&Server{}).ssPageAll(t.Context(), fixture, "subdomain", nil, pageSize)
				if err != nil || len(items) != min(total, 5000) || truncated != (total > 5000) {
					t.Fatalf("items=%d truncated=%v error=%v", len(items), truncated, err)
				}
				if total == 5000 && pageSize == 100 && fixture.calls != 51 {
					t.Fatalf("exact full cap was not probed: calls=%d", fixture.calls)
				}
			})
		}
	}
}

func TestSSPageAllRetainsEarlierPagesOnFailure(t *testing.T) {
	cause := errors.New("source unavailable")
	fixture := &ssPageFixture{total: 1000, failPage: 3, err: cause}
	items, truncated, err := (&Server{}).ssPageAll(t.Context(), fixture, "asset", nil, 100)
	if len(items) != 200 || truncated || !errors.Is(err, cause) || !strings.Contains(err.Error(), "第 3 页") {
		t.Fatalf("items=%d truncated=%v error=%v", len(items), truncated, err)
	}
	fixture = &ssPageFixture{total: 5000, failPage: 51, err: cause}
	items, truncated, err = (&Server{}).ssPageAll(t.Context(), fixture, "asset", nil, 100)
	if len(items) != 5000 || truncated || !errors.Is(err, cause) {
		t.Fatalf("failed cap probe must retain data without claiming known truncation: %d %v %v", len(items), truncated, err)
	}
}

func TestSSPageAllRejectsMalformedEnvelope(t *testing.T) {
	for _, response := range []string{`{}`, `null`, `{"error":"upstream failed"}`, `{"list":null}`, `{"list":{}}`, `[]`, `{`} {
		t.Run(response, func(t *testing.T) {
			fixture := &ssPageFixture{response: response}
			_, _, err := (&Server{}).ssPageAll(t.Context(), fixture, "asset", nil, 100)
			if err == nil {
				t.Fatalf("malformed response accepted: %s", response)
			}
		})
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	fixture := &ssPageFixture{}
	_, _, err := (&Server{}).ssPageAll(ctx, fixture, "asset", nil, 100)
	if !errors.Is(err, context.Canceled) || fixture.calls != 0 {
		t.Fatalf("canceled traversal called source: calls=%d error=%v", fixture.calls, err)
	}
}

func TestSSAssetQueryRespectsTypeCapabilities(t *testing.T) {
	name := "task \"quoted\" \\ value"
	app := ssAssetQuery("app", "task", name)
	if len(app) != 1 || app["search"] != "task=="+strconv.Quote(name) {
		t.Fatalf("app task query=%v", app)
	}
	for _, dimension := range []string{"task", "project"} {
		want := map[string]any{"filter": map[string]any{dimension: []string{name}}}
		if got := ssAssetQuery("asset", dimension, name); !reflect.DeepEqual(got, want) {
			t.Fatalf("asset query=%v want=%v", got, want)
		}
	}
	want := map[string]any{"filter": map[string]any{"project": []string{"id"}}}
	if got := ssAssetQuery("app", "project", "id"); !reflect.DeepEqual(got, want) {
		t.Fatalf("app project query=%v", got)
	}
}

func TestSSPrepareReportsSkippedAndInvalidRecords(t *testing.T) {
	for _, test := range []struct {
		kind string
		raw  string
		skip bool
		err  bool
	}{
		{"subdomain", `{}`, true, false},
		{"subdomain", `{"host":"   "}`, true, false},
		{"subdomain", `{"host":23}`, false, true},
		{"app", `{"description":"missing name"}`, true, false},
		{"service", `{"service":"http"}`, true, false},
		{"service", `{"service":"https"}`, true, false},
		{"service", `{"port":"22"}`, true, false},
		{"service", `{"ip":"192.0.2.1","port":"invalid"}`, false, true},
		{"service", `{"ip":"192.0.2.1","port":"70000"}`, false, true},
		{"service", `{"ip":"192.0.2.1","port":22}`, false, false},
		{"service", `{"ip":"192.0.2.1","port":"22"}`, false, false},
		{"service", `{"url":"https://example.invalid/"}`, false, false},
	} {
		t.Run(test.kind+test.raw, func(t *testing.T) {
			prepared, err := ssPrepareItem(test.kind, json.RawMessage(test.raw))
			if (err != nil) != test.err || (prepared.skipReason != "") != test.skip {
				t.Fatalf("prepared=%+v error=%v", prepared, err)
			}
			if !test.err && !test.skip && prepared.apply == nil {
				t.Fatal("valid record has no mutation")
			}
		})
	}
}

func TestSSStatusesDoNotReportOmissionsAsSuccess(t *testing.T) {
	for _, test := range []struct {
		result ssTargetResult
		want   string
	}{
		{ssTargetResult{}, "success"},
		{ssTargetResult{Error: "unavailable"}, "failed"},
		{ssTargetResult{Stats: db.SyncStats{Skipped: 1}}, "failed"},
		{ssTargetResult{Stats: db.SyncStats{Written: 1, Failed: 1}}, "partial"},
		{ssTargetResult{Stats: db.SyncStats{Written: 5000}, Truncated: true}, "partial"},
		{ssTargetResult{Stats: db.SyncStats{Written: 5000}}, "success"},
	} {
		if got := ssTargetStatus(test.result); got != test.want {
			t.Fatalf("target %+v status=%s want=%s", test.result, got, test.want)
		}
	}
	result := newSSSyncResult()
	if ssOverallStatus(result) != "failed" {
		t.Fatal("no attempted targets cannot be success")
	}
	result.Targets = []ssTargetResult{{Status: "success"}}
	if ssOverallStatus(result) != "success" {
		t.Fatal("empty successful source should succeed")
	}
	result.Warnings = []string{"company attribution failed"}
	if ssOverallStatus(result) != "partial" {
		t.Fatal("company failure must prevent full success")
	}
}

func TestSSUniqueAssetsMergeRepeatedAndDerivedWrites(t *testing.T) {
	seen := map[int64]db.SyncAssetChange{}
	ssMergeAssetChanges(seen, []db.SyncAssetChange{
		{AssetID: 1, Outcome: "created", Direct: true},
		{AssetID: 2, Outcome: "unchanged"},
		{AssetID: 3, Outcome: "unchanged"},
	})
	ssMergeAssetChanges(seen, []db.SyncAssetChange{
		{AssetID: 1, Outcome: "updated"},
		{AssetID: 2, Outcome: "updated", Direct: true},
	})
	ssMergeAssetChanges(seen, []db.SyncAssetChange{{AssetID: 2, Outcome: "unchanged"}})
	got := ssUniqueAssets(seen)
	want := db.SyncUniqueAssets{Total: 3, Created: 1, Updated: 1, Unchanged: 1, Direct: 2, Derived: 1}
	if got != want {
		t.Fatalf("unique assets=%+v want=%+v", got, want)
	}
}

func TestSSNormalizeRequestAndInterruptedHistory(t *testing.T) {
	req := ssSyncReq{Dimension: "task", Targets: []string{"name", "name"}, AssetTypes: []string{"app", "app"}, TargetIDs: map[string]string{"name": "stable"}}
	if err := req.normalize(); err != nil || len(req.Targets) != 1 || len(req.AssetTypes) != 1 || req.sourceTargetID("name") != "stable" {
		t.Fatalf("request=%+v error=%v", req, err)
	}
	result := newSSSyncResult()
	result.Targets = []ssTargetResult{{Status: "running"}, {Status: "success"}}
	result.Stats.Written = 7
	raw, _ := json.Marshal(result)
	now := time.Now().UTC()
	run := &db.SyncRun{ID: 123, Status: "interrupted", Result: raw, StartedAt: now, FinishedAt: &now}
	normalizeSSHistoryResult(run)
	if err := json.Unmarshal(run.Result, &result); err != nil {
		t.Fatal(err)
	}
	if result.RunID != 123 || result.Status != "interrupted" || result.Targets[0].Status != "interrupted" || result.Targets[1].Status != "success" || result.Stats.Written != 7 {
		t.Fatalf("history=%+v", result)
	}
}
