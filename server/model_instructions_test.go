package server

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Autumn-27/artex/agent"
	"github.com/Autumn-27/artex/db"
	"github.com/Autumn-27/norma/llm"
)

func instructionTestMux(s *Server) *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/llm/instruction-files", s.listModelInstructions)
	mux.HandleFunc("POST /api/llm/instruction-files", s.createModelInstructions)
	mux.HandleFunc("GET /api/llm/instruction-files/{name}", s.readModelInstructions)
	mux.HandleFunc("PUT /api/llm/instruction-files/{name}", s.updateModelInstructions)
	mux.HandleFunc("DELETE /api/llm/instruction-files/{name}", s.deleteModelInstructions)
	mux.HandleFunc("POST /api/llm/profiles", s.pgSaveProfile)
	mux.HandleFunc("GET /api/llm", s.getLLM)
	mux.HandleFunc("POST /api/llm", s.setLLM)
	mux.HandleFunc("POST /api/llm/test", s.testLLM)
	mux.HandleFunc("POST /api/llm/codex-config/inspect", s.inspectCodexConfig)
	return mux
}

func instructionTestRequest(t *testing.T, handler http.Handler, method, path string, body any) *httptest.ResponseRecorder {
	t.Helper()
	var raw []byte
	if body != nil {
		var err error
		raw, err = json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
	}
	r := httptest.NewRequest(method, path, bytes.NewReader(raw))
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, r)
	return w
}

func instructionDeveloperTexts(t *testing.T, body map[string]any) []string {
	t.Helper()
	var result []string
	input, _ := body["input"].([]any)
	for _, raw := range input {
		item, _ := raw.(map[string]any)
		if item["role"] != "developer" {
			continue
		}
		content, _ := item["content"].([]any)
		for _, rawBlock := range content {
			block, _ := rawBlock.(map[string]any)
			text, _ := block["text"].(string)
			result = append(result, text)
		}
	}
	return result
}

func TestModelInstructionsFileAPI(t *testing.T) {
	s := &Server{modelInstructionsDir: filepath.Join(t.TempDir(), "model-instructions")}
	handler := instructionTestMux(s)
	request := func(method, path string, body any, status int) *httptest.ResponseRecorder {
		t.Helper()
		w := instructionTestRequest(t, handler, method, path, body)
		if w.Code != status {
			t.Fatalf("%s %s status=%d, want %d: %s", method, path, w.Code, status, w.Body.String())
		}
		return w
	}
	rootPath := "/api/llm/instruction-files"
	listed := request("GET", rootPath, nil, 200)
	if !strings.Contains(listed.Body.String(), `"files":[]`) {
		t.Fatalf("initial list=%s", listed.Body.String())
	}
	const name = "基础-model.md"
	const original = "  Model instructions.\r\nLiteral {{.Goal}}. 🙂\r\n\r\n"
	created := request("POST", rootPath, map[string]any{"name": name, "content": "\ufeff" + original}, 201)
	var createResult struct {
		File modelInstructionsFileInfo `json:"file"`
	}
	if err := json.Unmarshal(created.Body.Bytes(), &createResult); err != nil {
		t.Fatal(err)
	}
	if createResult.File.Name != name || !createResult.File.Exists || createResult.File.Size != int64(len(original)) {
		t.Fatalf("create info=%+v", createResult.File)
	}
	if want := filepath.Join(s.modelInstructionsDir, name); createResult.File.Path != want {
		t.Fatalf("file path=%q want %q", createResult.File.Path, want)
	}
	request("POST", rootPath, map[string]any{"name": name, "content": "overwrite"}, 409)
	var readResult struct {
		Content string `json:"content"`
	}
	read := request("GET", rootPath+"/"+name, nil, 200)
	if err := json.Unmarshal(read.Body.Bytes(), &readResult); err != nil || readResult.Content != original {
		t.Fatalf("content=%q error=%v", readResult.Content, err)
	}
	const replacement = "New instructions.\n\n  whitespace retained.\n"
	request("PUT", rootPath+"/"+name, map[string]any{"content": replacement}, 200)
	data, err := os.ReadFile(createResult.File.Path)
	if err != nil || string(data) != replacement {
		t.Fatalf("saved=%q error=%v", data, err)
	}
	request("PUT", rootPath+"/missing.md", map[string]any{"content": "content"}, 404)
	request("GET", rootPath+"/missing.md", nil, 404)
	request("POST", rootPath, map[string]any{"name": "../outside.md", "content": "content"}, 400)
	request("POST", rootPath, map[string]any{"name": "bad.json", "content": "content"}, 400)
	request("POST", rootPath, map[string]any{"name": "empty.md", "content": " \r\n"}, 400)
	request("POST", rootPath, map[string]any{"name": "binary.md", "content": "a\x00b"}, 400)
	request("PUT", rootPath+"/"+name, map[string]any{"content": "\ufeff\n\n"}, 400)
	request("POST", rootPath, map[string]any{"name": "huge.md", "content": strings.Repeat("x", maxModelInstructionsSize+1)}, 400)
	// Failed edits leave the previous complete content intact.
	data, err = os.ReadFile(createResult.File.Path)
	if err != nil || string(data) != replacement {
		t.Fatalf("failed edit changed file=%q error=%v", data, err)
	}
	for _, raw := range [][]byte{
		[]byte(`{"name":"malformed.md","content":`),
		[]byte(`{"name":"malformed.md","content":"okay"} {}`),
		[]byte(`{"name":"malformed.md","content":"okay","path":"outside"}`),
		append([]byte(`{"name":"invalid.md","content":"`), []byte{0xff, '"', '}'}...),
	} {
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, httptest.NewRequest("POST", rootPath, bytes.NewReader(raw)))
		if w.Code != 400 {
			t.Fatalf("malformed request accepted: code=%d body=%s", w.Code, w.Body.String())
		}
	}
}

func TestModelInstructionsStoreRejectsUnsafeFiles(t *testing.T) {
	dir := t.TempDir()
	store := modelInstructionsStore{dir: dir}
	outside := filepath.Join(t.TempDir(), "outside.md")
	if err := os.WriteFile(outside, []byte("external content"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(dir, "symlink.md")); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(dir, "directory.txt"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "invalid.md"), []byte{0xff}, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "empty.md"), []byte("\n  \r\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "ignore.json"), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"symlink.md", "directory.txt", "invalid.md", "empty.md"} {
		if _, _, err := store.read(name); !errors.Is(err, errInvalidModelInstructions) {
			t.Fatalf("read %s error=%v", name, err)
		}
	}
	for _, name := range []string{"symlink.md", "directory.txt"} {
		if _, err := store.write(name, []byte("content"), false); !errors.Is(err, errInvalidModelInstructions) {
			t.Fatalf("update %s error=%v", name, err)
		}
		if err := store.remove(name); !errors.Is(err, errInvalidModelInstructions) {
			t.Fatalf("remove %s error=%v", name, err)
		}
	}
	files, err := store.list()
	if err != nil || len(files) != 4 {
		t.Fatalf("list=%+v error=%v", files, err)
	}
	for _, file := range files {
		if !file.Exists || file.Error == "" {
			t.Fatalf("invalid file status=%+v", file)
		}
	}
	if err := store.remove("missing.md"); err != nil {
		t.Fatal(err)
	}
	unsafeDir := filepath.Join(t.TempDir(), "model-instructions")
	if err := os.Symlink(dir, unsafeDir); err != nil {
		t.Fatal(err)
	}
	if _, err := (modelInstructionsStore{dir: unsafeDir}).write("new.md", []byte("content"), true); !errors.Is(err, errInvalidModelInstructions) {
		t.Fatalf("symlink managed directory accepted: %v", err)
	}
}

func TestModelInstructionsProfileValidation(t *testing.T) {
	s := &Server{modelInstructionsDir: t.TempDir()}
	if _, err := s.instructionStore().write("registered.md", []byte("Fixture model instructions."), true); err != nil {
		t.Fatal(err)
	}
	p := &db.LLMProfile{Format: "openai-responses", UserAgent: "codex", ModelInstructionsFile: "registered.md", ModelInstructionsFiles: []string{"registered.md"}}
	if err := s.validateProfileModelInstructions(p); err != nil {
		t.Fatal(err)
	}
	p.ModelInstructionsFiles = []string{}
	if err := s.validateProfileModelInstructions(p); err == nil {
		t.Fatal("enabled unregistered file was accepted")
	}
	p.ModelInstructionsFiles = []string{"missing.md"}
	p.ModelInstructionsFile = "missing.md"
	if err := s.validateProfileModelInstructions(p); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("enabled missing file error=%v", err)
	}
	p.Format = "anthropic"
	if err := s.validateProfileModelInstructions(p); err != nil || p.ModelInstructionsFile != "" || len(p.ModelInstructionsFiles) != 1 {
		t.Fatalf("other format did not disable while preserving list: %+v err=%v", p, err)
	}
	p.ModelInstructionsFiles = []string{"../outside.md"}
	if err := s.validateProfileModelInstructions(p); err == nil {
		t.Fatal("unsafe registered filename accepted")
	}
}

func instructionTestDB(t *testing.T) *db.DB {
	t.Helper()
	dsn := os.Getenv("ARTEX_INSTRUCTIONS_TEST_DSN")
	if dsn == "" {
		t.Skip("set ARTEX_INSTRUCTIONS_TEST_DSN to an isolated instruction test database")
	}
	u, err := url.Parse(dsn)
	if err != nil || !strings.HasPrefix(strings.TrimPrefix(u.Path, "/"), "artex_instructions_test_") {
		t.Fatal("instruction integration tests require a database named artex_instructions_test_*")
	}
	pg, err := db.Open(dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { pg.Close() })
	return pg
}

func TestModelInstructionsSavedProfileAndDelete(t *testing.T) {
	pg := instructionTestDB(t)
	s := &Server{m: &Manager{pg: pg}, modelInstructionsDir: t.TempDir()}
	handler := instructionTestMux(s)
	store := s.instructionStore()
	if _, err := store.write("enabled.md", []byte("Fixture model instructions."), true); err != nil {
		t.Fatal(err)
	}
	profile := map[string]any{"name": "fixture-profile", "format": "openai-responses", "model": "fixture-model", "user_agent": "codex", "model_instructions_file": "enabled.md", "model_instructions_files": []string{"enabled.md"}}
	w := instructionTestRequest(t, handler, "POST", "/api/llm/profiles", profile)
	if w.Code != 200 {
		t.Fatalf("save: %d %s", w.Code, w.Body.String())
	}
	var result struct {
		ID int64 `json:"id"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = pg.Exec(`DELETE FROM llm_profiles WHERE id=$1`, result.ID) })
	p, err := pg.ProfileByID(result.ID)
	if err != nil || p == nil || p.ModelInstructionsFile != "enabled.md" || len(p.ModelInstructionsFiles) != 1 {
		t.Fatalf("roundtrip profile=%+v error=%v", p, err)
	}
	profile["id"] = result.ID
	delete(profile, "model_instructions_file")
	delete(profile, "model_instructions_files")
	w = instructionTestRequest(t, handler, "POST", "/api/llm/profiles", profile)
	if w.Code != 200 {
		t.Fatalf("legacy profile edit: %d %s", w.Code, w.Body.String())
	}
	p, err = pg.ProfileByID(result.ID)
	if err != nil || p.ModelInstructionsFile != "enabled.md" || len(p.ModelInstructionsFiles) != 1 {
		t.Fatalf("legacy omitted fields were cleared: %+v error=%v", p, err)
	}
	w = instructionTestRequest(t, handler, "DELETE", "/api/llm/instruction-files/enabled.md", nil)
	if w.Code != 409 || !strings.Contains(w.Body.String(), "fixture-profile") || !strings.Contains(w.Body.String(), "停用") {
		t.Fatalf("enabled file deletion: %d %s", w.Code, w.Body.String())
	}
	profile["model_instructions_file"] = ""
	w = instructionTestRequest(t, handler, "POST", "/api/llm/profiles", profile)
	if w.Code != 200 {
		t.Fatalf("disable: %d %s", w.Code, w.Body.String())
	}
	w = instructionTestRequest(t, handler, "DELETE", "/api/llm/instruction-files/enabled.md", nil)
	if w.Code != 200 {
		t.Fatalf("delete registered inactive file: %d %s", w.Code, w.Body.String())
	}
	w = instructionTestRequest(t, handler, "DELETE", "/api/llm/instruction-files/enabled.md", nil)
	if w.Code != 200 {
		t.Fatalf("delete missing inactive file: %d %s", w.Code, w.Body.String())
	}
	profile["model_instructions_file"] = "enabled.md"
	w = instructionTestRequest(t, handler, "POST", "/api/llm/profiles", profile)
	if w.Code != 404 {
		t.Fatalf("enable deleted file: %d %s", w.Code, w.Body.String())
	}
	profile["format"] = "anthropic"
	w = instructionTestRequest(t, handler, "POST", "/api/llm/profiles", profile)
	if w.Code != 200 {
		t.Fatalf("switch format: %d %s", w.Code, w.Body.String())
	}
	p, err = pg.ProfileByID(result.ID)
	if err != nil || p.ModelInstructionsFile != "" || len(p.ModelInstructionsFiles) != 1 {
		t.Fatalf("format switch lost list: %+v error=%v", p, err)
	}
	// A same-named file can be recreated after removal without reviving the old
	// selection. Its new selection is an explicit profile edit.
	if _, err := store.write("enabled.md", []byte("Recreated fixture."), true); err != nil {
		t.Fatal(err)
	}
	profile["format"] = "openai-responses"
	profile["user_agent"] = ""
	w = instructionTestRequest(t, handler, "POST", "/api/llm/profiles", profile)
	if w.Code != 200 {
		t.Fatalf("switch UA: %d %s", w.Code, w.Body.String())
	}
	p, err = pg.ProfileByID(result.ID)
	if err != nil || p.ModelInstructionsFile != "" || len(p.ModelInstructionsFiles) != 1 {
		t.Fatalf("UA switch lost list: %+v error=%v", p, err)
	}
}

func TestModelInstructionsTestRequestAndRuntime(t *testing.T) {
	pg := instructionTestDB(t)
	s := &Server{m: &Manager{pg: pg, dir: t.TempDir()}, modelInstructionsDir: t.TempDir()}
	const base = "Model fixture only.\nKeep {{.Literal}} unchanged.\n"
	if _, err := s.instructionStore().write("base.md", []byte(base), true); err != nil {
		t.Fatal(err)
	}
	wires := make(chan map[string]any, 3)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		wires <- body
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "event: response.output_text.delta\ndata: {\"delta\":\"OK\"}\n\nevent: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\",\"usage\":{\"input_tokens\":1,\"output_tokens\":1}}}\n\n")
	}))
	defer upstream.Close()
	id, err := pg.SaveProfile(&db.LLMProfile{Name: "test-connection", Format: "openai-responses", Model: "fixture-model", BaseURL: upstream.URL + "/v1", APIKey: "fixture-key", UserAgent: "codex", Streaming: true, ModelInstructionsFile: "base.md", ModelInstructionsFiles: []string{"base.md"}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = pg.Exec(`DELETE FROM llm_profiles WHERE id=$1`, id) })
	cfg, ok := s.loadProfileConfig(id)
	if !ok || cfg.ModelInstructionsFile != filepath.Join(s.modelInstructionsDir, "base.md") {
		t.Fatalf("resolved config=%+v okay=%v", cfg, ok)
	}
	handler := instructionTestMux(s)
	testBody := map[string]any{"provider": "openai-responses", "model": "fixture-model", "base_url": upstream.URL + "/v1", "profile_id": id, "streaming": false}
	for _, enabled := range []bool{true, false} {
		if !enabled {
			testBody["model_instructions_file"] = ""
		}
		w := instructionTestRequest(t, handler, "POST", "/api/llm/test", testBody)
		if w.Code != 200 || !strings.Contains(w.Body.String(), `"ok":true`) {
			t.Fatalf("connection: %d %s", w.Code, w.Body.String())
		}
		wire := <-wires
		instructions, _ := wire["instructions"].(string)
		if enabled {
			texts := instructionDeveloperTexts(t, wire)
			if instructions != "" || len(texts) != 2 || texts[0] != base || !strings.Contains(texts[1], "你是连接测试") {
				t.Fatalf("test enabled developer texts=%q instructions=%q", texts, instructions)
			}
		} else if strings.Contains(instructions, base) || !strings.Contains(instructions, "你是连接测试") {
			t.Fatalf("test disabled instructions=%q", instructions)
		}
	}
	s.invalidateProfileAgents()
	provider, _, ok := s.providerForProfile(id)
	if !ok {
		t.Fatal("saved model profile provider unavailable")
	}
	req := llm.CompletionRequest{System: []string{"Agent fixture purpose."}, Messages: []llm.Message{llm.UserText("Summarize fixture")}}
	if _, _, _, err := provider.Complete(t.Context(), req); err != nil {
		t.Fatal(err)
	}
	if texts := instructionDeveloperTexts(t, <-wires); len(texts) != 2 || texts[0] != base || texts[1] != "Agent fixture purpose." {
		t.Fatalf("runtime developer instructions=%q", texts)
	}
	const replacement = "Updated literal {{.Model}} base.\n"
	w := instructionTestRequest(t, handler, "PUT", "/api/llm/instruction-files/base.md", map[string]any{"content": replacement})
	if w.Code != 200 {
		t.Fatalf("runtime file edit: %d %s", w.Code, w.Body.String())
	}
	provider, _, ok = s.providerForProfile(id)
	if !ok {
		t.Fatal("updated model profile provider unavailable")
	}
	if _, _, _, err := provider.Complete(t.Context(), req); err != nil {
		t.Fatal(err)
	}
	if texts := instructionDeveloperTexts(t, <-wires); len(texts) != 2 || texts[0] != replacement || texts[1] != "Agent fixture purpose." {
		t.Fatalf("updated runtime developer instructions=%q", texts)
	}
	if err := s.instructionStore().remove("base.md"); err != nil {
		t.Fatal(err)
	}
	cfg, ok = s.loadProfileConfig(id)
	if !ok || cfg.ModelInstructionsFile == "" {
		t.Fatal("missing enabled file silently disabled the profile")
	}
	if _, err := cfg.NewProvider(); err == nil {
		t.Fatal("missing enabled file silently used default instructions")
	}
	delete(testBody, "model_instructions_file")
	w = instructionTestRequest(t, handler, "POST", "/api/llm/test", testBody)
	if w.Code != 400 {
		t.Fatalf("test missing enabled file: %d %s", w.Code, w.Body.String())
	}
	if len(wires) != 0 {
		t.Fatal("bad file test sent an upstream request")
	}
}

func TestModelInstructionsLegacyConfig(t *testing.T) {
	pg := instructionTestDB(t)
	s := &Server{m: &Manager{pg: pg, dir: t.TempDir()}, modelInstructionsDir: t.TempDir()}
	if _, err := s.instructionStore().write("legacy.md", []byte("Legacy fixture."), true); err != nil {
		t.Fatal(err)
	}
	if _, err := pg.Exec(`UPDATE llm_profiles SET is_default=false`); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = pg.Exec(`DELETE FROM llm_profiles WHERE name='default'`) })
	handler := instructionTestMux(s)
	body := map[string]any{"provider": "openai-responses", "model": "fixture-model", "base_url": "https://example.invalid/v1", "api_key": "fixture-key", "user_agent": "codex", "model_instructions_file": "legacy.md", "model_instructions_files": []string{"legacy.md"}}
	check := func(wantName string) {
		t.Helper()
		w := instructionTestRequest(t, handler, "POST", "/api/llm", body)
		if w.Code != 200 {
			t.Fatalf("legacy save: %d %s", w.Code, w.Body.String())
		}
		var saved struct {
			Name  string   `json:"model_instructions_file"`
			Files []string `json:"model_instructions_files"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &saved); err != nil || saved.Name != wantName || len(saved.Files) != 1 {
			t.Fatalf("legacy response=%s error=%v", w.Body.String(), err)
		}
		p, err := pg.ActiveProfile()
		if err != nil || p == nil || p.ModelInstructionsFile != wantName || len(p.ModelInstructionsFiles) != 1 {
			t.Fatalf("legacy database profile=%+v error=%v", p, err)
		}
	}
	check("legacy.md")
	delete(body, "model_instructions_file")
	delete(body, "model_instructions_files")
	check("legacy.md")
	body["model_instructions_file"] = ""
	check("")
}

func instructionCodexFixture(t *testing.T) (string, string, string, string) {
	t.Helper()
	dir := t.TempDir()
	modelPath := filepath.Join(dir, "base.md")
	configPath := filepath.Join(dir, "config.toml")
	const base = "Codex model fixture.\nPreserve {{.Literal}}. 🙂\n"
	const developer = "Developer fixture.\nUse the supplied logs only."
	if err := os.WriteFile(modelPath, []byte(base), 0o600); err != nil {
		t.Fatal(err)
	}
	text := fmt.Sprintf("model_instructions_file = 'base.md'\ndeveloper_instructions = '''%s'''\nmodel = 'DO-NOT-USE-THIS-MODEL'\n[model_providers.secret]\napi_key = 'DO-NOT-RETURN-THIS-KEY'\n", developer)
	if err := os.WriteFile(configPath, []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
	return configPath, modelPath, base, developer
}

func TestModelInstructionsCodexConfigInspect(t *testing.T) {
	configPath, modelPath, base, developer := instructionCodexFixture(t)
	handler := instructionTestMux(&Server{})
	inspect := func(path string) *httptest.ResponseRecorder {
		return instructionTestRequest(t, handler, "POST", "/api/llm/codex-config/inspect", map[string]string{"path": path})
	}
	w := inspect(configPath)
	if w.Code != 200 {
		t.Fatalf("inspect: %d %s", w.Code, w.Body.String())
	}
	var result codexConfigInspection
	if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil || result.ConfigPath != configPath || result.ModelInstructionsFile != modelPath || result.ModelInstructionsBytes != len(base) || result.DeveloperInstructionsBytes != len(developer) || !result.HasModelInstructions {
		t.Fatalf("inspect result=%+v error=%v", result, err)
	}
	for _, forbidden := range []string{"DO-NOT-RETURN-THIS-KEY", "DO-NOT-USE-THIS-MODEL", base, developer} {
		if strings.Contains(w.Body.String(), forbidden) {
			t.Fatalf("inspect exposed private text %q", forbidden)
		}
	}
	if err := os.WriteFile(configPath, []byte("developer_instructions = 'Only developer text'\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	w = inspect(configPath)
	if w.Code != 200 {
		t.Fatalf("developer-only config rejected: %d %s", w.Code, w.Body.String())
	}
	if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil || result.HasModelInstructions || result.ModelInstructionsBytes != 0 || result.DeveloperInstructionsBytes != len("Only developer text") {
		t.Fatalf("developer-only inspection=%+v err=%v", result, err)
	}
	if err := os.WriteFile(configPath, []byte("api_key = 'DO-NOT-RETURN-THIS-KEY'\nbad = [\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	w = inspect(configPath)
	if w.Code != 400 || strings.Contains(w.Body.String(), "DO-NOT-RETURN-THIS-KEY") {
		t.Fatalf("invalid TOML result=%d %s", w.Code, w.Body.String())
	}
	w = inspect(filepath.Join(t.TempDir(), "missing.toml"))
	if w.Code != 400 {
		t.Fatalf("missing config result=%d %s", w.Code, w.Body.String())
	}
}

func TestModelInstructionsCodexConfigProfile(t *testing.T) {
	pg := instructionTestDB(t)
	configPath, modelPath, base, developer := instructionCodexFixture(t)
	s := &Server{m: &Manager{pg: pg, dir: t.TempDir()}, modelInstructionsDir: t.TempDir()}
	wires := make(chan map[string]any, 2)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		wires <- body
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "event: response.output_text.delta\ndata: {\"delta\":\"OK\"}\n\nevent: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\",\"usage\":{\"input_tokens\":1,\"output_tokens\":1}}}\n\n")
	}))
	defer upstream.Close()
	handler := instructionTestMux(s)
	body := map[string]any{"name": "codex-config-profile", "format": "openai-responses", "model": "fixture-model", "base_url": upstream.URL + "/v1", "api_key": "fixture-key", "user_agent": "codex", "codex_config_file": configPath}
	w := instructionTestRequest(t, handler, "POST", "/api/llm/profiles", body)
	if w.Code != 200 {
		t.Fatalf("save Codex config: %d %s", w.Code, w.Body.String())
	}
	var saved struct {
		ID int64 `json:"id"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &saved); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = pg.Exec(`DELETE FROM llm_profiles WHERE id=$1`, saved.ID) })
	p, err := pg.ProfileByID(saved.ID)
	if err != nil || p == nil || p.CodexConfigFile != configPath {
		t.Fatalf("stored path=%+v error=%v", p, err)
	}
	body["id"] = saved.ID
	delete(body, "codex_config_file")
	delete(body, "api_key")
	w = instructionTestRequest(t, handler, "POST", "/api/llm/profiles", body)
	if w.Code != 200 {
		t.Fatalf("partial edit: %d %s", w.Code, w.Body.String())
	}
	p, err = pg.ProfileByID(saved.ID)
	if err != nil || p.CodexConfigFile != configPath {
		t.Fatalf("omitted path was lost: %+v error=%v", p, err)
	}
	testBody := map[string]any{"provider": "openai-responses", "model": "fixture-model", "base_url": upstream.URL + "/v1", "profile_id": saved.ID, "streaming": false}
	w = instructionTestRequest(t, handler, "POST", "/api/llm/test", testBody)
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"ok":true`) {
		t.Fatalf("test stored Codex config: %d %s", w.Code, w.Body.String())
	}
	wire := <-wires
	texts := instructionDeveloperTexts(t, wire)
	if _, hasLegacy := wire["instructions"]; hasLegacy || wire["model"] != "fixture-model" || len(texts) != 3 || texts[0] != base || texts[1] != developer || !strings.Contains(texts[2], "你是连接测试") {
		t.Fatalf("Codex test developer text=%q model=%s legacy=%v", texts, wire["model"], hasLegacy)
	}
	testBody["codex_config_file"] = ""
	w = instructionTestRequest(t, handler, "POST", "/api/llm/test", testBody)
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"ok":true`) {
		t.Fatalf("test explicitly disabled Codex config: %d %s", w.Code, w.Body.String())
	}
	wire = <-wires
	if _, hasLegacy := wire["instructions"]; !hasLegacy || len(instructionDeveloperTexts(t, wire)) != 0 {
		t.Fatal("explicit empty test path inherited saved Codex config")
	}
	if _, err := s.instructionStore().write("manual.md", []byte("Manual fixture."), true); err != nil {
		t.Fatal(err)
	}
	body["model_instructions_file"] = "manual.md"
	body["model_instructions_files"] = []string{"manual.md"}
	w = instructionTestRequest(t, handler, "POST", "/api/llm/profiles", body)
	if w.Code != 400 {
		t.Fatalf("mutually exclusive sources accepted: %d %s", w.Code, w.Body.String())
	}
	delete(body, "model_instructions_file")
	delete(body, "model_instructions_files")
	// Invalid external edits keep the profile selected and produce a local error
	// provider, rather than falling back to any global provider.
	if err := os.Remove(modelPath); err != nil {
		t.Fatal(err)
	}
	s.invalidateProfileAgents()
	provider, cfg, ok := s.providerForProfile(saved.ID)
	if !ok || provider == nil || cfg.ConfigError == nil {
		t.Fatalf("missing file lost profile selection: okay=%v provider=%v error=%v", ok, provider, cfg.ConfigError)
	}
	if _, _, _, err := provider.Complete(t.Context(), llm.CompletionRequest{}); err == nil || !strings.Contains(err.Error(), "base.md") {
		t.Fatalf("error provider result=%v", err)
	}
	for _, err := range provider.Stream(t.Context(), llm.CompletionRequest{}) {
		if err == nil {
			t.Fatal("error provider stream returned success")
		}
	}
	if len(wires) != 0 {
		t.Fatal("local error sent request upstream")
	}
	// Even opt-in provider failover must not conceal a local instruction error.
	if err := s.m.SetLLMPoolEnabled(true); err != nil {
		t.Fatal(err)
	}
	if err := s.m.SetLLMPoolBindFallback(true); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = s.m.SetLLMPoolEnabled(false)
		_ = s.m.SetLLMPoolBindFallback(false)
	})
	backup, err := pg.SaveProfile(&db.LLMProfile{Name: "codex-config-backup", Format: "openai-responses", Model: "fixture-model", BaseURL: upstream.URL + "/v1", APIKey: "fixture-key", UserAgent: "codex"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = pg.Exec(`DELETE FROM llm_profiles WHERE id=$1`, backup) })
	for _, selected := range []llm.Provider{s.poolForBinding(saved.ID, provider, cfg), s.poolForActive(saved.ID, provider, cfg)} {
		if _, _, _, err := selected.Complete(t.Context(), llm.CompletionRequest{}); err == nil {
			t.Fatal("configuration error silently used another pool provider")
		}
	}
	if len(wires) != 0 {
		t.Fatal("configuration error tried a fallback upstream")
	}
	previousActive, err := pg.ActiveProfile()
	if err != nil {
		t.Fatal(err)
	}
	if err := pg.SetActiveProfile(saved.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.applyLLM(cfg); err != nil {
		t.Fatalf("cannot install explicit configuration error provider: %v", err)
	}
	global, globalCfg, ready := s.globalProvider()
	if !ready || globalCfg.ConfigError == nil {
		t.Fatal("global provider retained previous valid source after broken source selected")
	}
	if _, _, _, err := global.Complete(t.Context(), llm.CompletionRequest{}); err == nil {
		t.Fatal("global provider concealed instruction source error")
	}
	if previousActive == nil {
		_, err = pg.Exec(`UPDATE llm_profiles SET is_default=false WHERE id=$1`, saved.ID)
	} else {
		err = pg.SetActiveProfile(previousActive.ID)
	}
	if err != nil {
		t.Fatal(err)
	}
	_ = s.m.SetLLMPoolEnabled(false)
	_ = s.m.SetLLMPoolBindFallback(false)
	body["codex_config_file"] = ""
	w = instructionTestRequest(t, handler, "POST", "/api/llm/profiles", body)
	if w.Code != 200 {
		t.Fatalf("explicit disable with missing source: %d %s", w.Code, w.Body.String())
	}
	p, err = pg.ProfileByID(saved.ID)
	if err != nil || p.CodexConfigFile != "" {
		t.Fatalf("explicit disable did not persist: %+v error=%v", p, err)
	}
	body["codex_config_file"] = configPath
	body["format"] = "anthropic"
	w = instructionTestRequest(t, handler, "POST", "/api/llm/profiles", body)
	if w.Code != 200 {
		t.Fatalf("format change failed to disable: %d %s", w.Code, w.Body.String())
	}
	p, err = pg.ProfileByID(saved.ID)
	if err != nil || p.CodexConfigFile != "" {
		t.Fatalf("format change retained Codex path: %+v error=%v", p, err)
	}
}

func TestModelInstructionsSideIdentity(t *testing.T) {
	s := &Server{modelInstructionsDir: t.TempDir()}
	info, err := s.instructionStore().write("base.md", []byte("First base."), true)
	if err != nil {
		t.Fatal(err)
	}
	cfg := agent.ConfigFrom("openai-responses", "fixture", "https://example.invalid/v1", "fixture-key", "")
	cfg.UserAgent, cfg.ModelInstructionsFile = "codex", info.Path
	first := sideModel(cfg, 42, "fixture")
	if _, err := s.instructionStore().write("base.md", []byte("Second base."), false); err != nil {
		t.Fatal(err)
	}
	second := sideModel(cfg, 42, "fixture")
	if first.Identity == second.Identity {
		t.Fatal("editing instruction content reused the previous side question identity")
	}
	cfg.ModelInstructionsFile = ""
	disabled := sideModel(cfg, 42, "fixture")
	if disabled.Identity == first.Identity || disabled.Identity == second.Identity {
		t.Fatal("disabling instructions reused the enabled identity")
	}
	configPath, modelPath, _, _ := instructionCodexFixture(t)
	cfg.CodexConfigFile = configPath
	fromConfig := sideModel(cfg, 42, "fixture")
	if err := os.WriteFile(modelPath, []byte("Updated Codex source."), 0o600); err != nil {
		t.Fatal(err)
	}
	updatedConfig := sideModel(cfg, 42, "fixture")
	if fromConfig.Identity == updatedConfig.Identity || fromConfig.Identity == disabled.Identity {
		t.Fatal("Codex config model file edits did not change side question identity")
	}
}
