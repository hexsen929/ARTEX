//go:build ignore

package llm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
)

func TestAnthropicThinkingModeMatrix(t *testing.T) {
	for _, configMode := range []string{"", "enabled", "disabled"} {
		for _, override := range []string{"", "enabled", "disabled"} {
			for _, stream := range []bool{false, true} {
				for _, hasThinking := range []bool{false, true} {
					name := fmt.Sprintf("config=%s/override=%s/stream=%t/history=%t", configMode, override, stream, hasThinking)
					t.Run(name, func(t *testing.T) {
						messages := []Message{UserText("request"), {Role: RoleAssistant, Content: []ContentBlock{TextBlock("answer")}}, UserText("continue")}
						if hasThinking {
							messages[1].Content = append([]ContentBlock{{Type: BlockThinking, Thinking: "original reasoning", Signature: "original signature"}}, messages[1].Content...)
						}
						before, _ := json.Marshal(messages)
						var body anthropicReq
						srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
							raw, _ := io.ReadAll(r.Body)
							if err := json.Unmarshal(raw, &body); err != nil {
								t.Error(err)
							}
							if stream {
								fmt.Fprint(w, "data: {\"type\":\"message_stop\"}\n\n")
								return
							}
							fmt.Fprint(w, `{"content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn"}`)
						}))
						defer srv.Close()
						p, err := NewProvider(Config{Format: FormatAnthropic, BaseURL: srv.URL, Model: "m", APIKey: "k", ThinkingType: configMode})
						if err != nil {
							t.Fatal(err)
						}
						req := CompletionRequest{Messages: messages, Thinking: override}
						if stream {
							for _, err := range p.Stream(context.Background(), req) {
								if err != nil {
									t.Fatal(err)
								}
							}
						} else {
							if _, _, _, err := p.Complete(context.Background(), req); err != nil {
								t.Fatal(err)
							}
						}
						mode := configMode
						if override != "" {
							mode = override
						}
						if mode == "" && body.Thinking != nil {
							t.Fatalf("default switch must be omitted: %+v", body.Thinking)
						}
						if mode != "" && (body.Thinking == nil || body.Thinking.Type != mode) {
							t.Fatalf("wrong mode: %+v", body.Thinking)
						}
						gotThinking := false
						for _, m := range body.Messages {
							for _, b := range m.Content {
								if b.Type == BlockThinking {
									gotThinking = true
								}
							}
						}
						if want := hasThinking && mode != "disabled"; gotThinking != want {
							t.Fatalf("thinking replay=%t, want %t", gotThinking, want)
						}
						after, _ := json.Marshal(messages)
						if !reflect.DeepEqual(before, after) {
							t.Fatal("provider changed original history")
						}
					})
				}
			}
		}
	}
}

func TestAnthropicHistoryValidationAndRestoration(t *testing.T) {
	use := ContentBlock{Type: BlockToolUse, ID: "call", Name: "Write", Input: json.RawMessage(`{"path":"a"}`)}
	assistant := Message{Role: RoleAssistant, Content: []ContentBlock{use}}
	result := Message{Role: RoleUser, Content: []ContentBlock{ToolResultText("call", "saved", false)}}
	original := Message{Role: RoleAssistant, Content: []ContentBlock{{Type: BlockThinking, Thinking: "real", Signature: "real signature"}, use}}
	cases := []struct {
		name       string
		req        CompletionRequest
		wantReason string
	}{
		{name: "missing current reasoning", req: CompletionRequest{Messages: []Message{UserText("go"), assistant, result}, Thinking: "enabled"}, wantReason: "missing thinking"},
		{name: "restored exact original", req: CompletionRequest{Messages: []Message{UserText("go"), assistant, result}, OriginalMessages: []Message{original}, Thinking: "enabled"}},
		{name: "orphan result", req: CompletionRequest{Messages: []Message{UserText("go"), result}}, wantReason: "orphan"},
		{name: "incomplete results", req: CompletionRequest{Messages: []Message{UserText("go"), assistant}}, wantReason: "missing tool result"},
		{name: "disabled complete pair", req: CompletionRequest{Messages: []Message{UserText("go"), original, result}, Thinking: "disabled"}},
		{name: "default complete pair", req: CompletionRequest{Messages: []Message{UserText("go"), original, result}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			before, _ := json.Marshal(tc.req)
			p := anthropicProvider{cfg: Config{Model: "m"}}
			body, err := p.buildBody(tc.req, false)
			if tc.wantReason != "" {
				var historyErr *HistoryError
				if !errors.As(err, &historyErr) || !strings.Contains(historyErr.Reason, tc.wantReason) {
					t.Fatalf("error=%v", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if tc.name == "restored exact original" && !strings.Contains(string(body), "real signature") {
				t.Fatalf("original thinking not restored: %s", body)
			}
			after, _ := json.Marshal(tc.req)
			if !reflect.DeepEqual(before, after) {
				t.Fatal("history restoration mutated request")
			}
		})
	}
}

func TestAnthropicInvalidThinkingModeAndHistoryHTTPClassification(t *testing.T) {
	p := anthropicProvider{cfg: Config{ThinkingType: "invalid"}}
	if _, err := p.buildBody(CompletionRequest{}, false); err == nil {
		t.Fatal("invalid mode accepted")
	}
	cases := []struct {
		name, body string
		status     int
		history    bool
	}{
		{name: "missing reasoning", status: 400, body: `{"error":{"message":"messages.1: expected thinking before tool_use"}}`, history: true},
		{name: "recorded task thinking replay", status: 400, body: "{\"error\":{\"type\":\"invalid_request_error\",\"message\":\"The `content[].thinking` in the thinking mode must be passed back to the API.\"},\"type\":\"error\"}", history: true},
		{name: "bad signature", status: 400, body: `{"error":{"message":"Invalid signature in thinking block"}}`, history: true},
		{name: "token configuration", status: 400, body: `{"error":{"message":"thinking budget_tokens must be less than max_tokens"}}`},
		{name: "mandatory reasoning", status: 400, body: `{"error":{"message":"Reasoning is mandatory for this endpoint and cannot be disabled."}}`},
		{name: "thinking parameter configuration", status: 400, body: `{"error":{"message":"thinking.type must be passed back to the API as enabled"}}`},
		{name: "server replay failure", status: 500, body: "{\"error\":{\"message\":\"The `content[].thinking` in the thinking mode must be passed back to the API.\"}}"},
		{name: "server failure", status: 500, body: `{"error":{"message":"missing thinking in messages"}}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			original := &HTTPError{Provider: "anthropic", StatusCode: tc.status, Body: tc.body}
			got := classifyAnthropicHistory(original)
			var historyErr *HistoryError
			if errors.As(got, &historyErr) != tc.history {
				t.Fatalf("error classification: %v", got)
			}
			if !errors.Is(got, original) {
				t.Fatal("lost original HTTP error")
			}
		})
	}
}
