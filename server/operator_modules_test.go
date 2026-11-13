package server

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"github.com/Autumn-27/artex/c2"
	"github.com/Autumn-27/artex/stage"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func operatorFixture(t *testing.T) *Server {
	t.Helper()
	pg := openIntranetTestDB(t)
	ctx, cancel := context.WithCancel(context.Background())
	s := &Server{ctx: ctx, m: &Manager{pg: pg, tasks: map[string]*Task{}}, jwtKey: []byte("synthetic-operator-fixture-key"), dataDir: t.TempDir(), skillDir: t.TempDir()}
	s.c2Ops = &c2OperatorState{ctx: ctx, cancel: cancel, running: map[int64]*c2Running{}}
	t.Cleanup(func() { s.closeOperatorModules(); cancel() })
	return s
}
func operatorRequest(t *testing.T, s *Server, method, path string, body any) (int, map[string]any) {
	t.Helper()
	mux := http.NewServeMux()
	s.registerOperatorRoutes(mux)
	b, _ := json.Marshal(body)
	req := httptest.NewRequest(method, path, bytes.NewReader(b))
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	var v map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &v); err != nil {
		t.Fatalf("%s %s status=%d body=%s", method, path, w.Code, w.Body.String())
	}
	return w.Code, v
}
func TestOperatorLibraryPresetAndSkillsContracts(t *testing.T) {
	s := operatorFixture(t)
	code := "operator-fixture-" + strconv.FormatInt(time.Now().UnixNano(), 10)
	t.Cleanup(func() { _, _ = s.m.pg.Exec("DELETE FROM kb_entries WHERE code=$1", code) })
	status, resp := operatorRequest(t, s, "POST", "/api/kb/entry", map[string]any{"code": code, "title": "Operator fixture", "category": "protocol", "kind": "script", "content": "synthetic", "verified": false})
	if status != 200 || resp["created"] != true {
		t.Fatal(status, resp)
	}
	status, resp = operatorRequest(t, s, "GET", "/api/kb/entry?code="+code, nil)
	if status != 200 || resp["content"] != "synthetic" || resp["hit_count"] != float64(0) {
		t.Fatal(status, resp)
	}
	for i := 0; i < 2; i++ {
		if _, err := s.m.pg.KBGet(t.Context(), code, true, "fixture"); err != nil {
			t.Fatal(err)
		}
	}
	_, resp = operatorRequest(t, s, "GET", "/api/kb/entry?code="+code, nil)
	if resp["hit_count"] != float64(2) {
		t.Fatal(resp)
	}
	var before int
	if err := s.m.pg.QueryRow("SELECT count(*) FROM intercept_rules").Scan(&before); err != nil {
		t.Fatal(err)
	}
	status, resp = operatorRequest(t, s, "POST", "/api/armor/presets", map[string]any{"name": "operator fixture", "text": "retain exact evidence"})
	if status != 200 {
		t.Fatal(status, resp)
	}
	id := int64(resp["id"].(float64))
	defer s.m.pg.Exec("DELETE FROM armor_presets WHERE id=$1", id)
	status, resp = operatorRequest(t, s, "POST", fmt.Sprintf("/api/armor/presets/%d/apply", id), map[string]any{})
	if status != 200 {
		t.Fatal(status, resp)
	}
	var count int
	if err := s.m.pg.QueryRow("SELECT count(*) FROM intercept_rules").Scan(&count); err != nil || count != before {
		t.Fatal("preset changed intercept rules", count, err)
	}

	defer s.m.pg.Exec("DELETE FROM operator_module_settings WHERE key='armor'")
	os.MkdirAll(filepath.Join(s.skillDir, "fixture", "refs"), 0700)
	os.WriteFile(filepath.Join(s.skillDir, "fixture", "SKILL.md"), []byte("---\nname: fixture\ndescription: fixture\n---\nneedle-operator"), 0600)
	os.WriteFile(filepath.Join(s.skillDir, "fixture", "refs", "child.md"), []byte("nested needle-operator reference"), 0600)
	status, resp = operatorRequest(t, s, "POST", "/api/skills/reindex", map[string]any{})
	if status != 200 || resp["indexed"] != float64(2) {
		t.Fatal(status, resp)
	}
	status, resp = operatorRequest(t, s, "GET", "/api/skills/search?q=needle-operator", nil)
	if status != 200 || len(resp["hits"].([]any)) != 2 {
		t.Fatal(status, resp)
	}
}
func TestOperatorC2EncryptedQueueResultsAndNoReplay(t *testing.T) {
	s := operatorFixture(t)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	ln.Close()
	status, resp := operatorRequest(t, s, "POST", "/api/c2/listeners", map[string]any{"name": "operator fixture", "kind": "http_beacon", "bind_host": "127.0.0.1", "bind_port": port, "envelope": "gcm", "start": true})
	if status != 200 || resp["started"] != true {
		t.Fatal(status, resp)
	}
	lid := int64(resp["id"].(float64))
	defer s.m.pg.Exec("DELETE FROM c2_listeners WHERE id=$1", lid)
	defer s.stopOperatorC2(lid)
	l, err := s.c2Listener(t.Context(), lid)
	if err != nil {
		t.Fatal(err)
	}
	key, _ := c2.ParseKey(l.Key)
	envelope := c2.Envelope{Key: key, Mode: "gcm"}
	beacon := func(message any) map[string]any {
		t.Helper()
		raw, _ := json.Marshal(message)
		wire, err := envelope.Seal(raw, "gcm")
		if err != nil {
			t.Fatal(err)
		}
		req, _ := http.NewRequest("POST", fmt.Sprintf("http://127.0.0.1:%d/updates/check", port), bytes.NewReader(wire))
		req.Header.Set("X-Implant-Token", l.Token)
		client := &http.Client{Timeout: 3 * time.Second, Transport: &http.Transport{Proxy: nil}}
		r, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer r.Body.Close()
		var v map[string]string
		if err = json.NewDecoder(r.Body).Decode(&v); err != nil {
			t.Fatal(r.Status, err)
		}
		encoded, _ := json.Marshal(v)
		plain, _, err := envelope.Open(encoded)
		if err != nil {
			t.Fatal(err)
		}
		var out map[string]any
		if err = json.Unmarshal(plain, &out); err != nil {
			t.Fatal(err)
		}
		return out
	}
	reply := beacon(map[string]any{"type": "checkin", "token": l.Token, "implant": map[string]any{"uuid": "fixture-" + fmt.Sprint(lid), "hostname": "fixture"}})
	sid := int64(reply["session_id"].(float64))
	defer s.m.pg.Exec("DELETE FROM c2_tasks WHERE session_id=$1", sid)
	defer s.m.pg.Exec("DELETE FROM c2_sessions WHERE id=$1", sid)
	id, _, err := s.queueOperatorC2Task(t.Context(), sid, json.RawMessage(`{"type":"shell","command":"fixture-not-executed"}`), "manual")
	if err != nil {
		t.Fatal(err)
	}
	reply = beacon(map[string]any{"type": "beacon", "session_id": sid})
	if len(reply["tasks"].([]any)) != 1 {
		t.Fatal(reply)
	}
	reply = beacon(map[string]any{"type": "beacon", "session_id": sid})
	if len(reply["tasks"].([]any)) != 0 {
		t.Fatal("task delivered twice", reply)
	}
	result := map[string]any{"type": "result", "session_id": sid, "results": []map[string]any{{"id": id, "output": "fixture-result", "status": "success"}}}
	beacon(result)
	beacon(result)
	var out, statusText string
	if err = s.m.pg.QueryRow("SELECT result_text,status FROM c2_tasks WHERE id=$1", id).Scan(&out, &statusText); err != nil || statusText != "success" || out != "fixture-result" {
		t.Fatal(out, statusText, err)
	}
	var stored string
	if err = s.m.pg.QueryRow("SELECT encryption_key FROM c2_listeners WHERE id=$1", lid).Scan(&stored); err != nil || stored == base64.StdEncoding.EncodeToString(key) || !strings.HasPrefix(stored, "gcm1:") {
		t.Fatal("unencrypted listener secret", err)
	}
}
func TestOperatorCleanupNeverTraversesSymlinks(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	os.WriteFile(filepath.Join(outside, "keep"), []byte("fixture"), 0600)
	if err := os.Symlink(outside, filepath.Join(root, "escape")); err != nil {
		t.Skip(err)
	}
	if cleanupPathSafe(root, filepath.Join(root, "escape", "keep")) {
		t.Fatal("symlink escape accepted")
	}
	if cleanupPathSafe(root, root) {
		t.Fatal("root deletion accepted")
	}
	if cleanupPathSafe(root, filepath.Join(root, "..", "outside")) {
		t.Fatal("parent escape accepted")
	}
}

func TestOperatorCleanupRetainsManagedResourcesAndDetectsNestedChanges(t *testing.T) {
	s := operatorFixture(t)
	root := filepath.Join(s.dataDir, "stage")
	manager, err := stage.New(s.ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	s.stage = manager
	entry, err := manager.Put(t.Context(), "fixture.txt", strings.NewReader("fixture"), stage.PutOpts{})
	if err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-48 * time.Hour)
	if err = os.Chtimes(filepath.Join(root, entry.Token), old, old); err != nil {
		t.Fatal(err)
	}
	items, _, err := s.scanOperatorCleanup(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	for _, v := range items {
		if v.Key == "stale_stage" && v.Count != 0 {
			t.Fatal("managed stage entry classified as garbage")
		}
	}
	dir := filepath.Join(s.dataDir, "fixture")
	os.MkdirAll(dir, 0700)
	file := filepath.Join(dir, "nested.txt")
	os.WriteFile(file, []byte("one"), 0600)
	_, before, err := cleanupTreeSnapshot(dir)
	if err != nil {
		t.Fatal(err)
	}
	os.WriteFile(file, []byte("changed"), 0600)
	_, after, err := cleanupTreeSnapshot(dir)
	if err != nil || before == after {
		t.Fatal("nested file change did not invalidate snapshot", err)
	}
}
