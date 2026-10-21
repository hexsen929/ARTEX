//go:build ignore

package tool

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
)

// This host implementation deliberately does not call CheckExecution itself.
// Dispatch must protect arbitrary CoreTool implementations, not only Build.
type unguardedExecutionTool struct {
	CoreTool
	calls int
}

func (t *unguardedExecutionTool) Call(context.Context, json.RawMessage, *ToolContext) (Result, error) {
	t.calls++
	return Text("executed"), nil
}

func TestExecuteExtraToolCannotBypassInheritedExecutionGuard(t *testing.T) {
	target := &unguardedExecutionTool{CoreTool: echoTool()}
	reg := NewRegistry(target)
	exec := NewExecuteExtraTool(reg, NewUnlockSet("echo", ExecuteExtraToolName))
	reg.Add(exec)
	ctx := WithExecutionGuard(context.Background(), func(name string) error {
		if name == "echo" {
			return errors.New("checkpoint only")
		}
		return nil
	})
	// A child guard may narrow admission, but may not overwrite the parent's.
	ctx = WithExecutionGuard(ctx, func(string) error { return nil })
	for _, input := range []string{
		`{"tool_name":"echo","params":{"msg":"hi"}}`,
		`{"tool_name":"ExecuteExtraTool","params":{"tool_name":"echo","params":{"msg":"hi"}}}`,
	} {
		res, err := exec.Call(ctx, []byte(input), nil)
		if err != nil || !res.IsError || target.calls != 0 {
			t.Fatalf("indirect invocation bypassed guard: res=%+v err=%v calls=%d", res, err, target.calls)
		}
	}
	res, err := exec.Call(context.Background(), []byte(`{"tool_name":"echo","params":{"msg":"hi"}}`), nil)
	if err != nil || res.IsError || target.calls != 1 {
		t.Fatalf("ordinary call changed: res=%+v err=%v calls=%d", res, err, target.calls)
	}
}

func TestInvokeRejectsCanceledExecutionBeforeHostCallback(t *testing.T) {
	target := &unguardedExecutionTool{CoreTool: echoTool()}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	res, err := Invoke(ctx, target, []byte(`{"msg":"hi"}`), nil)
	if err != nil || !res.IsError || target.calls != 0 {
		t.Fatalf("res=%+v err=%v calls=%d", res, err, target.calls)
	}
}
