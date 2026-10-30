package llmpool

import (
	"context"
	"errors"
	"iter"
	"reflect"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Autumn-27/artex/llmguard"
	"github.com/Autumn-27/norma/llm"
)

// Model the memory validator's deterministic cancellation contract without
// coupling routing to the implementation of the new memory package.
type memoryLocalAdmissionError struct{}

func (*memoryLocalAdmissionError) Error() string {
	return "memory_invalidated: synthetic admission failure"
}
func (*memoryLocalAdmissionError) Unwrap() error { return context.Canceled }

type memoryHealthProvider struct {
	calls    atomic.Int64
	stream   func(context.Context) iter.Seq2[llm.StreamEvent, error]
	complete func(context.Context) (llm.Message, string, llm.Usage, error)
}

func (p *memoryHealthProvider) Stream(ctx context.Context, _ llm.CompletionRequest) iter.Seq2[llm.StreamEvent, error] {
	p.calls.Add(1)
	return p.stream(ctx)
}

func (p *memoryHealthProvider) Complete(ctx context.Context, _ llm.CompletionRequest) (llm.Message, string, llm.Usage, error) {
	p.calls.Add(1)
	return p.complete(ctx)
}

func memoryHealthFixture(first llm.Provider) (*Pool, *Registry, *fakeProv, State, *atomic.Int64, *atomic.Int64) {
	var persisted, forgotten atomic.Int64
	reg := NewRegistry(func(int64, State) { persisted.Add(1) }, func(int64) { forgotten.Add(1) })
	before := State{
		Fails: 2, Trips: 1, OpenUntil: time.Unix(100, 0),
		LastError: "synthetic earlier provider failure", LastAt: time.Unix(90, 0),
	}
	reg.Restore(1, before)
	backup := okProv("unused backup", "must not be requested")
	pool := New([]*Member{member(1, "first", 10, first), member(2, "backup", 5, backup)}, reg)
	return pool, reg, backup, before, &persisted, &forgotten
}

func assertMemoryHealthUnchanged(t *testing.T, reg *Registry, before State, backup *fakeProv, persisted, forgotten *atomic.Int64) {
	t.Helper()
	if after := reg.Get(1); !reflect.DeepEqual(after, before) {
		t.Fatalf("local/cancelled request changed channel health: before=%+v after=%+v", before, after)
	}
	if backup.calls != 0 || persisted.Load() != 0 || forgotten.Load() != 0 {
		t.Fatalf("local/cancelled request routed or changed persisted health: backup=%d persist=%d forget=%d", backup.calls, persisted.Load(), forgotten.Load())
	}
}

func TestMemoryCancellationBeforePoolAdmissionDoesNotCallOrResetProviders(t *testing.T) {
	for _, streaming := range []bool{false, true} {
		t.Run(map[bool]string{false: "complete", true: "stream"}[streaming], func(t *testing.T) {
			ctx, cancel := context.WithCancelCause(t.Context())
			cause := &memoryLocalAdmissionError{}
			cancel(cause)
			first := okProv("must not be called", "unused")
			pool, reg, backup, before, persisted, forgotten := memoryHealthFixture(first)
			var err error
			if streaming {
				_, err = drain(pool.Stream(ctx, llm.CompletionRequest{}))
			} else {
				_, _, _, err = pool.Complete(ctx, llm.CompletionRequest{})
			}
			if !errors.Is(err, cause) || !errors.Is(err, context.Canceled) || first.calls != 0 {
				t.Fatalf("pre-cancelled request lost cause or called provider: err=%v first=%d", err, first.calls)
			}
			assertMemoryHealthUnchanged(t, reg, before, backup, persisted, forgotten)
		})
	}
}

func TestMemoryCancelledStreamNeverPassesHealthOnSilentOrSuccessfulLookingEnd(t *testing.T) {
	for _, ending := range []string{"silent", "message_stop", "partial_then_silent", "provider_error"} {
		t.Run(ending, func(t *testing.T) {
			ctx, cancel := context.WithCancelCause(t.Context())
			cause := &memoryLocalAdmissionError{}
			first := &memoryHealthProvider{stream: func(context.Context) iter.Seq2[llm.StreamEvent, error] {
				return func(yield func(llm.StreamEvent, error) bool) {
					if ending == "partial_then_silent" && !yield(llm.StreamEvent{Type: llm.SETextDelta, Text: "partial"}, nil) {
						return
					}
					cancel(cause)
					switch ending {
					case "message_stop":
						yield(llm.StreamEvent{Type: llm.SEMessageStop}, nil)
					case "provider_error":
						yield(llm.StreamEvent{}, errors.New("openai: status 503: synthetic concurrent failure"))
					}
				}
			}}
			pool, reg, backup, before, persisted, forgotten := memoryHealthFixture(first)
			text, err := drain(pool.Stream(ctx, llm.CompletionRequest{}))
			if !errors.Is(err, cause) || first.calls.Load() != 1 {
				t.Fatalf("silent cancellation became success or lost cause: err=%v calls=%d", err, first.calls.Load())
			}
			if ending == "partial_then_silent" && text != "partial" || ending != "partial_then_silent" && text != "" {
				t.Fatalf("cancelled stream changed already delivered output: %q", text)
			}
			assertMemoryHealthUnchanged(t, reg, before, backup, persisted, forgotten)
		})
	}
}

func TestMemoryCancelledCompleteNeverPassesOrTripsChannelHealth(t *testing.T) {
	for _, providerFails := range []bool{false, true} {
		t.Run(map[bool]string{false: "nil_error", true: "concurrent_provider_error"}[providerFails], func(t *testing.T) {
			ctx, cancel := context.WithCancelCause(t.Context())
			cause := &memoryLocalAdmissionError{}
			first := &memoryHealthProvider{complete: func(context.Context) (llm.Message, string, llm.Usage, error) {
				cancel(cause)
				if providerFails {
					return llm.Message{}, "", llm.Usage{}, errors.New("status 503: synthetic failure")
				}
				return llm.Message{}, "end_turn", llm.Usage{}, nil
			}}
			pool, reg, backup, before, persisted, forgotten := memoryHealthFixture(first)
			_, stop, _, err := pool.Complete(ctx, llm.CompletionRequest{})
			if !errors.Is(err, cause) || stop != "" || first.calls.Load() != 1 {
				t.Fatalf("cancelled completion looked successful: stop=%q err=%v calls=%d", stop, err, first.calls.Load())
			}
			assertMemoryHealthUnchanged(t, reg, before, backup, persisted, forgotten)
		})
	}
}

func TestMemoryLocalAndNonFailoverErrorsNeitherPassNorTripHealth(t *testing.T) {
	for _, original := range []error{&memoryLocalAdmissionError{}, errors.New("status 400: synthetic local validation"), llmguard.ErrRetryBudget} {
		for _, streaming := range []bool{false, true} {
			t.Run(original.Error()+map[bool]string{false: "/complete", true: "/stream"}[streaming], func(t *testing.T) {
				first := &fakeProv{errs: []error{original}}
				pool, reg, backup, before, persisted, forgotten := memoryHealthFixture(first)
				var err error
				if streaming {
					_, err = drain(pool.Stream(t.Context(), llm.CompletionRequest{}))
				} else {
					_, _, _, err = pool.Complete(t.Context(), llm.CompletionRequest{})
				}
				if !errors.Is(err, original) || first.calls != 1 {
					t.Fatalf("local error replaced or repeated: err=%v first=%d", err, first.calls)
				}
				assertMemoryHealthUnchanged(t, reg, before, backup, persisted, forgotten)
			})
		}
	}
}

func TestMemoryNonCancelledSuccessfulRequestsStillRestoreHealth(t *testing.T) {
	for _, streaming := range []bool{false, true} {
		t.Run(map[bool]string{false: "complete", true: "stream"}[streaming], func(t *testing.T) {
			first := okProv("recovered", "ok")
			pool, reg, backup, _, persisted, forgotten := memoryHealthFixture(first)
			var err error
			if streaming {
				_, err = drain(pool.Stream(t.Context(), llm.CompletionRequest{}))
			} else {
				_, _, _, err = pool.Complete(t.Context(), llm.CompletionRequest{})
			}
			if err != nil || first.calls != 1 || backup.calls != 0 || reg.Get(1) != (State{}) || persisted.Load() != 0 || forgotten.Load() != 1 {
				t.Fatalf("real success stopped restoring channel: err=%v state=%+v first=%d backup=%d persist=%d forget=%d", err, reg.Get(1), first.calls, backup.calls, persisted.Load(), forgotten.Load())
			}
		})
	}
}
