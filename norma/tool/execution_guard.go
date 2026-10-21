package tool

import (
	"context"
	"fmt"
)

// ExecutionGuard is a host-owned execution restriction. It is checked immediately
// before dispatch, including indirect calls through ExecuteExtraTool. Guards must
// be concurrency-safe; returning an error rejects the call before its callback.
type ExecutionGuard func(name string) error

type executionGuardKey struct{}

type executionGuards struct {
	guard  ExecutionGuard
	parent *executionGuards
}

// WithExecutionGuard adds a restriction without replacing inherited restrictions.
// A child agent or an indirect dispatcher cannot broaden its parent's policy.
func WithExecutionGuard(ctx context.Context, guard ExecutionGuard) context.Context {
	if guard == nil {
		return ctx
	}
	parent, _ := ctx.Value(executionGuardKey{}).(*executionGuards)
	return context.WithValue(ctx, executionGuardKey{}, &executionGuards{guard: guard, parent: parent})
}

// CheckExecution verifies that a new tool call can be admitted. Calls admitted
// before a host changes its policy may finish; subsequent dispatches are checked
// again. Context cancellation always prevents a new call.
func CheckExecution(ctx context.Context, name string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	guards, _ := ctx.Value(executionGuardKey{}).(*executionGuards)
	for g := guards; g != nil; g = g.parent {
		if err := g.guard(name); err != nil {
			return fmt.Errorf("tool %q is not permitted: %w", name, err)
		}
	}
	return nil
}

// Invoke is the guarded dispatch boundary for registry tools, including host
// implementations of CoreTool that are not constructed with Build.
func Invoke(ctx context.Context, t CoreTool, input []byte, tc *ToolContext) (Result, error) {
	if err := CheckExecution(ctx, t.Name()); err != nil {
		return Errorf(err.Error()), nil
	}
	return t.Call(ctx, input, tc)
}
