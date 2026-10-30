package llmrec

import (
	"context"
	"encoding/json"
	"errors"
	"iter"
	"os"
	"strings"
	"testing"

	"github.com/Autumn-27/artex/db"
	"github.com/Autumn-27/artex/memcontext"
	"github.com/Autumn-27/norma/llm"
	"github.com/google/uuid"
)

const memoryRecordingCanary = "private-memory-recording-canary-9ce8"

type memoryPrivacyProvider struct {
	t    *testing.T
	fail bool
}

func (p *memoryPrivacyProvider) Complete(ctx context.Context, req llm.CompletionRequest) (llm.Message, string, llm.Usage, error) {
	if !memcontext.MetadataOnly(ctx) || CaptureFrom(ctx) != nil {
		p.t.Fatal("memory completion retained raw wire capture")
	}
	var err error
	if p.fail {
		err = errors.New(memoryRecordingCanary)
	}
	return llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{
		llm.TextBlock(memoryRecordingCanary), {Type: llm.BlockThinking, Thinking: memoryRecordingCanary},
	}}, "stop", llm.Usage{InputTokens: 7, OutputTokens: 3}, err
}

func (p *memoryPrivacyProvider) Stream(ctx context.Context, req llm.CompletionRequest) iter.Seq2[llm.StreamEvent, error] {
	return func(yield func(llm.StreamEvent, error) bool) {
		if !memcontext.MetadataOnly(ctx) || CaptureFrom(ctx) != nil {
			p.t.Fatal("memory stream retained raw wire capture")
		}
		for _, event := range []llm.StreamEvent{
			{Type: llm.SEMessageStart, Usage: llm.Usage{InputTokens: 7}},
			{Type: llm.SETextDelta, Text: memoryRecordingCanary},
			{Type: llm.SEThinkingDelta, Text: memoryRecordingCanary},
			{Type: llm.SEMessageDelta, Usage: llm.Usage{OutputTokens: 3}, StopReason: "stop"},
			{Type: llm.SEMessageStop},
		} {
			if !yield(event, nil) {
				return
			}
		}
	}
}

func TestMemoryMetadataSuppressesEvenAnEnclosingRawCapture(t *testing.T) {
	ctx, capture := NewCapture(t.Context())
	if CaptureFrom(ctx) != capture {
		t.Fatal("ordinary capture changed")
	}
	ctx = memcontext.WithMetadataOnly(ctx)
	if CaptureFrom(ctx) != nil {
		t.Fatal("metadata-only scope exposed enclosing wire capture")
	}
	metadata := serializeMemoryMetadata([]memcontext.Reference{{ID: 7, Version: 9}})
	var decoded map[string]any
	if json.Unmarshal([]byte(metadata), &decoded) != nil || decoded["recording_mode"] != "metadata_only" || strings.Contains(metadata, memoryRecordingCanary) {
		t.Fatalf("invalid metadata request: %s", metadata)
	}
	if metadataStopReason(memoryRecordingCanary) != "other" {
		t.Fatal("nonstandard stop reason could persist memory content")
	}
}

func TestMemoryCallsRecordOnlyMetadataAndKeepUsage(t *testing.T) {
	dsn := os.Getenv("ARTEX_PG_DSN")
	if dsn == "" {
		t.Skip("requires isolated ARTEX_PG_DSN")
	}
	pg, err := db.Open(dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = pg.Close() })
	if err = pg.EnsureLLMRecordsTable(); err != nil {
		t.Fatal(err)
	}
	if err = pg.EnsureLLMUsageTable(); err != nil {
		t.Fatal(err)
	}
	profile := "memory-privacy-" + uuid.NewString()
	t.Cleanup(func() {
		_, _ = pg.Exec(`DELETE FROM llm_records WHERE profile_name=$1`, profile)
		_, _ = pg.Exec(`DELETE FROM llm_usage WHERE profile_name=$1`, profile)
	})
	provider := &memoryPrivacyProvider{t: t}
	recorder := Wrap(provider, pg, "fixture", profile, "", "", func() bool { return true })
	ctx, release := memcontext.WithRecall(t.Context(), []memcontext.Reference{{ID: 7, Version: 9}}, func(context.Context) error { return nil })
	defer release()
	req := llm.CompletionRequest{System: []string{memcontext.FormatBlock(memoryRecordingCanary, memcontext.References(ctx))}, Messages: []llm.Message{llm.UserText(memoryRecordingCanary)}}
	if _, _, _, err = recorder.Complete(ctx, req); err != nil {
		t.Fatal(err)
	}
	for _, streamErr := range recorder.Stream(ctx, req) {
		if streamErr != nil {
			t.Fatal(streamErr)
		}
	}
	provider.fail = true
	if _, _, _, err = recorder.Complete(ctx, req); err == nil {
		t.Fatal("fixture failure was lost")
	}
	rows, err := pg.Query(`SELECT request_body,response_body,raw_request,raw_response,error FROM llm_records WHERE profile_name=$1 ORDER BY id`, profile)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	count := 0
	for rows.Next() {
		var request, response, rawRequest, rawResponse, failure string
		if err = rows.Scan(&request, &response, &rawRequest, &rawResponse, &failure); err != nil {
			t.Fatal(err)
		}
		if strings.Contains(request+response+rawRequest+rawResponse+failure, memoryRecordingCanary) || rawRequest != "" || rawResponse != "" {
			t.Fatal("memory content persisted in normalized/raw/error recording")
		}
		var requestJSON, responseJSON map[string]any
		if json.Unmarshal([]byte(request), &requestJSON) != nil || json.Unmarshal([]byte(response), &responseJSON) != nil || requestJSON["recording_mode"] != "metadata_only" || responseJSON["recording_mode"] != "metadata_only" {
			t.Fatal("metadata-only trace was not explicitly labelled")
		}
		count++
	}
	if err = rows.Err(); err != nil || count != 3 {
		t.Fatalf("trace rows=%d error=%v", count, err)
	}
	var inputs, outputs int
	if err = pg.QueryRow(`SELECT COALESCE(SUM(input_tokens),0),COALESCE(SUM(output_tokens),0) FROM llm_usage WHERE profile_name=$1`, profile).Scan(&inputs, &outputs); err != nil {
		t.Fatal(err)
	}
	if inputs != 21 || outputs != 9 {
		t.Fatalf("memory calls lost usage accounting: %d/%d", inputs, outputs)
	}
}
