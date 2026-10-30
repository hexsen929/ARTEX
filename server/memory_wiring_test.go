package server

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/Autumn-27/artex/agent"
	"github.com/Autumn-27/artex/db"
	"github.com/Autumn-27/norma/llm"
	"github.com/Autumn-27/norma/transcript"
)

type memoryHTTPFixture struct {
	server  *Server
	pg      *db.DB
	handler http.Handler
	token   string
	entries []int64
	calls   int
}

// Construct only the HTTP surface and store. In particular, do not invoke New,
// NewManager, startMemoryWorker, task engines, network tools or real providers.
func newMemoryHTTPFixture(t *testing.T) *memoryHTTPFixture {
	t.Helper()
	dsn := os.Getenv("ARTEX_PG_DSN")
	if dsn == "" {
		t.Skip("isolated test PostgreSQL is not configured")
	}
	pg, err := db.Open(dsn)
	if err != nil {
		t.Fatal(err)
	}
	previous, err := pg.GetMemorySettings(t.Context())
	if err != nil {
		pg.Close()
		t.Fatal(err)
	}
	f := &memoryHTTPFixture{pg: pg}
	f.server = &Server{ctx: t.Context(), m: &Manager{pg: pg, dir: t.TempDir(), tasks: map[string]*Task{}},
		jwtKey: testJWTKey, authkv: newMemKV(), llmOn: true, llmCfg: agent.Config{Model: "memory-fixture"}}
	f.server.llmDirect = retestProvider{complete: func(context.Context, llm.CompletionRequest) (llm.Message, string, llm.Usage, error) {
		f.calls++
		return llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{llm.TextBlock("[]")}}, "end_turn", llm.Usage{}, nil
	}}
	f.token, err = signJWT(testJWTKey, 0)
	if err != nil {
		pg.Close()
		t.Fatal(err)
	}
	f.handler = f.server.Handler()
	t.Cleanup(func() {
		for _, id := range f.entries {
			_, _ = pg.Exec(`DELETE FROM memory_audit WHERE entry_id=$1`, id)
			_, _ = pg.Exec(`DELETE FROM memory_entries WHERE id=$1`, id)
		}
		if current, err := pg.GetMemorySettings(context.Background()); err == nil {
			_, _ = pg.UpdateMemorySettings(context.Background(), previous, current.Epoch)
		}
		if f.calls != 0 {
			t.Errorf("HTTP-only fixture unexpectedly invoked an extractor: %d", f.calls)
		}
		_ = pg.Close()
	})
	baseline := previous
	baseline.Enabled, baseline.AutoExtract, baseline.GlobalShare = true, true, true
	baseline.ProfileID, baseline.MaxEntries, baseline.MaxTokens = 0, 5, 2000
	baseline.MaxJobsPerHour, baseline.RetentionDays = 12, 90
	if _, err := pg.UpdateMemorySettings(t.Context(), baseline, previous.Epoch); err != nil {
		t.Fatal(err)
	}
	return f
}

func (f *memoryHTTPFixture) request(t *testing.T, method, path string, body any) *httptest.ResponseRecorder {
	t.Helper()
	var data []byte
	if body != nil {
		var err error
		data, err = json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
	}
	req := httptest.NewRequest(method, path, bytes.NewReader(data))
	req.Header.Set("Authorization", "Bearer "+f.token)
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	f.handler.ServeHTTP(w, req)
	return w
}

func memoryEntryResponse(t *testing.T, w *httptest.ResponseRecorder, status int) db.MemoryEntry {
	t.Helper()
	if w.Code != status {
		t.Fatalf("HTTP %d, want %d: %s", w.Code, status, w.Body)
	}
	var entry db.MemoryEntry
	if err := json.Unmarshal(w.Body.Bytes(), &entry); err != nil {
		t.Fatal(err)
	}
	if entry.ID < 1 || entry.Version < 1 {
		t.Fatalf("missing entry identity: %+v", entry)
	}
	return entry
}

func (f *memoryHTTPFixture) create(t *testing.T, suffix string, extra map[string]any) db.MemoryEntry {
	t.Helper()
	input := map[string]any{"title": "memory-wiring-" + t.Name() + suffix, "content": "工具平台兼容性的合成参考记录 " + suffix,
		"kind": "lesson", "status": "active", "confidence": "unverified", "applicability": "只用于合成环境", "tags": []string{"fixture"}}
	for key, value := range extra {
		input[key] = value
	}
	entry := memoryEntryResponse(t, f.request(t, http.MethodPost, "/api/memories", input), http.StatusCreated)
	f.entries = append(f.entries, entry.ID)
	return entry
}

func TestMemoryRoutesRequireAdministratorJWT(t *testing.T) {
	s := &Server{jwtKey: testJWTKey, authkv: newMemKV()}
	handler := s.Handler()
	for _, route := range []struct{ method, path string }{
		{"GET", "/api/memories"}, {"POST", "/api/memories"},
		{"GET", "/api/memories/1"}, {"PATCH", "/api/memories/1"}, {"DELETE", "/api/memories/1"},
		{"GET", "/api/memories/settings"}, {"PUT", "/api/memories/settings"},
		{"GET", "/api/memories/usage"}, {"POST", "/api/memories/extract"},
	} {
		t.Run(route.method+route.path, func(t *testing.T) {
			w := httptest.NewRecorder()
			req := httptest.NewRequest(route.method, route.path, strings.NewReader(`{}`))
			handler.ServeHTTP(w, req)
			if w.Code != http.StatusUnauthorized {
				t.Fatalf("unauthenticated route: %d %s", w.Code, w.Body)
			}
		})
	}
}

func TestMemorySettingsEpochConflictDoesNotOverwrite(t *testing.T) {
	f := newMemoryHTTPFixture(t)
	w := f.request(t, http.MethodGet, "/api/memories/settings", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("settings: %d %s", w.Code, w.Body)
	}
	var initial db.MemorySettings
	if err := json.Unmarshal(w.Body.Bytes(), &initial); err != nil {
		t.Fatal(err)
	}
	changed := initial
	changed.MaxEntries = 4
	w = f.request(t, http.MethodPut, "/api/memories/settings", changed)
	if w.Code != http.StatusOK {
		t.Fatalf("save: %d %s", w.Code, w.Body)
	}
	var saved db.MemorySettings
	if err := json.Unmarshal(w.Body.Bytes(), &saved); err != nil {
		t.Fatal(err)
	}
	if saved.Epoch != initial.Epoch+1 || saved.MaxEntries != 4 {
		t.Fatalf("wrong saved settings: %+v", saved)
	}
	initial.MaxTokens = 128
	w = f.request(t, http.MethodPut, "/api/memories/settings", initial)
	if w.Code != http.StatusConflict {
		t.Fatalf("stale settings save: %d %s", w.Code, w.Body)
	}
	current, err := f.pg.GetMemorySettings(t.Context())
	if err != nil || current.MaxEntries != 4 || current.MaxTokens != saved.MaxTokens || current.Epoch != saved.Epoch {
		t.Fatalf("stale writer changed state: %+v err=%v", current, err)
	}
}

func TestMemoryCRUDRejectsSensitiveInputsAndStaleMutation(t *testing.T) {
	f := newMemoryHTTPFixture(t)
	before := 0
	if err := f.pg.QueryRow(`SELECT count(*) FROM memory_entries`).Scan(&before); err != nil {
		t.Fatal(err)
	}
	for _, input := range []map[string]any{
		{"title": "fixture", "content": "password=fixture-sensitive-value", "kind": "lesson"},
		{"title": "fixture", "content": "部署经验", "kind": "lesson", "tags": []string{"password=fixture-sensitive-value"}},
		{"title": "fixture", "content": "部署经验", "kind": "lesson", "target": "https://fixture-user:fixture-secret@HOST/"},
		{"title": "fixture", "content": "部署经验", "kind": "lesson", "source_kind": "node", "source_id": "1"},
	} {
		w := f.request(t, http.MethodPost, "/api/memories", input)
		if w.Code != http.StatusBadRequest || strings.Contains(w.Body.String(), "fixture-sensitive-value") || strings.Contains(w.Body.String(), "fixture-secret") {
			t.Fatalf("sensitive/forged source request: %d %s", w.Code, w.Body)
		}
	}
	after := 0
	if err := f.pg.QueryRow(`SELECT count(*) FROM memory_entries`).Scan(&after); err != nil || after != before {
		t.Fatalf("rejected inputs were persisted: before=%d after=%d err=%v", before, after, err)
	}
	entry := f.create(t, "-crud", nil)
	path := fmt.Sprintf("/api/memories/%d", entry.ID)
	read := memoryEntryResponse(t, f.request(t, http.MethodGet, path, nil), http.StatusOK)
	if read.Content != entry.Content || read.State != "active" || read.Automatic {
		t.Fatalf("manual record contract: %+v", read)
	}
	updated := memoryEntryResponse(t, f.request(t, http.MethodPatch, path, map[string]any{"version": entry.Version, "status": "disabled"}), http.StatusOK)
	if updated.State != "disabled" || updated.Version != entry.Version+1 || updated.Content != entry.Content {
		t.Fatalf("toggle overwrote content: %+v", updated)
	}
	if w := f.request(t, http.MethodDelete, path, map[string]any{"version": entry.Version}); w.Code != http.StatusConflict {
		t.Fatalf("stale delete: %d %s", w.Code, w.Body)
	}
	if w := f.request(t, http.MethodDelete, path, map[string]any{"version": updated.Version}); w.Code != http.StatusOK {
		t.Fatalf("delete: %d %s", w.Code, w.Body)
	}
	stored, err := f.pg.GetMemoryEntry(t.Context(), entry.ID)
	if err != nil || stored == nil || stored.State != "deleted" || stored.Content != "" || stored.Title != "" {
		t.Fatalf("deleted memory retained body: %+v err=%v", stored, err)
	}
}

func TestMemoryPatchOmissionEmptyValuesAndExpiryNull(t *testing.T) {
	f := newMemoryHTTPFixture(t)
	expires := time.Now().UTC().Add(48 * time.Hour).Truncate(time.Microsecond)
	entry := f.create(t, "-patch", map[string]any{"target": "HOST", "tags": []string{"old"}, "expires_at": expires})
	path := fmt.Sprintf("/api/memories/%d", entry.ID)
	for _, field := range []string{"title", "content"} {
		w := f.request(t, http.MethodPatch, path, map[string]any{"version": entry.Version, field: ""})
		if w.Code != http.StatusBadRequest {
			t.Fatalf("empty required %s accepted: %d %s", field, w.Code, w.Body)
		}
	}
	changed := memoryEntryResponse(t, f.request(t, http.MethodPatch, path, map[string]any{"version": entry.Version, "title": "新的合成标题"}), http.StatusOK)
	if changed.Content != entry.Content || changed.Target != "HOST" || changed.ExpiresAt == nil || !changed.ExpiresAt.Equal(expires) || len(changed.Tags) != 1 {
		t.Fatalf("omitted fields lost: %+v", changed)
	}
	cleared := memoryEntryResponse(t, f.request(t, http.MethodPatch, path, map[string]any{
		"version": changed.Version, "target": "", "applicability": "", "tags": []string{}, "expires_at": nil,
	}), http.StatusOK)
	if cleared.Target != "" || cleared.Applicability != "" || len(cleared.Tags) != 0 || cleared.ExpiresAt != nil || cleared.Content != entry.Content {
		t.Fatalf("explicit clear was ignored: %+v", cleared)
	}
	if w := f.request(t, http.MethodPatch, path, map[string]any{"version": cleared.Version, "expires_at": "not-a-time"}); w.Code != http.StatusBadRequest {
		t.Fatalf("invalid expiry accepted: %d %s", w.Code, w.Body)
	}
}

func TestMemoryListPaginationAndQueryBoundaries(t *testing.T) {
	f := newMemoryHTTPFixture(t)
	marker := "memory-wiring-" + t.Name()
	first := f.create(t, "-first", nil)
	second := f.create(t, "-second", nil)
	for offset, wantID := range map[int]int64{0: second.ID, 1: first.ID} {
		w := f.request(t, http.MethodGet, "/api/memories?q="+url.QueryEscape(marker)+"&limit=1&offset="+strconv.Itoa(offset), nil)
		var result struct {
			Items []db.MemoryEntry `json:"items"`
			Total int              `json:"total"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil || w.Code != http.StatusOK || result.Total != 2 || len(result.Items) != 1 || result.Items[0].ID != wantID {
			t.Fatalf("offset %d: HTTP=%d body=%s err=%v", offset, w.Code, w.Body, err)
		}
	}
	for _, path := range []string{
		"/api/memories/0", "/api/memories/-1", "/api/memories/not-a-number", "/api/memories/9223372036854775808",
		"/api/memories?task_id=0", "/api/memories?conversation_id=-1", "/api/memories?task_id=1%20OR%201=1",
		"/api/memories?offset=-1", "/api/memories?offset=1000001", "/api/memories?offset=invalid",
		"/api/memories?q=" + strings.Repeat("q", 513),
		"/api/memories/usage?conversation_id=1%20OR%201=1", "/api/memories/usage?task_id=-1",
		"/api/memories/usage?session_id=" + strings.Repeat("s", 129), "/api/memories/usage?session_id=%FF",
	} {
		w := f.request(t, http.MethodGet, path, nil)
		if w.Code != http.StatusBadRequest {
			t.Errorf("invalid path %q: %d %s", path, w.Code, w.Body)
		}
	}
}

func TestMemorySourceOwnershipSurvivesPatch(t *testing.T) {
	f := newMemoryHTTPFixture(t)
	conversation, err := f.pg.CreateConversation("worker", "memory source fixture", nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = f.pg.Exec(`DELETE FROM conversations WHERE id=$1`, conversation.ID) })
	entry := f.create(t, "-source", map[string]any{"origin_conversation_id": conversation.ID, "session_id": fmt.Sprintf("conv-%d", conversation.ID), "agent_key": "worker"})
	path := fmt.Sprintf("/api/memories/%d", entry.ID)
	updated := memoryEntryResponse(t, f.request(t, http.MethodPatch, path, map[string]any{"version": entry.Version, "title": "来源归属保留"}), http.StatusOK)
	if updated.OriginConversationID != conversation.ID || updated.SessionID != entry.SessionID || updated.AgentKey != entry.AgentKey {
		t.Fatalf("source jump identity lost: %+v", updated)
	}
	w := f.request(t, http.MethodPatch, path, map[string]any{"version": updated.Version, "origin_conversation_id": conversation.ID + 1})
	if w.Code != http.StatusBadRequest {
		t.Fatalf("PATCH source ownership accepted: %d %s", w.Code, w.Body)
	}
}

func TestMemoryUsageFiltersSessionsBeforeLimit(t *testing.T) {
	f := newMemoryHTTPFixture(t)
	entry := f.create(t, "-audit", nil)
	taskID := int64(800000000) + entry.ID
	rows := []struct {
		task, conversation int64
		session            string
	}{
		{taskID, 0, "intent:7"}, {taskID, 0, "plan"}, {taskID, 0, "main:2"},
		{taskID + 1, 0, "intent:7"}, {0, taskID, "conv-7"}, {0, taskID + 1, "conv-7"},
	}
	for _, row := range rows {
		if _, err := f.pg.Exec(`INSERT INTO memory_audit(entry_id,entry_version,task_id,conversation_id,session_id,agent_key,action)
			VALUES($1,1,$2,$3,$4,'worker','admitted')`, entry.ID, row.task, row.conversation, row.session); err != nil {
			t.Fatal(err)
		}
	}
	for _, test := range []struct {
		query   string
		session string
		task    int64
		conv    int64
	}{
		{fmt.Sprintf("task_id=%d&session_id=intent%%3A7", taskID), "intent:7", taskID, 0},
		{fmt.Sprintf("task_id=%d&session_id=plan", taskID), "plan", taskID, 0},
		{fmt.Sprintf("task_id=%d&session_id=main%%3A2", taskID), "main:2", taskID, 0},
		{fmt.Sprintf("conversation_id=%d&session_id=conv-7", taskID), "conv-7", 0, taskID},
	} {
		w := f.request(t, http.MethodGet, "/api/memories/usage?"+test.query+"&limit=1", nil)
		var result struct {
			Items []struct {
				TaskID         int64  `json:"task_id"`
				ConversationID int64  `json:"conversation_id"`
				SessionID      string `json:"session_id"`
				Result         string `json:"result"`
			} `json:"items"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil || w.Code != http.StatusOK || len(result.Items) != 1 {
			t.Fatalf("usage %s: %d %s err=%v", test.query, w.Code, w.Body, err)
		}
		item := result.Items[0]
		if item.TaskID != test.task || item.ConversationID != test.conv || item.SessionID != test.session || item.Result != "admitted" {
			t.Fatalf("cross-session usage leaked: %+v", item)
		}
		if strings.Contains(w.Body.String(), entry.Content) || strings.Contains(w.Body.String(), entry.Title) {
			t.Fatal("usage API retained memory body or title")
		}
	}
	w := f.request(t, http.MethodGet, fmt.Sprintf("/api/memories/usage?task_id=%d&session_id=%s", taskID, url.QueryEscape("intent:7' OR '1'='1")), nil)
	var unmatched struct {
		Items []json.RawMessage `json:"items"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &unmatched); err != nil || w.Code != http.StatusOK || len(unmatched.Items) != 0 {
		t.Fatalf("session filter was interpreted as SQL: %d %s err=%v", w.Code, w.Body, err)
	}
}

func TestMemoryRecallQueryIgnoresToolsSystemAndAssistant(t *testing.T) {
	req := llm.CompletionRequest{System: []string{"system-only-query"}, Messages: []llm.Message{
		llm.UserText("human-query-compatible-platform"),
		{Role: llm.RoleAssistant, Content: []llm.ContentBlock{llm.TextBlock("assistant-query")}},
		{Role: llm.RoleUser, Content: []llm.ContentBlock{llm.ToolResultText("fixture", "tool-query", false)}},
	}}
	if got := memoryRecallQuery(req); got != "human-query-compatible-platform" {
		t.Fatalf("query derived from non-human text: %q", got)
	}
	if got := memoryRecallQuery(llm.CompletionRequest{System: req.System, Messages: req.Messages[1:]}); got != "" {
		t.Fatalf("non-human material became a query: %q", got)
	}
	if got := memoryRecallQuery(llm.CompletionRequest{Messages: []llm.Message{llm.UserText("password=fixture-secret")}}); got != "" {
		t.Fatalf("secret used in recall search: %q", got)
	}
	got := memoryRecallQuery(llm.CompletionRequest{Messages: []llm.Message{llm.UserText(strings.Repeat("中", 300))}})
	if len(got) > 512 || !utf8.ValidString(got) || got == "" {
		t.Fatalf("query truncation broke UTF-8 or budget: %q", got)
	}
}

func TestMemoryRecallRestrictedRolesAndUnknownIdentityDoNotReadStore(t *testing.T) {
	// With no manager/store, these must return before even attempting a lookup.
	s := &Server{}
	request := llm.CompletionRequest{System: []string{"task_id=99"}, Messages: []llm.Message{llm.UserText("task_id=99 兼容性经验")}}
	for _, role := range []string{db.CredentialAgentKey, "asset_mapper"} {
		result, err := s.recallMemory(t.Context(), agent.RunInfo{TaskID: 99}, role, request)
		if err != nil || result.Block != "" || len(result.References) != 0 {
			t.Fatalf("restricted role %s accessed global memory: %+v err=%v", role, result, err)
		}
	}
	result, err := s.recallMemory(t.Context(), agent.RunInfo{}, "worker", request)
	if err != nil || result.Block != "" || len(result.References) != 0 {
		t.Fatalf("text manufactured a run identity: %+v err=%v", result, err)
	}
}

func TestMemoryUISessionKeysMatchUsageAttribution(t *testing.T) {
	tests := []struct {
		name, role, transcript, want string
		info                         agent.RunInfo
	}{
		{"planner", "planner", "exp1-planner", "plan", agent.RunInfo{TaskID: 1}},
		{"main default", "mainagent", "exp1-main", "main:0", agent.RunInfo{TaskID: 1}},
		{"main segment", "mainagent", "exp1-main-s3", "main:3", agent.RunInfo{TaskID: 1}},
		{"worker", "worker", "exp1-worker-i9", "intent:9", agent.RunInfo{TaskID: 1, IntentID: 9}},
		{"conversation", "worker", "conv-17", "conv-17", agent.RunInfo{SessionID: "conv-17"}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			ctx := transcript.WithSessionID(t.Context(), test.transcript)
			if got := memorySessionID(ctx, test.info, test.role); got != test.want {
				t.Fatalf("session=%q, want=%q", got, test.want)
			}
		})
	}
	for session, want := range map[string]int64{"conv-17": 17, "conv-0": 0, "conv--1": 0, "intent:17": 0, "conv-17-extra": 0, "conv-9223372036854775808": 0} {
		if got := memoryConversationID(session); got != want {
			t.Errorf("conversation %q=%d, want %d", session, got, want)
		}
	}
}
