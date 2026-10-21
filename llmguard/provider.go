package llmguard

import (
	"context"
	"github.com/Autumn-27/norma/llm"
	"iter"
)

type budgetProvider struct{ inner llm.Provider }

// Wrap also protects calls outside task runtimes (chat/report/model probes).
// Nested wrappers reuse their caller's budget instead of resetting it.
func Wrap(p llm.Provider) llm.Provider { return budgetProvider{inner: p} }
func (p budgetProvider) Stream(ctx context.Context, r llm.CompletionRequest) iter.Seq2[llm.StreamEvent, error] {
	return func(yield func(llm.StreamEvent, error) bool) {
		for ev, err := range p.inner.Stream(WithBudget(ctx, DefaultHTTPAttempts), r) {
			if !yield(ev, err) {
				return
			}
		}
	}
}
func (p budgetProvider) Complete(ctx context.Context, r llm.CompletionRequest) (llm.Message, string, llm.Usage, error) {
	return p.inner.Complete(WithBudget(ctx, DefaultHTTPAttempts), r)
}
