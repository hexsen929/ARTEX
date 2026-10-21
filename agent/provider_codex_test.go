package agent

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/Autumn-27/norma/llm"
	"github.com/Autumn-27/norma/transcript"
	"github.com/google/uuid"
)

type codexWireRequest struct {
	path    string
	headers http.Header
	body    map[string]any
	err     error
}

func writeCodexTestFrame(w io.Writer, event string, body any) {
	raw, _ := json.Marshal(body)
	_, _ = fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, raw)
}

func writeCodexTestCompleted(w io.Writer) {
	writeCodexTestFrame(w, "response.completed", map[string]any{
		"type": "response.completed", "response": map[string]any{
			"status": "completed", "usage": map[string]any{
				"input_tokens": 101, "output_tokens": 8,
				"input_tokens_details": map[string]any{"cached_tokens": 12},
			},
		},
	})
}

// Cover a real two-turn tool workflow through NewProvider, including the
// streaming-only wire used when the host asks for a non-streaming completion.
func TestCodexResponsesCompleteToolRoundTrip(t *testing.T) {
	wires := make(chan codexWireRequest, 2)
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		wire := codexWireRequest{path: r.URL.Path, headers: r.Header.Clone()}
		wire.err = json.NewDecoder(r.Body).Decode(&wire.body)
		wires <- wire
		w.Header().Set("Content-Type", "text/event-stream")
		if calls.Add(1) == 1 {
			writeCodexTestFrame(w, "response.output_text.delta", map[string]any{"delta": "I will inspect."})
			writeCodexTestFrame(w, "response.output_item.added", map[string]any{
				"item": map[string]any{"type": "function_call", "call_id": "call_read", "name": "read_file"},
			})
			for _, part := range []string{`{"path":"目录/a.txt",`, `"limit":2}`} {
				writeCodexTestFrame(w, "response.function_call_arguments.delta", map[string]any{"delta": part})
			}
		} else {
			writeCodexTestFrame(w, "response.output_text.delta", map[string]any{"delta": "Checked."})
		}
		writeCodexTestCompleted(w)
	}))
	defer upstream.Close()

	cfg := ConfigFrom("openai-responses", "test-model", upstream.URL+"/v1", "fixture-key", "")
	cfg.UserAgent, cfg.Stream, cfg.SessionHeaderKey = "codex", false, "X-Artex-Session"
	provider, err := cfg.NewProvider()
	if err != nil {
		t.Fatal(err)
	}
	ctx := transcript.WithSessionID(t.Context(), "worker-intent-42")
	temperature := 0.25
	schema := map[string]any{
		"type": "object", "additionalProperties": false,
		"properties": map[string]any{
			"path":  map[string]any{"type": "string", "description": "文件路径"},
			"limit": map[string]any{"type": "integer"},
		},
		"required": []string{"path"},
	}
	req := llm.CompletionRequest{
		System:    []string{"保留 ARTEX 的任务权限与范围。", "Read only the requested file."},
		Messages:  []llm.Message{llm.UserText("Inspect 目录/a.txt")},
		Tools:     []llm.ToolSchema{{Name: "read_file", Description: "Read a file", InputSchema: schema}},
		MaxTokens: 512, Temperature: &temperature,
	}
	first, stop, usage, err := provider.Complete(ctx, req)
	if err != nil {
		t.Fatalf("first completion: %v", err)
	}
	tools := first.ToolUses()
	if first.Text() != "I will inspect." || len(tools) != 1 || tools[0].ID != "call_read" || tools[0].Name != "read_file" || string(tools[0].Input) != `{"path":"目录/a.txt","limit":2}` {
		t.Fatalf("stream aggregation lost text, tool ID, name, or arguments: %+v", first)
	}
	if stop != "tool_use" {
		t.Errorf("tool completion stop=%q, want tool_use", stop)
	}
	if usage != (llm.Usage{InputTokens: 101, OutputTokens: 8, CacheReadTokens: 12}) {
		t.Fatalf("usage=%+v, want input=101 output=8 cached=12", usage)
	}
	req.Messages = append(req.Messages, first, llm.Message{Role: llm.RoleUser, Content: []llm.ContentBlock{
		llm.ToolResultText("call_read", "第一行\nsecond line", false), llm.TextBlock("Now summarize."),
	}})
	second, stop, _, err := provider.Complete(ctx, req)
	if err != nil || second.Text() != "Checked." || stop != "end_turn" {
		t.Fatalf("second completion text=%q stop=%q err=%v", second.Text(), stop, err)
	}

	var firstSession string
	for turn := 0; turn < 2; turn++ {
		wire := <-wires
		if wire.err != nil {
			t.Fatalf("decode outgoing request: %v", wire.err)
		}
		if wire.path != "/v1/responses" || wire.headers.Get("User-Agent") != "codex_cli_rs/0.162.1" || wire.headers.Get("originator") != "codex_cli_rs" {
			t.Fatal("outgoing request is missing the Codex client identity or Responses endpoint")
		}
		if wire.headers.Get("OpenAI-Beta") != "responses=experimental" || wire.headers.Get("version") != "0.162.1" || wire.headers.Get("X-Artex-Session") != "worker-intent-42" {
			t.Fatal("Codex protocol or custom session headers were lost")
		}
		session := wire.headers.Get("session-id")
		if _, err := uuid.Parse(session); err != nil || wire.body["prompt_cache_key"] != session {
			t.Fatal("session header and prompt cache key must carry the same UUID")
		}
		if turn == 0 {
			firstSession = session
		} else if session != firstSession {
			t.Fatal("session/cache identity changed between tool turns")
		}
		wantInstructions := "You are Codex, a command-line coding agent.\n\n" + strings.Join(req.System, "\n\n")
		if wire.body["instructions"] != wantInstructions {
			t.Fatal("Codex compatibility changed or duplicated the ARTEX system prompt")
		}
		if wire.body["stream"] != true || wire.body["store"] != false || wire.body["tool_choice"] != "auto" || wire.body["parallel_tool_calls"] != false {
			t.Fatal("request is not a stateless streaming Codex tool request")
		}
		for _, field := range []string{"max_output_tokens", "max_tokens", "max_completion_tokens", "temperature"} {
			if _, present := wire.body[field]; present {
				t.Errorf("Codex request still contains unsupported %s", field)
			}
		}
		if !reflect.DeepEqual(wire.body["include"], []any{"reasoning.encrypted_content"}) {
			t.Fatal("Codex reasoning include was not requested")
		}
		wantTools, _ := json.Marshal([]map[string]any{{"type": "function", "name": "read_file", "description": "Read a file", "parameters": schema}})
		var decodedTools any
		_ = json.Unmarshal(wantTools, &decodedTools)
		if !reflect.DeepEqual(wire.body["tools"], decodedTools) {
			t.Fatal("Codex compatibility altered the tool schema")
		}
		wantInput := []map[string]any{{"type": "message", "role": "user", "content": []map[string]string{{"type": "input_text", "text": "Inspect 目录/a.txt"}}}}
		if turn == 1 {
			wantInput = append(wantInput,
				map[string]any{"type": "message", "role": "assistant", "content": []map[string]string{{"type": "output_text", "text": "I will inspect."}}},
				map[string]any{"type": "function_call", "call_id": "call_read", "name": "read_file", "arguments": `{"path":"目录/a.txt","limit":2}`},
				map[string]any{"type": "function_call_output", "call_id": "call_read", "output": "第一行\nsecond line"},
				map[string]any{"type": "message", "role": "user", "content": []map[string]string{{"type": "input_text", "text": "Now summarize."}}},
			)
		}
		encoded, _ := json.Marshal(wantInput)
		var decodedInput any
		_ = json.Unmarshal(encoded, &decodedInput)
		if !reflect.DeepEqual(wire.body["input"], decodedInput) {
			t.Fatalf("turn %d history lost text, tool call/result pairing, or ordering: %v", turn+1, wire.body["input"])
		}
	}
}

func TestCodexResponsesCustomSessionHeaderKeepsCacheIdentity(t *testing.T) {
	for _, headerKey := range []string{"session-id", "Session-ID"} {
		t.Run(headerKey, func(t *testing.T) {
			wires := make(chan codexWireRequest, 1)
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				wire := codexWireRequest{headers: r.Header.Clone()}
				wire.err = json.NewDecoder(r.Body).Decode(&wire.body)
				wires <- wire
				w.Header().Set("Content-Type", "text/event-stream")
				writeCodexTestFrame(w, "response.output_text.delta", map[string]any{"delta": "OK"})
				writeCodexTestCompleted(w)
			}))
			defer upstream.Close()
			cfg := ConfigFrom("openai-responses", "test-model", upstream.URL, "fixture-key", "")
			cfg.UserAgent, cfg.SessionHeaderKey = "codex", headerKey
			provider, err := cfg.NewProvider()
			if err != nil {
				t.Fatal(err)
			}
			const rawSessionID = "worker-intent-collision"
			ctx := transcript.WithSessionID(t.Context(), rawSessionID)
			message, _, _, err := provider.Complete(ctx, llm.CompletionRequest{Messages: []llm.Message{llm.UserText("hello")}})
			if err != nil || message.Text() != "OK" {
				t.Fatalf("completion failed: text=%q err=%v", message.Text(), err)
			}
			wire := <-wires
			if wire.err != nil {
				t.Fatal(wire.err)
			}
			sid := wire.headers.Get("session-id")
			if _, err := uuid.Parse(sid); err != nil || sid == rawSessionID || wire.body["prompt_cache_key"] != sid {
				t.Fatal("custom session-id header replaced the Codex UUID or diverged from the prompt cache identity")
			}
		})
	}
}

func TestCodexResponsesCompleteRejectsBrokenStream(t *testing.T) {
	for _, tc := range []struct {
		name  string
		frame string
	}{
		{name: "EOF_after_text"},
		{name: "response_failed", frame: "response.failed"},
		{name: "error", frame: "error"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				writeCodexTestFrame(w, "response.output_text.delta", map[string]any{"delta": "partial"})
				if tc.frame != "" {
					body := map[string]any{"type": tc.frame}
					if tc.frame == "response.failed" {
						body["response"] = map[string]any{"status": "failed", "error": map[string]any{"message": "fixture upstream failure"}}
					} else {
						body["error"] = map[string]any{"message": "fixture upstream failure"}
					}
					writeCodexTestFrame(w, tc.frame, body)
				}
			}))
			defer upstream.Close()
			cfg := ConfigFrom("openai-responses", "test-model", upstream.URL, "fixture-key", "")
			cfg.UserAgent = "codex"
			provider, err := cfg.NewProvider()
			if err != nil {
				t.Fatal(err)
			}
			message, stop, usage, err := provider.Complete(t.Context(), llm.CompletionRequest{Messages: []llm.Message{llm.UserText("hello")}})
			if err == nil {
				t.Fatal("truncated/failed stream was returned as a successful partial completion")
			}
			if message.Text() != "" || stop != "" || usage != (llm.Usage{}) {
				t.Fatal("failed completion exposed a successful-looking partial result")
			}
		})
	}
}

func TestResponsesCodexCompatibilityIsOptIn(t *testing.T) {
	for _, tc := range []struct {
		name, format, userAgent, wantUA, capField string
	}{
		{name: "default_responses", format: "openai-responses", wantUA: "Go-http-client/1.1", capField: "max_output_tokens"},
		{name: "custom_codex_ua", format: "openai-responses", userAgent: "codex_cli_rs/0.162.1", wantUA: "codex_cli_rs/0.162.1", capField: "max_output_tokens"},
		{name: "codex_chat", format: "openai", userAgent: "codex", wantUA: "codex_cli_rs/0.162.1", capField: "max_tokens"},
		{name: "codex_anthropic", format: "anthropic", userAgent: "codex", wantUA: "codex_cli_rs/0.162.1", capField: "max_tokens"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			wires := make(chan codexWireRequest, 1)
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				wire := codexWireRequest{path: r.URL.Path, headers: r.Header.Clone()}
				wire.err = json.NewDecoder(r.Body).Decode(&wire.body)
				wires <- wire
				w.Header().Set("Content-Type", "application/json")
				switch tc.format {
				case "anthropic":
					_, _ = io.WriteString(w, `{"id":"msg_test","type":"message","role":"assistant","content":[{"type":"text","text":"OK"}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`)
				case "openai":
					_, _ = io.WriteString(w, `{"choices":[{"message":{"role":"assistant","content":"OK"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`)
				default:
					_, _ = io.WriteString(w, `{"status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"OK"}]}],"usage":{"input_tokens":1,"output_tokens":1}}`)
				}
			}))
			defer upstream.Close()
			cfg := ConfigFrom(tc.format, "test-model", upstream.URL, "fixture-key", "")
			cfg.UserAgent = tc.userAgent
			provider, err := cfg.NewProvider()
			if err != nil {
				t.Fatal(err)
			}
			message, _, _, err := provider.Complete(t.Context(), llm.CompletionRequest{
				System: []string{"Original ARTEX prompt"}, Messages: []llm.Message{llm.UserText("hello")}, MaxTokens: 73,
			})
			if err != nil || message.Text() != "OK" {
				t.Fatalf("ordinary Complete failed: text=%q err=%v", message.Text(), err)
			}
			wire := <-wires
			if wire.err != nil {
				t.Fatal(wire.err)
			}
			if wire.headers.Get("User-Agent") != tc.wantUA || wire.headers.Get("originator") != "" || wire.headers.Get("session-id") != "" {
				t.Fatal("Codex request compatibility leaked outside the explicit Responses preset")
			}
			if wire.body["stream"] == true || wire.body[tc.capField] != float64(73) {
				t.Fatal("ordinary non-streaming request or output cap changed")
			}
			if tc.format == "openai-responses" {
				if wire.body["instructions"] != "Original ARTEX prompt" {
					t.Fatal("ordinary Responses prompt was changed")
				}
				input := wire.body["input"].([]any)[0].(map[string]any)
				if input["content"] != "hello" {
					t.Fatal("ordinary Responses message content was changed")
				}
			}
		})
	}
}
