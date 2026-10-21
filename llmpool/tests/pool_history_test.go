//go:build ignore

package llmpool

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/Autumn-27/norma/llm"
)

func TestDeterministicHistoryErrorDoesNotFailoverOrAlterHealth(t *testing.T) {
	for _, stream := range []bool{true, false} {
		t.Run(map[bool]string{true: "stream", false: "complete"}[stream], func(t *testing.T) {
			bad := &llm.HistoryError{Reason: "missing thinking", Recoverable: true}
			a := &fakeProv{errs: []error{fmt.Errorf("wrapped: %w", bad)}}
			b := okProv("fallback", "ok")
			reg := NewRegistry(nil, nil)
			reg.Trip(1, "prior 429", false)
			before := reg.Get(1)
			p := New([]*Member{member(1, "a", 10, a), member(2, "b", 5, b)}, reg)
			var err error
			if stream {
				_, err = drain(p.Stream(context.Background(), llm.CompletionRequest{}))
			} else {
				_, _, _, err = p.Complete(context.Background(), llm.CompletionRequest{})
			}
			if !errors.Is(err, bad) || a.calls != 1 || b.calls != 0 {
				t.Fatalf("calls a=%d b=%d error=%v", a.calls, b.calls, err)
			}
			if after := reg.Get(1); after.Fails != before.Fails || after.Trips != before.Trips {
				t.Fatalf("deterministic error changed health: before=%+v after=%+v", before, after)
			}
		})
	}
}
