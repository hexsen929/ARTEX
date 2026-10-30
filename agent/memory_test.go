package agent

import (
	"context"
	"errors"
	"fmt"
	"io"
	"iter"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/Autumn-27/artex/db"
	"github.com/Autumn-27/artex/llmguard"
	"github.com/Autumn-27/artex/memcontext"
	"github.com/Autumn-27/norma/agentcore"
	"github.com/Autumn-27/norma/compaction"
	"github.com/Autumn-27/norma/llm"
)

type memoryFixtureProvider struct {
	complete func(context.Context, llm.CompletionRequest) (llm.Message, string, llm.Usage, error)
	stream   func(context.Context, llm.CompletionRequest) iter.Seq2[llm.StreamEvent, error]
}

func (p memoryFixtureProvider) Complete(ctx context.Context, req llm.CompletionRequest) (llm.Message, string, llm.Usage, error) {
	return p.complete(ctx, req)
}
func (p memoryFixtureProvider) Stream(ctx context.Context, req llm.CompletionRequest) iter.Seq2[llm.StreamEvent, error] {
	if p.stream != nil {
		return p.stream(ctx, req)
	}
	return func(yield func(llm.StreamEvent, error) bool) {
		_, _, _, err := p.Complete(ctx, req)
		yield(llm.StreamEvent{Type: llm.SEMessageStop}, err)
	}
}

func TestMemoryRecallRefreshesEachPrimaryCallWithoutMutatingSession(t *testing.T) {
	old := MemoryRecall
	t.Cleanup(func() { MemoryRecall = old })
	identity := RunInfo{TaskID: 42, ExplorationID: 51, IntentID: 61}
	calls := 0
	MemoryRecall = func(ctx context.Context, got RunInfo, role string, req llm.CompletionRequest) (MemoryRecallResult, error) {
		calls++
		if got != identity || role != "worker" || memcontext.ByteBudget(ctx) != 1600 {
			t.Fatalf("untrusted/wrong runtime identity or budget: %+v %q", got, role)
		}
		return MemoryRecallResult{Block: fmt.Sprintf("remember revision %d", calls), References: []memcontext.Reference{{ID: 9, Version: int64(calls)}}, Revalidate: func(context.Context) error { return nil }}, nil
	}
	var requests []llm.CompletionRequest
	provider := memoryFixtureProvider{complete: func(ctx context.Context, req llm.CompletionRequest) (llm.Message, string, llm.Usage, error) {
		requests = append(requests, req)
		if len(req.System) == 2 {
			if !memcontext.MetadataOnly(ctx) || len(memcontext.References(ctx)) != 1 {
				t.Fatal("memory request lost privacy/dependency context")
			}
		} else if memcontext.MetadataOnly(ctx) {
			t.Fatal("auxiliary call inherited main memory context")
		}
		return llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{llm.TextBlock("done")}}, "stop", llm.Usage{OutputTokens: 1}, nil
	}}
	opts := agentcore.Options{Provider: provider}
	ctx := attachMemory(WithRunInfo(t.Context(), identity), &opts, "worker")
	backing := []string{"host rules", "untouched backing capacity", "third"}
	req := llm.CompletionRequest{System: backing[:1], Messages: []llm.Message{llm.UserText("do current task")}}
	for i := 0; i < 2; i++ {
		if _, _, _, err := opts.Deps.CallModelSync(ctx, req); err != nil {
			t.Fatal(err)
		}
	}
	if calls != 2 || len(requests) != 2 || !strings.Contains(requests[0].System[1], "revision 1") || !strings.Contains(requests[1].System[1], "revision 2") {
		t.Fatal("primary calls reused stale session-fixed memory")
	}
	if len(req.System) != 1 || backing[1] != "untouched backing capacity" || ctx.Err() != nil {
		t.Fatal("memory wrapper mutated the harness session or cancelled its parent")
	}
	// Compaction/title/extraction use opts.Provider directly, not QueryDeps.
	if _, _, _, err := opts.Provider.Complete(ctx, req); err != nil || calls != 2 {
		t.Fatal("auxiliary provider invocation recursively recalled memory")
	}
}

func TestMemoryWrapperPreservesRestrictedRolesAndMissingIdentity(t *testing.T) {
	old := MemoryRecall
	t.Cleanup(func() { MemoryRecall = old })
	MemoryRecall = func(context.Context, RunInfo, string, llm.CompletionRequest) (MemoryRecallResult, error) {
		t.Fatal("restricted/no-identity call attempted global memory retrieval")
		return MemoryRecallResult{}, nil
	}
	provider := memoryFixtureProvider{complete: func(context.Context, llm.CompletionRequest) (llm.Message, string, llm.Usage, error) {
		return llm.Message{}, "", llm.Usage{}, nil
	}}
	for _, role := range []string{db.CredentialAgentKey, "asset_mapper", "worker.intranet", "memory", ""} {
		opts := agentcore.Options{Provider: provider}
		attachMemory(WithRunInfo(t.Context(), RunInfo{TaskID: 1}), &opts, role)
		if opts.Deps.CallModel != nil || opts.Deps.CallModelSync != nil {
			t.Fatalf("restricted role %q gained memory hook", role)
		}
	}
	opts := agentcore.Options{Provider: provider}
	attachMemory(t.Context(), &opts, "worker")
	if opts.Deps.CallModelSync != nil {
		t.Fatal("identityless call gained global memory access")
	}
}

func TestMemoryRecallFailuresContinueWithoutCrossScopeFallback(t *testing.T) {
	old := MemoryRecall
	t.Cleanup(func() { MemoryRecall = old })
	for _, failure := range []bool{true, false} {
		MemoryRecall = func(context.Context, RunInfo, string, llm.CompletionRequest) (MemoryRecallResult, error) {
			if failure {
				return MemoryRecallResult{}, errors.New("private database failure")
			}
			return MemoryRecallResult{Block: "unverifiable memory", References: []memcontext.Reference{{ID: 1, Version: 1}}}, nil
		}
		provider := memoryFixtureProvider{complete: func(ctx context.Context, req llm.CompletionRequest) (llm.Message, string, llm.Usage, error) {
			if len(req.System) != 1 || memcontext.MetadataOnly(ctx) {
				t.Fatal("failed retrieval injected an unverified/global fallback")
			}
			return llm.Message{}, "stop", llm.Usage{}, nil
		}}
		opts := agentcore.Options{Provider: provider}
		ctx := attachMemory(WithRunInfo(t.Context(), RunInfo{SessionID: "conv-8"}), &opts, "assistant")
		if _, _, _, err := opts.Deps.CallModelSync(ctx, llm.CompletionRequest{System: []string{"rules"}}); err != nil {
			t.Fatalf("optional memory failure broke the authorized task: %v", err)
		}
	}
}

func TestMemoryStaleDiagnosisSurvivesSDKCancellation(t *testing.T) {
	old := MemoryRecall
	t.Cleanup(func() { MemoryRecall = old })
	MemoryRecall = func(context.Context, RunInfo, string, llm.CompletionRequest) (MemoryRecallResult, error) {
		return MemoryRecallResult{Block: "canary", References: []memcontext.Reference{{ID: 1, Version: 1}}, Revalidate: func(context.Context) error { return errors.New("revoked") }}, nil
	}
	provider := memoryFixtureProvider{complete: func(ctx context.Context, req llm.CompletionRequest) (llm.Message, string, llm.Usage, error) {
		if !memcontext.IsStale(memcontext.Validate(ctx)) {
			t.Fatal("invalid memory admitted")
		}
		return llm.Message{}, "", llm.Usage{}, fmt.Errorf("SDK HTTP request cancelled: %w", ctx.Err())
	}}
	for _, streaming := range []bool{false, true} {
		opts := agentcore.Options{Provider: provider}
		ctx := attachMemory(WithRunInfo(t.Context(), RunInfo{TaskID: 2}), &opts, "worker")
		var err error
		if streaming {
			for _, callErr := range opts.Deps.CallModel(ctx, llm.CompletionRequest{}) {
				err = callErr
			}
		} else {
			_, _, _, err = opts.Deps.CallModelSync(ctx, llm.CompletionRequest{})
		}
		if !memcontext.IsStale(err) || llmguard.Retryable(err) || ctx.Err() != nil {
			t.Fatalf("stale failure became provider retry/task cancellation: %v", err)
		}
	}
}

func TestMemoryFullFormattedBlockHonorsContextBudget(t *testing.T) {
	old := MemoryRecall
	t.Cleanup(func() { MemoryRecall = old })
	MemoryRecall = func(context.Context, RunInfo, string, llm.CompletionRequest) (MemoryRecallResult, error) {
		return MemoryRecallResult{Block: strings.Repeat("a", 100), References: []memcontext.Reference{{ID: 1, Version: 1}}, Revalidate: func(context.Context) error { return nil }}, nil
	}
	provider := memoryFixtureProvider{complete: func(ctx context.Context, req llm.CompletionRequest) (llm.Message, string, llm.Usage, error) {
		if len(req.System) != 1 || memcontext.MetadataOnly(ctx) {
			t.Fatal("formatting/references overhead escaped the 5% context budget")
		}
		return llm.Message{}, "", llm.Usage{}, nil
	}}
	opts := agentcore.Options{Provider: provider, Compaction: &compaction.Config{ContextWindow: 4000}}
	ctx := attachMemory(WithRunInfo(t.Context(), RunInfo{TaskID: 1}), &opts, "worker")
	if _, _, _, err := opts.Deps.CallModelSync(ctx, llm.CompletionRequest{System: []string{"host"}}); err != nil {
		t.Fatal(err)
	}
}

type memoryRoundTripFunc func(*http.Request) (*http.Response, error)

func (f memoryRoundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }

func TestMemoryAdmissionRunsAfterQueueAndBeforeWire(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		limiter := &RequestLimiter{}
		if err := limiter.configure(1, 0); err != nil {
			t.Fatal(err)
		}
		if err := limiter.wait(t.Context()); err != nil {
			t.Fatal(err)
		}
		start := time.Now()
		var active atomic.Bool
		active.Store(true)
		var checkedAt time.Time
		ctx, release := memcontext.WithRecall(t.Context(), []memcontext.Reference{{ID: 1, Version: 1}}, func(context.Context) error {
			checkedAt = time.Now()
			if !active.Load() {
				return errors.New("deleted while queued")
			}
			return nil
		})
		defer release()
		go func() { time.Sleep(500 * time.Millisecond); active.Store(false) }()
		sent := 0
		client := &http.Client{Transport: rateLimitedTransport{limiter: limiter, base: quotaAwareTransport{base: memoryRoundTripFunc(func(*http.Request) (*http.Response, error) {
			sent++
			return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader("ok"))}, nil
		})}}}
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://fixture.invalid/v1/messages", strings.NewReader("memory canary"))
		if err != nil {
			t.Fatal(err)
		}
		_, err = client.Do(req)
		if !errors.Is(err, context.Canceled) || !memcontext.IsStale(memcontext.ErrorFrom(ctx)) || sent != 0 || checkedAt.Before(start.Add(time.Second)) {
			t.Fatalf("queued stale body reached wire: err=%v sent=%d checkedAt=%v", err, sent, checkedAt.Sub(start))
		}
	})
}

func TestMemoryAdmissionRevalidatesEveryActualHTTPAttempt(t *testing.T) {
	active, checks, sent := true, 0, 0
	ctx, release := memcontext.WithRecall(t.Context(), []memcontext.Reference{{ID: 1, Version: 1}}, func(context.Context) error {
		checks++
		if !active {
			return errors.New("source version changed")
		}
		return nil
	})
	defer release()
	transport := quotaAwareTransport{base: memoryRoundTripFunc(func(*http.Request) (*http.Response, error) {
		sent++
		return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader("ok"))}, nil
	})}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://fixture.invalid/v1/messages", strings.NewReader("canary"))
	if err != nil {
		t.Fatal(err)
	}
	resp, err := transport.RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	active = false
	_, err = transport.RoundTrip(req)
	if !memcontext.IsStale(err) || checks != 2 || sent != 1 {
		t.Fatalf("actual retry reused stale admission: checks=%d sent=%d err=%v", checks, sent, err)
	}
}

// Exercise the real SDK retry stacks against a synthetic gateway. Some formats
// return the first 429 directly; in that case an explicit second invocation
// represents the outer router's retry. Neither path may send the old body twice.
func TestMemoryInvalidationBlocksRealSDKRetryAcrossFormats(t *testing.T) {
	for _, format := range []string{"anthropic", "openai", "openai-responses"} {
		for _, streaming := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/stream=%v", format, streaming), func(t *testing.T) {
				var active atomic.Bool
				active.Store(true)
				var sent, checked atomic.Int64
				gateway := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					sent.Add(1)
					_, _ = io.Copy(io.Discard, r.Body)
					active.Store(false)
					w.Header().Set("Content-Type", "application/json")
					w.WriteHeader(http.StatusTooManyRequests)
					_, _ = io.WriteString(w, `{"error":{"message":"rate limited","type":"rate_limit_error"}}`)
				}))
				defer gateway.Close()
				cfg := ConfigFrom(format, "fixture", gateway.URL+"/v1", "fixture-key", "")
				cfg.Retry.ConnectAttempts = 3
				cfg.Retry.ConnectInterval = time.Millisecond
				cfg.Retry.EmptyAttempts = -1
				provider, err := cfg.NewProvider()
				if err != nil {
					t.Fatal(err)
				}
				parent, stopParent := context.WithTimeout(t.Context(), 5*time.Second)
				defer stopParent()
				ctx, release := memcontext.WithRecall(parent, []memcontext.Reference{{ID: 7, Version: 2}}, func(context.Context) error {
					checked.Add(1)
					if !active.Load() {
						return errors.New("source revoked after initial request")
					}
					return nil
				})
				defer release()
				request := llm.CompletionRequest{System: []string{memcontext.FormatBlock("canary", memcontext.References(ctx))}, Messages: []llm.Message{llm.UserText("fixture")}, MaxTokens: 32}
				call := func() error {
					if !streaming {
						_, _, _, callErr := provider.Complete(ctx, request)
						return callErr
					}
					var callErr error
					for _, eventErr := range provider.Stream(ctx, request) {
						if eventErr != nil {
							callErr = eventErr
						}
					}
					return callErr
				}
				if err := call(); err == nil {
					t.Fatal("synthetic rate-limit failure was lost")
				}
				if memcontext.ErrorFrom(ctx) == nil {
					if err := call(); err == nil {
						t.Fatal("router retry silently succeeded with stale memory")
					}
				}
				if !memcontext.IsStale(memcontext.ErrorFrom(ctx)) || sent.Load() != 1 || checked.Load() != 2 || parent.Err() != nil {
					t.Fatalf("SDK replayed stale body: sent=%d checked=%d error=%v parent=%v", sent.Load(), checked.Load(), memcontext.ErrorFrom(ctx), parent.Err())
				}
			})
		}
	}
}
