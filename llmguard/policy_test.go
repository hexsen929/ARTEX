package llmguard

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
)

func TestClassification(t *testing.T) {
	for _, tc := range []struct {
		s     string
		k     Kind
		retry bool
		hard  bool
	}{
		{`status 503: {"code":"model_not_found","message":"No available channel"}`, ModelUnavailable, false, true},
		{"status 521: gateway down", ProviderFailure, true, false}, {"status 429: rate limit", RateLimit, true, false},
		{"HTTP 401: invalid key", Auth, false, true}, {"status 402: payment", Quota, false, true},
		{"status 400: context_length_exceeded", ContextLimit, false, false}, {"status 422: invalid", InvalidRequest, false, false},
		{"unexpected EOF", TransportFailure, true, false}, {ErrRetryBudget.Error(), RetryBudget, false, false},
		{"llm_cooling_down", CoolingDown, false, false},
	} {
		t.Run(tc.s, func(t *testing.T) {
			e := errors.New(tc.s)
			if Classify(e) != tc.k || Retryable(e) != tc.retry || HardFailure(e) != tc.hard {
				t.Fatalf("classification %v retry=%v hard=%v", Classify(e), Retryable(e), HardFailure(e))
			}
		})
	}
}
func TestBudgetSharedAndConcurrent(t *testing.T) {
	ctx := WithBudget(context.Background(), 3)
	ctx = WithBudget(ctx, 100)
	var success atomic.Int64
	var wg sync.WaitGroup
	for range 20 {
		wg.Go(func() {
			if TakeHTTP(ctx) == nil {
				success.Add(1)
			}
		})
	}
	wg.Wait()
	if success.Load() != 3 {
		t.Fatalf("budget allowed %d requests", success.Load())
	}
	if !errors.Is(TakeHTTP(ctx), ErrRetryBudget) {
		t.Fatal("budget reset")
	}
	if TakeHTTP(context.Background()) != nil {
		t.Fatal("unbudgeted probes should work")
	}
	cancelCtx, cancel := context.WithCancel(WithBudget(context.Background(), 1))
	cancel()
	if !errors.Is(TakeHTTP(cancelCtx), context.Canceled) {
		t.Fatal("cancellation not preserved")
	}
}
