package agent

import "context"

type explanationOnlyKey struct{}

// WithExplanationOnly is a host-only ceiling for a requested explanation of a
// terminal/unbound historical conversation. It is not a prompt string or a
// model-selectable option. Chat preserves model/history attribution but must
// not assemble tools, dependencies, networking, compaction or settlement runs.
func WithExplanationOnly(ctx context.Context) context.Context {
	return context.WithValue(ctx, explanationOnlyKey{}, true)
}

func ExplanationOnly(ctx context.Context) bool {
	v, _ := ctx.Value(explanationOnlyKey{}).(bool)
	return v
}
