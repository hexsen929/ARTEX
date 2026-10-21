//go:build ignore

package harness

import (
	"context"
	"encoding/json"
	"fmt"
	"iter"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/Autumn-27/norma/llm"
)

func TestHistoryRecoveryPreservesResultsAndResume(t *testing.T) {
	for _, nonStreaming := range []bool{false, true} {
		t.Run(map[bool]string{false: "stream", true: "complete"}[nonStreaming], func(t *testing.T) {
			history := []llm.Message{llm.UserText("save the file"), {Role: llm.RoleAssistant, Content: []llm.ContentBlock{{Type: llm.BlockToolUse, ID: "saved", Name: "Write", Input: json.RawMessage(`{"path":"saved.txt"}`)}}}, {Role: llm.RoleUser, Content: []llm.ContentBlock{llm.ToolResultText("saved", "saved successfully", false)}}}
			before, _ := json.Marshal(history)
			calls := 0
			check := func(req llm.CompletionRequest) error {
				calls++
				if calls == 1 {
					return &llm.HistoryError{Reason: "missing thinking", Recoverable: true}
				}
				for _, m := range req.Messages {
					for _, b := range m.Content {
						if b.Type != llm.BlockText {
							t.Fatalf("recovery fabricated protocol blocks: %+v", b)
						}
					}
				}
				payload, _ := json.Marshal(req.Messages)
				if !strings.Contains(string(payload), "saved successfully") || !strings.Contains(string(payload), "saved.txt") {
					t.Fatalf("lost recorded facts: %s", payload)
				}
				return nil
			}
			deps := QueryDeps{
				CallModel: func(_ context.Context, req llm.CompletionRequest) iter.Seq2[llm.StreamEvent, error] {
					return func(yield func(llm.StreamEvent, error) bool) {
						if err := check(req); err != nil {
							yield(llm.StreamEvent{}, err)
							return
						}
						yield(llm.StreamEvent{Type: llm.SETextDelta, Text: "continued"}, nil)
					}
				},
				CallModelSync: func(_ context.Context, req llm.CompletionRequest) (llm.Message, string, llm.Usage, error) {
					if err := check(req); err != nil {
						return llm.Message{}, "", llm.Usage{}, err
					}
					return llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{llm.TextBlock("continued")}}, "end_turn", llm.Usage{}, nil
				},
			}
			rec := &capRecorder{}
			term := drain(t, QueryInput{Messages: history, NonStreaming: nonStreaming, Recorder: rec}, deps)
			if term.Err != nil || term.Text != "continued" || calls != 2 {
				t.Fatalf("calls=%d terminal=%+v", calls, term)
			}
			if rec.boundaries != 1 {
				t.Fatalf("recovery not persisted: %d boundaries", rec.boundaries)
			}
			if len(llm.MessagesForAPI(rec.msgs)) == 0 {
				t.Fatal("cold resume has no recovery context")
			}
			after, _ := json.Marshal(history)
			if !reflect.DeepEqual(before, after) {
				t.Fatal("original history changed")
			}
		})
	}
}

func TestHistoryRecoveryRecordedGatewayReplayComplaint(t *testing.T) {
	for _, nonStreaming := range []bool{false, true} {
		t.Run(map[bool]string{false: "stream", true: "complete"}[nonStreaming], func(t *testing.T) {
			history := []llm.Message{llm.UserText("save the file"), {Role: llm.RoleAssistant, Content: []llm.ContentBlock{{Type: llm.BlockToolUse, ID: "saved", Name: "Write", Input: json.RawMessage(`{"path":"saved.txt"}`)}}}, {Role: llm.RoleUser, Content: []llm.ContentBlock{llm.ToolResultText("saved", "saved successfully", false)}}}
			before, _ := json.Marshal(history)
			var calls atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var body struct {
					Messages []llm.Message `json:"messages"`
				}
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					t.Error(err)
				}
				if calls.Add(1) == 1 {
					w.WriteHeader(http.StatusBadRequest)
					fmt.Fprint(w, "{\"error\":{\"type\":\"invalid_request_error\",\"message\":\"The `content[].thinking` in the thinking mode must be passed back to the API.\"},\"type\":\"error\"}")
					return
				}
				payload, _ := json.Marshal(body.Messages)
				if !strings.Contains(string(payload), "saved successfully") || !strings.Contains(string(payload), "saved.txt") {
					t.Errorf("recovery lost the completed write: %s", payload)
				}
				for _, message := range body.Messages {
					for _, block := range message.Content {
						if block.Type != llm.BlockText {
							t.Errorf("recovery fabricated replay blocks: %+v", block)
						}
					}
				}
				if nonStreaming {
					fmt.Fprint(w, `{"content":[{"type":"text","text":"continued"}],"stop_reason":"end_turn"}`)
				} else {
					fmt.Fprint(w, "data: {\"type\":\"content_block_delta\",\"delta\":{\"type\":\"text_delta\",\"text\":\"continued\"}}\n\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"}}\n\ndata: {\"type\":\"message_stop\"}\n\n")
				}
			}))
			defer srv.Close()
			provider, err := llm.NewProvider(llm.Config{Format: llm.FormatAnthropic, BaseURL: srv.URL, Model: "m", MaxRetries: -1})
			if err != nil {
				t.Fatal(err)
			}
			recorder := &capRecorder{}
			terminal := drain(t, QueryInput{Provider: provider, Messages: history, NonStreaming: nonStreaming, Recorder: recorder}, QueryDeps{})
			if terminal.Err != nil || terminal.Text != "continued" || calls.Load() != 2 || recorder.boundaries != 1 {
				t.Fatalf("recorded complaint not recovered exactly once: terminal=%+v calls=%d boundaries=%d", terminal, calls.Load(), recorder.boundaries)
			}
			after, _ := json.Marshal(history)
			if !reflect.DeepEqual(before, after) {
				t.Fatal("recovery rewrote the original history")
			}
		})
	}
}

func TestHistoryRecoveryIsBoundedAndNeverAfterOutput(t *testing.T) {
	for _, partial := range []bool{false, true} {
		t.Run(map[bool]string{false: "repeated", true: "partial"}[partial], func(t *testing.T) {
			calls := 0
			deps := QueryDeps{CallModel: func(_ context.Context, _ llm.CompletionRequest) iter.Seq2[llm.StreamEvent, error] {
				return func(yield func(llm.StreamEvent, error) bool) {
					calls++
					if partial {
						yield(llm.StreamEvent{Type: llm.SETextDelta, Text: "partial"}, nil)
					}
					yield(llm.StreamEvent{}, &llm.HistoryError{Reason: "bad signature", Recoverable: true})
				}
			}}
			term := drain(t, QueryInput{Messages: []llm.Message{llm.UserText("go"), {Role: llm.RoleAssistant, Content: []llm.ContentBlock{llm.TextBlock("old")}}}}, deps)
			want := 2
			if partial {
				want = 1
			}
			if calls != want || term.Err == nil {
				t.Fatalf("calls=%d want %d terminal=%+v", calls, want, term)
			}
		})
	}
}
