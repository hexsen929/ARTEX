package agent

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/Autumn-27/norma/llm"
)

func modelInstructionsMessage(role, kind, text string) map[string]any {
	return map[string]any{
		"type": "message", "role": role,
		"content": []map[string]string{{"type": kind, "text": text}},
	}
}

func assertModelInstructionsJSON(t *testing.T, got, want any) {
	t.Helper()
	raw, err := json.Marshal(want)
	if err != nil {
		t.Fatal(err)
	}
	var normalized any
	if err := json.Unmarshal(raw, &normalized); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, normalized) {
		t.Fatalf("request items or instruction layer order changed: got=%v want=%v", got, normalized)
	}
}

// A model file replaces the client preamble, not the Agent's independently
// rendered prompt. Provider rebuilds are the boundary for observing file edits.
func TestModelInstructionsProviderReloadAndDisable(t *testing.T) {
	const originalBase = "  Model fixture 基础说明。\r\nLiteral {{.Goal}} stays literal. 🙂\r\n"
	const updatedBase = "Updated model fixture.\nKeep {{if .Flag}} as text.\n"
	path := filepath.Join(t.TempDir(), "model-base.md")
	if err := os.WriteFile(path, append([]byte{0xef, 0xbb, 0xbf}, []byte(originalBase)...), 0600); err != nil {
		t.Fatal(err)
	}
	wires := make(chan codexWireRequest, 4)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		wire := codexWireRequest{path: r.URL.Path, headers: r.Header.Clone()}
		wire.err = json.NewDecoder(r.Body).Decode(&wire.body)
		wires <- wire
		w.Header().Set("Content-Type", "text/event-stream")
		writeCodexTestFrame(w, "response.output_text.delta", map[string]any{"delta": "OK"})
		writeCodexTestCompleted(w)
	}))
	defer upstream.Close()
	cfg := ConfigFrom("openai-responses", "test-model", upstream.URL+"/v1", "fixture-key", "")
	cfg.UserAgent, cfg.ModelInstructionsFile = "codex", path
	provider, err := cfg.NewProvider()
	if err != nil {
		t.Fatal(err)
	}
	req := llm.CompletionRequest{
		System:   []string{"ARTEX Agent role: summarize only supplied logs.", "已渲染的目标与产物规范，原样保留。\n"},
		Messages: []llm.Message{llm.UserText("Summarize the fixture log")},
	}
	// Norma already trims its system segments before they reach the transport.
	const agentPrompt = "ARTEX Agent role: summarize only supplied logs.\n\n已渲染的目标与产物规范，原样保留。"
	check := func(p llm.Provider, wantBase string, layered, stream bool) {
		t.Helper()
		var message llm.Message
		if stream {
			acc := llm.NewAccumulator()
			for event, err := range p.Stream(t.Context(), req) {
				if err != nil {
					t.Fatalf("stream: %v", err)
				}
				acc.Add(event)
			}
			message = acc.Message()
		} else {
			var err error
			message, _, _, err = p.Complete(t.Context(), req)
			if err != nil {
				t.Fatalf("complete: %v", err)
			}
		}
		if message.Text() != "OK" {
			t.Fatalf("reply=%q, want OK", message.Text())
		}
		wire := <-wires
		if wire.err != nil {
			t.Fatal(wire.err)
		}
		user := modelInstructionsMessage("user", "input_text", "Summarize the fixture log")
		if layered {
			if _, exists := wire.body["instructions"]; exists {
				t.Fatal("layered model instructions must not share the top-level instructions field")
			}
			wantInput := []map[string]any{
				modelInstructionsMessage("developer", "input_text", wantBase),
				modelInstructionsMessage("developer", "input_text", agentPrompt),
				user,
			}
			assertModelInstructionsJSON(t, wire.body["input"], wantInput)
		} else {
			want := codexInstructions + "\n\n" + agentPrompt
			if wire.body["instructions"] != want {
				t.Fatalf("disabled file did not restore legacy Codex instructions: got %q, want %q", wire.body["instructions"], want)
			}
			assertModelInstructionsJSON(t, wire.body["input"], []map[string]any{user})
		}
		if wire.path != "/v1/responses" || wire.headers.Get("originator") != "codex_cli_rs" || wire.body["stream"] != true {
			t.Fatal("model file changed Codex endpoint, client identity, or streaming mode")
		}
	}
	check(provider, originalBase, true, false)
	if err := os.WriteFile(path, []byte(updatedBase), 0600); err != nil {
		t.Fatal(err)
	}
	// Existing instances remain coherent until the server invalidates/rebuilds them.
	check(provider, originalBase, true, false)
	rebuilt, err := cfg.NewProvider()
	if err != nil {
		t.Fatal(err)
	}
	check(rebuilt, updatedBase, true, true)
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	cfg.ModelInstructionsFile = ""
	disabled, err := cfg.NewProvider()
	if err != nil {
		t.Fatalf("disabled model file must not read the deleted file: %v", err)
	}
	check(disabled, codexInstructions, false, false)
}

func TestModelInstructionsFilesRejectInvalidInputsBeforeRequest(t *testing.T) {
	var requests atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer upstream.Close()

	for _, tc := range []struct {
		name, wantError string
		content         []byte
		kind            string
	}{
		{name: "invalid_utf8", content: []byte{0xff}, wantError: "UTF-8"},
		{name: "binary_NUL", content: []byte("literal\x00text"), wantError: "UTF-8"},
		{name: "empty", content: []byte{}, wantError: "empty"},
		{name: "BOM_only", content: []byte{0xef, 0xbb, 0xbf}, wantError: "empty"},
		{name: "whitespace", content: []byte(" \r\n\t"), wantError: "empty"},
		{name: "missing", kind: "missing", wantError: "model instructions file"},
		{name: "directory", kind: "directory", wantError: "regular file"},
		{name: "symlink", kind: "symlink", wantError: "regular file"},
		{name: "oversize", content: bytes.Repeat([]byte("a"), 1024*1024+1), wantError: "exceeds 1 MiB"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			path := filepath.Join(root, "model-base.md")
			switch tc.kind {
			case "missing":
			case "directory":
				path = root
			case "symlink":
				target := filepath.Join(root, "fixture-target.md")
				if err := os.WriteFile(target, []byte("Model fixture"), 0600); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(target, path); err != nil {
					t.Fatalf("create test symlink: %v", err)
				}
			default:
				if err := os.WriteFile(path, tc.content, 0600); err != nil {
					t.Fatal(err)
				}
			}
			cfg := ConfigFrom("openai-responses", "test-model", upstream.URL, "fixture-key", "")
			cfg.UserAgent, cfg.ModelInstructionsFile = "codex", path
			provider, err := cfg.NewProvider()
			if err == nil || provider != nil || !strings.Contains(err.Error(), tc.wantError) {
				t.Fatalf("invalid file did not fail provider construction with %q: provider=%T err=%v", tc.wantError, provider, err)
			}
			if requests.Load() != 0 {
				t.Fatal("invalid local model file reached the network")
			}
		})
	}
}

func TestModelInstructionsFileSizeBoundaryAndProtocolGate(t *testing.T) {
	path := filepath.Join(t.TempDir(), "model-base.txt")
	if err := os.WriteFile(path, bytes.Repeat([]byte("a"), 1024*1024), 0600); err != nil {
		t.Fatal(err)
	}
	cfg := ConfigFrom("openai-responses", "test-model", "http://127.0.0.1:1", "fixture-key", "")
	cfg.UserAgent, cfg.ModelInstructionsFile = "codex", path
	if _, err := cfg.NewProvider(); err != nil {
		t.Fatalf("a valid file of exactly 1 MiB should be accepted: %v", err)
	}
	for _, tc := range []struct{ format, ua string }{
		{format: "openai-responses"},
		{format: "openai-responses", ua: "claude-code"},
		{format: "openai-responses", ua: "codex_cli_rs/0.162.1"},
		{format: "openai", ua: "codex"},
		{format: "anthropic", ua: "codex"},
	} {
		t.Run(tc.format+"/"+tc.ua, func(t *testing.T) {
			other := ConfigFrom(tc.format, "test-model", "http://127.0.0.1:1", "fixture-key", "")
			other.UserAgent, other.ModelInstructionsFile = tc.ua, path
			if _, err := other.NewProvider(); err == nil || !strings.Contains(err.Error(), "requires OpenAI Responses with the Codex preset") {
				t.Fatalf("model file was accepted outside the explicit Responses Codex preset: %v", err)
			}
		})
	}
}

// Only the selected TOML's top-level instruction fields enter the request.
// Provider/model/credential tables remain independent ARTEX profile settings.
func TestCodexConfigInstructionsWireLayers(t *testing.T) {
	const modelBase = "  Model base 基础说明 {{literal.variable}}\r\n"
	const developer = "Developer fixture line 1.\nKeep {{.Goal}} literal.\n"
	const agentPrompt = "Rendered ARTEX Agent role.\n\nKeep the task result and tool workflow."
	root := t.TempDir()
	modelPath := filepath.Join(root, "base.md")
	if err := os.WriteFile(modelPath, append([]byte{0xef, 0xbb, 0xbf}, []byte(modelBase)...), 0600); err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(root, "config.toml")
	for _, tc := range []struct {
		name, config, base, developer string
	}{
		{
			name: "base_and_developer", base: modelBase, developer: developer,
			config: `model_instructions_file = "base.md"
developer_instructions = """
Developer fixture line 1.
Keep {{.Goal}} literal.
"""
model = "fixture-wrong-model"
model_provider = "unrelated"
[model_providers.unrelated]
base_url = "http://127.0.0.1:1/do-not-send"
experimental_bearer_token = "fixture-unused-secret"
model_instructions_file = "missing-nested.md"
developer_instructions = "must-not-use-nested-provider-text"
[profiles.other]
developer_instructions = "must-not-use-profile-text"
`,
		},
		{
			name: "developer_only", base: codexInstructions, developer: "Only developer 文本 {{.Goal}}",
			config: "developer_instructions = 'Only developer 文本 {{.Goal}}'\n",
		},
		{
			name: "base_only", base: modelBase,
			config: "model_instructions_file = 'base.md'\n",
		},
		{
			name: "no_top_level_instructions", base: codexInstructions,
			config: "[model_providers.custom]\nmodel_instructions_file = 'missing.md'\ndeveloper_instructions = 'must-not-use-nested-text'\n",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := os.WriteFile(configPath, []byte(tc.config), 0600); err != nil {
				t.Fatal(err)
			}
			wires := make(chan codexWireRequest, 1)
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				wire := codexWireRequest{path: r.URL.Path, headers: r.Header.Clone()}
				wire.err = json.NewDecoder(r.Body).Decode(&wire.body)
				wires <- wire
				w.Header().Set("Content-Type", "text/event-stream")
				writeCodexTestFrame(w, "response.output_text.delta", map[string]any{"delta": "OK"})
				writeCodexTestCompleted(w)
			}))
			defer upstream.Close()
			cfg := ConfigFrom("openai-responses", "artex-fixture-model", upstream.URL+"/v1", "fixture-profile-key", "")
			cfg.UserAgent, cfg.CodexConfigFile = "codex", configPath
			provider, err := cfg.NewProvider()
			if err != nil {
				t.Fatal(err)
			}
			req := llm.CompletionRequest{
				System: []string{"Rendered ARTEX Agent role.", "Keep the task result and tool workflow."},
				Messages: []llm.Message{
					llm.UserText("Read fixture.txt"),
					{Role: llm.RoleAssistant, Content: []llm.ContentBlock{
						llm.TextBlock("Reading."),
						{Type: llm.BlockToolUse, ID: "call_fixture", Name: "read_file", Input: json.RawMessage(`{"path":"fixture.txt"}`)},
					}},
					{Role: llm.RoleUser, Content: []llm.ContentBlock{llm.ToolResultText("call_fixture", "file contents", false)}},
				},
			}
			message, _, _, err := provider.Complete(t.Context(), req)
			if err != nil || message.Text() != "OK" {
				t.Fatalf("completion failed: text=%q err=%v", message.Text(), err)
			}
			wire := <-wires
			if wire.err != nil {
				t.Fatal(wire.err)
			}
			if wire.path != "/v1/responses" || wire.body["model"] != "artex-fixture-model" || wire.headers.Get("Authorization") != "Bearer fixture-profile-key" {
				t.Fatal("Codex instruction import altered the ARTEX endpoint, model, or credentials")
			}
			if _, exists := wire.body["instructions"]; exists {
				t.Fatal("Codex config layers must use developer messages rather than top-level instructions")
			}
			wantInput := []map[string]any{modelInstructionsMessage("developer", "input_text", tc.base)}
			if tc.developer != "" {
				wantInput = append(wantInput, modelInstructionsMessage("developer", "input_text", tc.developer))
			}
			wantInput = append(wantInput,
				modelInstructionsMessage("developer", "input_text", agentPrompt),
				modelInstructionsMessage("user", "input_text", "Read fixture.txt"),
				modelInstructionsMessage("assistant", "output_text", "Reading."),
				map[string]any{"type": "function_call", "call_id": "call_fixture", "name": "read_file", "arguments": `{"path":"fixture.txt"}`},
				map[string]any{"type": "function_call_output", "call_id": "call_fixture", "output": "file contents"},
			)
			assertModelInstructionsJSON(t, wire.body["input"], wantInput)
		})
	}
}

func TestCodexConfigInstructionsErrorsDoNotMakeRequests(t *testing.T) {
	var requests atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer upstream.Close()
	root := t.TempDir()
	configPath := filepath.Join(root, "config.toml")
	modelPath := filepath.Join(root, "base.md")
	if err := os.WriteFile(modelPath, []byte("valid model fixture"), 0600); err != nil {
		t.Fatal(err)
	}
	sentinel := errors.New("fixture selected config unavailable")
	for _, tc := range []struct {
		name, content, wantError string
		missing, both            bool
		configError              error
	}{
		{name: "missing", missing: true, wantError: "read Codex configuration file"},
		{name: "invalid_toml", content: `developer_instructions = "fixture-secret-invalid`, wantError: "invalid Codex configuration TOML"},
		{name: "wrong_type", content: `developer_instructions = ['fixture-secret-invalid']`, wantError: "invalid Codex configuration TOML"},
		{name: "missing_model_file", content: `model_instructions_file = 'missing.md'`, wantError: "read model instructions file"},
		{name: "mutually_exclusive", content: `developer_instructions = 'valid'`, both: true, wantError: "select either"},
		{name: "retained_config_error", missing: true, configError: sentinel, wantError: sentinel.Error()},
	} {
		t.Run(tc.name, func(t *testing.T) {
			selectedPath := configPath
			if tc.missing {
				selectedPath = filepath.Join(root, "never-created.toml")
			} else if err := os.WriteFile(configPath, []byte(tc.content), 0600); err != nil {
				t.Fatal(err)
			}
			cfg := ConfigFrom("openai-responses", "test-model", upstream.URL, "fixture-profile-key", "")
			cfg.UserAgent, cfg.CodexConfigFile, cfg.ConfigError = "codex", selectedPath, tc.configError
			if tc.both {
				cfg.ModelInstructionsFile = modelPath
			}
			provider, err := cfg.NewProvider()
			if err == nil || provider != nil || !strings.Contains(err.Error(), tc.wantError) {
				t.Fatalf("invalid Codex config did not fail with %q: provider=%T err=%v", tc.wantError, provider, err)
			}
			if strings.Contains(err.Error(), "fixture-secret-invalid") {
				t.Fatal("config error disclosed unrelated source contents")
			}
			if tc.configError != nil && !errors.Is(err, sentinel) {
				t.Fatal("selected configuration error was replaced by another error")
			}
			failed := ErrorProvider(err)
			message, stop, usage, completeErr := failed.Complete(t.Context(), llm.CompletionRequest{})
			if !errors.Is(completeErr, err) || len(message.Content) != 0 || stop != "" || usage != (llm.Usage{}) {
				t.Fatal("error provider returned a successful-looking non-streaming result")
			}
			streamErrors := 0
			for event, streamErr := range failed.Stream(t.Context(), llm.CompletionRequest{}) {
				if !errors.Is(streamErr, err) || event.Type != "" {
					t.Fatal("error provider returned a successful-looking stream event")
				}
				streamErrors++
			}
			if streamErrors != 1 || requests.Load() != 0 {
				t.Fatal("invalid selected profile retried or reached the network")
			}
		})
	}
}
