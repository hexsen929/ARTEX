//go:build ignore

package agentcore

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Autumn-27/norma/harness"
	"github.com/Autumn-27/norma/llm"
	"github.com/Autumn-27/norma/transcript"
)

func TestThinkingHistoryHTTPRecoveryAndColdResume(t *testing.T) {
	store := transcript.NewStore(t.TempDir())
	sid := transcript.NewSessionID()
	w := store.NewWriter(sid, "")
	w.RecordMessage(llm.UserText("save the file"), llm.Usage{})
	w.RecordMessage(llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{{Type: llm.BlockToolUse, ID: "done", Name: "Write", Input: json.RawMessage(`{"path":"saved.txt"}`)}}}, llm.Usage{})
	w.RecordMessage(llm.Message{Role: llm.RoleUser, Content: []llm.ContentBlock{llm.ToolResultText("done", "already saved", false)}}, llm.Usage{})
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		raw, _ := io.ReadAll(r.Body)
		if calls == 1 {
			w.WriteHeader(400)
			fmt.Fprint(w, `{"error":{"message":"messages.1: expected thinking before tool_use"}}`)
			return
		}
		var body struct {
			Messages []llm.Message `json:"messages"`
		}
		if err := json.Unmarshal(raw, &body); err != nil {
			t.Error(err)
		}
		for _, m := range body.Messages {
			for _, b := range m.Content {
				if b.Type == llm.BlockToolUse || b.Type == llm.BlockToolResult || b.Type == llm.BlockThinking {
					t.Errorf("replayed rejected protocol block: %+v", b)
				}
			}
		}
		if !strings.Contains(string(raw), "already saved") {
			t.Error("lost completed result")
		}
		fmt.Fprint(w, "data: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"continued\"}}\n\ndata: {\"type\":\"message_stop\"}\n\n")
	}))
	defer srv.Close()
	p, err := llm.NewProvider(llm.Config{Format: llm.FormatAnthropic, BaseURL: srv.URL, Model: "m", APIKey: "k"})
	if err != nil {
		t.Fatal(err)
	}
	opts := Options{Provider: p, Transcript: store, DisableBackgroundTasks: true}
	s := NewSession(opts)
	defer s.Close()
	if err := s.Resume(sid); err != nil {
		t.Fatal(err)
	}
	for ev, err := range s.Prompt(context.Background(), "continue") {
		if err != nil {
			t.Fatal(err)
		}
		if ev.Kind == harness.KindResult && ev.Terminal.Err != nil {
			t.Fatal(ev.Terminal.Err)
		}
		if ev.Kind == harness.KindResult && ev.Terminal.Text != "continued" {
			t.Fatalf("response dropped: %+v", ev.Terminal)
		}
	}
	if calls != 2 {
		t.Fatalf("requests=%d want 2", calls)
	}
	recs, err := store.Load(store.MainPath(sid))
	if err != nil {
		t.Fatal(err)
	}
	if recs[1].Message.Content[0].Type != llm.BlockToolUse {
		t.Fatal("original transcript was rewritten")
	}
	fresh := NewSession(opts)
	defer fresh.Close()
	if err := fresh.Resume(sid); err != nil {
		t.Fatal(err)
	}
	for ev, err := range fresh.Prompt(context.Background(), "check saved result") {
		if err != nil {
			t.Fatal(err)
		}
		if ev.Kind == harness.KindResult && ev.Terminal.Err != nil {
			t.Fatal(ev.Terminal.Err)
		}
	}
	if calls != 3 {
		t.Fatalf("cold resume repeated recovery: requests=%d", calls)
	}
}

func TestColdResumeModelSwitchKeepsFactsWithoutOldSignature(t *testing.T) {
	candidateCalls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var body struct {
			Model string `json:"model"`
		}
		json.Unmarshal(raw, &body)
		if body.Model == "original" {
			fmt.Fprint(w, `{"content":[{"type":"thinking","thinking":"real thinking","signature":"signed-original"},{"type":"text","text":"saved information"}],"stop_reason":"end_turn"}`)
			return
		}
		candidateCalls++
		if strings.Contains(string(raw), "signed-original") || !strings.Contains(string(raw), "saved information") {
			t.Errorf("invalid recovered context: %s", raw)
		}
		fmt.Fprint(w, `{"content":[{"type":"text","text":"continued"}],"stop_reason":"end_turn"}`)
	}))
	defer srv.Close()
	oldProvider, _ := llm.NewProvider(llm.Config{Format: llm.FormatAnthropic, Model: "original", BaseURL: srv.URL, APIKey: "key"})
	old, _, _, err := oldProvider.Complete(context.Background(), llm.CompletionRequest{Messages: []llm.Message{llm.UserText("remember")}})
	if err != nil {
		t.Fatal(err)
	}
	store := transcript.NewStore(t.TempDir())
	sid := transcript.NewSessionID()
	writer := store.NewWriter(sid, "")
	writer.RecordMessage(llm.UserText("remember"), llm.Usage{})
	writer.RecordMessage(old, llm.Usage{})
	if writer.Err() != nil {
		t.Fatal(writer.Err())
	}
	newProvider, _ := llm.NewProvider(llm.Config{Format: llm.FormatAnthropic, Model: "candidate", BaseURL: srv.URL, APIKey: "key"})
	s := NewSession(Options{Provider: newProvider, Transcript: store, NonStreaming: true, DisableBackgroundTasks: true})
	defer s.Close()
	if err := s.Resume(sid); err != nil {
		t.Fatal(err)
	}
	for ev, err := range s.Prompt(context.Background(), "continue") {
		if err != nil {
			t.Fatal(err)
		}
		if ev.Kind == harness.KindResult && (ev.Terminal.Err != nil || ev.Terminal.Text != "continued") {
			t.Fatalf("terminal=%+v", ev.Terminal)
		}
	}
	if candidateCalls != 1 {
		t.Fatalf("incompatible replay reached endpoint: calls=%d", candidateCalls)
	}
	records, err := store.Load(store.MainPath(sid))
	if err != nil {
		t.Fatal(err)
	}
	if records[1].Message.Content[0].Signature != "signed-original" || records[1].Replay.Source.Model != "original" {
		t.Fatal("original history/source rewritten")
	}
}
