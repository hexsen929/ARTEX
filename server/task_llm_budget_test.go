package server

import (
	"context"
	"errors"
	"github.com/Autumn-27/artex/agent"
	"github.com/Autumn-27/artex/llmguard"
	"github.com/Autumn-27/norma/harness"
	"github.com/Autumn-27/norma/llm"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func TestTaskLLMBudgetCountsSDKAndStreamRetries(t *testing.T) {
	for _, stream := range []bool{true, false} {
		t.Run(map[bool]string{true: "stream", false: "complete"}[stream], func(t *testing.T) {
			var calls atomic.Int64
			host := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				w.WriteHeader(503)
				w.Write([]byte(`{"error":{"message":"overloaded"}}`))
			}))
			defer host.Close()
			cfg := agent.Config{Format: llm.FormatOpenAI, BaseURL: host.URL, APIKey: "fixture", Model: "fixture", Retry: agent.RetryConfig{ConnectAttempts: 5, ConnectInterval: time.Nanosecond, EmptyAttempts: -1, StreamAttempts: 5, StreamInterval: time.Nanosecond}}
			provider, err := cfg.NewProvider()
			if err != nil {
				t.Fatal(err)
			}
			hooks := taskLLMStreamHooks{current: func() (taskLLMSelection, error) { return taskLLMSelection{provider: provider, retry: cfg.Retry}, nil }}
			if stream {
				_, err = collectTaskLLMStream(streamTaskLLM(context.Background(), "fixture", llm.CompletionRequest{}, hooks))
			} else {
				_, _, _, err = completeTaskLLM(context.Background(), "fixture", llm.CompletionRequest{}, hooks)
			}
			if err == nil || !isTaskLLMRuntimeError(err) || llmguard.Classify(err) != llmguard.RetryBudget || calls.Load() != llmguard.DefaultHTTPAttempts {
				t.Fatalf("calls=%d err=%v kind=%s", calls.Load(), err, llmguard.Classify(err))
			}
		})
	}
}
func TestMidStreamNetworkErrorNeverReplaysWholeWorker(t *testing.T) {
	p := &scriptedLLMProvider{events: []llm.StreamEvent{{Type: llm.SEToolUseStart}}, err: errors.New("unexpected EOF")}
	hooks := taskLLMStreamHooks{current: func() (taskLLMSelection, error) { return taskLLMSelection{provider: p}, nil }}
	_, err := collectTaskLLMStream(streamTaskLLM(context.Background(), "fixture", llm.CompletionRequest{}, hooks))
	if !isTaskLLMRuntimeError(err) || p.calls != 1 || retryableWorkerModelError(harness.ReasonModelError, err) {
		t.Fatalf("unsafe stream replay calls=%d error=%v", p.calls, err)
	}
}
