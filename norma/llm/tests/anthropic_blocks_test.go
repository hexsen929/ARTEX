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

func TestAnthropicBlockBoundariesAndSignatures(t *testing.T) {
	frames := []string{
		`{"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":""}}`,
		`{"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"one"}}`,
		`{"type":"content_block_delta","index":0,"delta":{"type":"signature_delta","signature":"sig"}}`,
		`{"type":"content_block_delta","index":0,"delta":{"type":"signature_delta","signature":"-1"}}`,
		`{"type":"content_block_stop","index":0}`,
		`{"type":"content_block_start","index":1,"content_block":{"type":"thinking","thinking":""}}`,
		`{"type":"content_block_delta","index":1,"delta":{"type":"signature_delta","signature":"sig-2"}}`,
		`{"type":"content_block_stop","index":1}`,
		`{"type":"content_block_start","index":2,"content_block":{"type":"text","text":"initial"}}`,
		`{"type":"content_block_delta","index":2,"delta":{"type":"text_delta","text":" text"}}`,
		`{"type":"content_block_stop","index":2}`,
		`{"type":"message_stop"}`,
	}
	acc := NewAccumulator()
	for _, raw := range frames {
		ev, ok, err := parseAnthropicFrame(raw)
		if err != nil {
			t.Fatal(err)
		}
		if ok {
			acc.Add(ev)
		}
	}
	want := []ContentBlock{{Type: BlockThinking, Thinking: "one", Signature: "sig-1"}, {Type: BlockThinking, Signature: "sig-2"}, TextBlock("initial text")}
	if got := acc.Message().Content; !reflect.DeepEqual(got, want) {
		t.Fatalf("blocks=%+v, want %+v", got, want)
	}
}

func TestAnthropicReplaySourceAndInterruptedStream(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "data: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"thinking\",\"thinking\":\"partial\"}}\n\n")
	}))
	defer srv.Close()
	p, _ := NewProvider(Config{Format: FormatAnthropic, Model: "model-a", BaseURL: srv.URL, APIKey: "private-secret"})
	acc := NewAccumulator()
	var streamErr error
	for ev, err := range p.Stream(context.Background(), CompletionRequest{Messages: []Message{UserText("go")}}) {
		if err != nil {
			streamErr = err
			break
		}
		acc.Add(ev)
	}
	if !errors.Is(streamErr, io.ErrUnexpectedEOF) {
		t.Fatalf("drop silently succeeded: %v", streamErr)
	}
	m := acc.Message()
	if m.Replay == nil || !m.Replay.Incomplete || m.Replay.Source == nil {
		t.Fatalf("missing partial provenance: %+v", m)
	}
	metadata, _ := json.Marshal(m.Replay)
	if strings.Contains(string(metadata), "private-secret") {
		t.Fatal("credential leaked")
	}
	m.Replay.Incomplete = false
	candidate := p.(*anthropicProvider)
	if _, err := candidate.buildBody(CompletionRequest{Messages: []Message{UserText("go"), m, UserText("continue")}}, false); err != nil {
		t.Fatal(err)
	}
	other := *candidate
	other.cfg.Model = "model-b"
	_, err := other.buildBody(CompletionRequest{Messages: []Message{UserText("go"), m, UserText("continue")}}, false)
	var historyErr *HistoryError
	if !errors.As(err, &historyErr) || !historyErr.Recoverable {
		t.Fatalf("model switch not rejected: %v", err)
	}
	body, err := other.buildBody(CompletionRequest{Messages: []Message{UserText("go"), m, UserText("continue")}, Thinking: "disabled"}, false)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(body), "endpoint_hash") || strings.Contains(string(body), "incomplete") {
		t.Fatalf("local metadata on wire: %s", body)
	}
}

func TestAnthropicRawOrphanCannotBeSilentlyProjectedAway(t *testing.T) {
	p := anthropicProvider{cfg: Config{Model: "m"}}
	_, err := p.buildBody(CompletionRequest{Messages: []Message{UserText("go")}, OriginalMessages: []Message{UserText("go"), {Role: RoleUser, Content: []ContentBlock{ToolResultText("unknown", "saved fact", false)}}}, ValidateOriginalHistory: true}, false)
	var rejected *HistoryError
	if !errors.As(err, &rejected) || rejected.Recoverable {
		t.Fatalf("raw corruption ignored: %v", err)
	}
}

func TestAnthropicNonStreamingPreservesOpaque(t *testing.T) {
	raw := []byte(`{"content":[{"type":"thinking","thinking":"","signature":"s"},{"type":"redacted_thinking","data":"opaque"},{"type":"text","text":""}],"stop_reason":"end_turn"}`)
	msg, _, _, err := parseAnthropicResponse(raw)
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(msg.Content)
	if err != nil {
		t.Fatal(err)
	}
	var expected, actual any
	json.Unmarshal([]byte(`[{"type":"thinking","thinking":"","signature":"s"},{"type":"redacted_thinking","data":"opaque"},{"type":"text","text":""}]`), &expected)
	json.Unmarshal(encoded, &actual)
	if !reflect.DeepEqual(actual, expected) {
		t.Fatalf("lost blocks: %s", encoded)
	}
}
