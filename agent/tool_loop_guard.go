package agent

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"

	"github.com/Autumn-27/norma/agentcore"
	"github.com/Autumn-27/norma/harness"
	"github.com/Autumn-27/norma/llm"
)

const broadTrafficCallLimit = 12
const trafficCallLimit = 40
const repeatedContractFailureLimit = 3
const contractFailureLimit = 8

var ErrToolLoopGuard = errors.New("tool_loop_guard")

// Contains only host-selected categories, never target URLs, arguments or
// provider errors. This is a blocked run, not a completed task or a model fault.
type toolLoopError struct{ category string }

func (e *toolLoopError) Unwrap() error { return ErrToolLoopGuard }
func (e *toolLoopError) Error() string {
	if e.category == "traffic_read_budget" {
		return "tool_loop_guard: 本轮流量检索达到读取上限，已停止继续翻页；已有记录保留，尚未查完，不表示任务完成。请人工核对检索目的与进度后再处理。"
	}
	return "tool_loop_guard: 同类参数或恢复条件错误反复出现，本轮已停止；已有记录保留，尚未完成。请先处理明确的缺失字段或人工恢复条件，不重放同一请求。"
}

type toolLoopGuard struct {
	inner                     harness.HookRunner
	cancel                    context.CancelCauseFunc
	mu                        sync.Mutex
	searches, broad, failures int
	repeated                  map[string]int
	stopped                   error
}

func attachToolLoopGuard(ctx context.Context, opts *agentcore.Options) (context.Context, func()) {
	child, cancel := context.WithCancelCause(ctx)
	opts.Hooks = &toolLoopGuard{inner: opts.Hooks, cancel: cancel, repeated: make(map[string]int)}
	return child, func() { cancel(nil) }
}

func toolLoopCause(ctx context.Context) error {
	cause := context.Cause(ctx)
	if errors.Is(cause, ErrToolLoopGuard) {
		return cause
	}
	return nil
}

func (g *toolLoopGuard) stopLocked(category string) string {
	if g.stopped == nil {
		g.stopped = &toolLoopError{category: category}
		g.cancel(g.stopped)
	}
	return g.stopped.Error()
}

func (g *toolLoopGuard) PreToolUse(ctx context.Context, name string, input []byte) (bool, string, []byte) {
	if ctx.Err() != nil {
		return true, "本轮已停止，工具未执行。", nil
	}
	var rewritten []byte
	if g.inner != nil {
		blocked, message, changed := g.inner.PreToolUse(ctx, name, input)
		if blocked {
			return blocked, message, changed
		}
		rewritten = changed
		if changed != nil {
			input = changed
		}
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.stopped != nil {
		return true, g.stopped.Error(), nil
	}
	if name == "traffic_search" {
		var args struct {
			Contains string `json:"contains"`
			Body     string `json:"body_contains"`
		}
		_ = json.Unmarshal(input, &args)
		broad := strings.TrimSpace(args.Contains) == "" && strings.TrimSpace(args.Body) == ""
		if g.searches >= trafficCallLimit || (broad && g.broad >= broadTrafficCallLimit) {
			return true, g.stopLocked("traffic_read_budget"), nil
		}
		g.searches++
		if broad {
			g.broad++
		}
	}
	return false, "", rewritten
}

func contractFailureCode(name string, result []byte) string {
	if name != "add_intent" && name != "set_finding_verification" {
		return ""
	}
	for _, code := range []string{"verification_missing_fields", "intent_missing_fields", "intent_mixed_modes", "continuation_requires_operator"} {
		if strings.Contains(string(result), code) {
			return code
		}
	}
	return ""
}

func contractFailureTarget(input []byte) string {
	var args map[string]json.RawMessage
	if json.Unmarshal(input, &args) != nil {
		return "invalid"
	}
	for _, key := range []string{"continue_intent_id", "finding_id"} {
		if id := args[key]; len(id) > 0 {
			var text string
			if json.Unmarshal(id, &text) != nil {
				text = string(id)
			}
			return fmt.Sprintf("%x", sha256.Sum256([]byte(strings.TrimSpace(text))))
		}
	}
	return "new_or_batch"
}

func (g *toolLoopGuard) PostToolUse(ctx context.Context, name string, input, result []byte, isErr bool) {
	if g.inner != nil {
		g.inner.PostToolUse(ctx, name, input, result, isErr)
	}
	if !isErr || ctx.Err() != nil {
		return
	}
	code := contractFailureCode(name, result)
	if code == "" {
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	g.failures++
	key := name + ":" + code + ":" + contractFailureTarget(input)
	g.repeated[key]++
	if g.repeated[key] >= repeatedContractFailureLimit || g.failures >= contractFailureLimit {
		g.stopLocked("repeated_contract_failure")
	}
}

func (g *toolLoopGuard) Stop(ctx context.Context, messages []llm.Message) (bool, []string, string) {
	if ctx.Err() != nil {
		return false, nil, ""
	}
	if g.inner != nil {
		return g.inner.Stop(ctx, messages)
	}
	return false, nil, ""
}
