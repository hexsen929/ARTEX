package agent

import (
	"context"
	"github.com/Autumn-27/norma/agentcore"
	"strings"
)

// OperatorSupplement is host-owned. Imported references are bounded data, never
// authorization or instructions; auxiliary completions do not call this hook.
var OperatorSupplement func(context.Context, RunInfo, string) string

func attachOperatorSupplement(ctx context.Context, opts *agentcore.Options, role string) {
	if opts == nil || OperatorSupplement == nil || !memoryRoleAllowed(role) {
		return
	}
	b := strings.TrimSpace(OperatorSupplement(ctx, RunInfoFrom(ctx), role))
	if b == "" || len(b) > 40<<10 {
		return
	}
	opts.SystemPrompt = append(append([]string(nil), opts.SystemPrompt...), b)
}
