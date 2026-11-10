package agent

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"

	"github.com/Autumn-27/artex/db"
	"github.com/Autumn-27/norma/agentcore"
	"github.com/Autumn-27/norma/harness"
	"github.com/Autumn-27/norma/llm"
)

const broadTrafficCallLimit = 12
const trafficCallLimit = 40
const repeatedContractFailureLimit = 3
const contractFailureLimit = 8

var ErrToolLoopGuard = errors.New("tool_loop_guard")
var ErrTrafficReadBudget = errors.New("traffic_read_budget")

// Contains only host-selected categories, never target URLs, arguments or
// provider errors. This is a blocked run, not a completed task or a model fault.
type toolLoopError struct {
	category     string
	verification *db.VerificationFieldFailure
}

func (e *toolLoopError) Unwrap() error { return ErrToolLoopGuard }
func (e *toolLoopError) Error() string {
	if e.verification != nil {
		return fmt.Sprintf("tool_loop_guard: set_finding_verification 更新记录 #%d 缺少 %s；本轮还存在其它工具错误或未返回操作，系统保留各项原因与已有进度，不重放原请求、不要求用户猜测内部字段。", e.verification.FindingID, strings.Join(e.verification.Fields, ", "))
	}
	return "tool_loop_guard: 工具请求重复失败，本轮已停止；具体原因保留在最后的工具返回，已有记录保留，尚未完成。系统需核对错误类型与未完成操作，不重放同一请求。"
}

// Expose only the host-constructed diagnostic, never arbitrary provider text.
func ToolLoopDiagnostic(err error) string {
	var stopped *toolLoopError
	if errors.As(err, &stopped) {
		return stopped.Error()
	}
	return "工具请求重复失败，本轮已停止；具体原因保留在最后的工具返回中，已有进度保留，不自动重派"
}

type toolLoopGuard struct {
	inner                     harness.HookRunner
	cancel                    context.CancelCauseFunc
	mu                        sync.Mutex
	searches, broad, failures int
	repeated                  map[string]int
	stopped                   error
	broadDenied               int
	readBudget                bool
	handoffTaken              bool
	handoffText               string
	hardContractFailure       bool
}

type toolLoopGuardKey struct{}

func attachToolLoopGuard(ctx context.Context, opts *agentcore.Options) (context.Context, func()) {
	child, cancel := context.WithCancelCause(ctx)
	guard := &toolLoopGuard{inner: opts.Hooks, cancel: cancel, repeated: make(map[string]int)}
	opts.Hooks = guard
	guard.wrapModelCalls(opts)
	return context.WithValue(child, toolLoopGuardKey{}, guard), func() { cancel(nil) }
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
	g.mu.Lock()
	settling := g.readBudget
	g.mu.Unlock()
	if settling {
		return true, readHandoffNotice, nil
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
	if g.readBudget {
		return true, readHandoffNotice, nil
	}
	if name == "traffic_search" {
		var args struct {
			Contains string `json:"contains"`
			Body     string `json:"body_contains"`
		}
		_ = json.Unmarshal(input, &args)
		broad := strings.TrimSpace(args.Contains) == "" && strings.TrimSpace(args.Body) == ""
		if g.searches >= trafficCallLimit {
			g.readBudget = true
			return true, readHandoffNotice, nil
		}
		if broad && g.broad >= broadTrafficCallLimit {
			g.broadDenied++
			if g.broadDenied >= 3 {
				g.readBudget = true
				return true, readHandoffNotice, nil
			}
			return true, "traffic_query_limit: 本轮宽泛流量翻页已停止；由系统自动整理已读结果。已有记录详情仍可读取，有明确筛选条件时可在本轮剩余预算内查询；不要继续仅改变页码、换主机重试，也不需要用户逐页核对。", nil
		}
		g.searches++
		if broad {
			g.broad++
		}
	}
	return false, "", rewritten
}

func contractFailureCode(name string, result []byte) string {
	if name != "add_intent" && name != "set_finding_verification" && name != "traffic_search" {
		return ""
	}
	if strings.HasPrefix(string(result), "Error: invalid tool input:") {
		return "tool_input_invalid"
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
	// Contract failures are observed from captureRunSession's actual result
	// events, which also include SDK schema rejections before hooks execute.
}

func observeToolLoopResult(ctx context.Context, name string, input, result []byte, isErr bool, otherToolsPending ...bool) {
	g, _ := ctx.Value(toolLoopGuardKey{}).(*toolLoopGuard)
	if g == nil {
		return
	}
	if !isErr || ctx.Err() != nil {
		return
	}
	code := contractFailureCode(name, result)
	if code == "" {
		if db.IsDeferredLookupRejection(name, input, result) {
			return
		}
		// An earlier permission, approval, transport or unknown tool failure
		// must not disappear behind a later ordinary field-validation handoff.
		g.mu.Lock()
		g.hardContractFailure = true
		g.mu.Unlock()
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	g.failures++
	var verification *db.VerificationFieldFailure
	if name == "set_finding_verification" {
		verification = db.ParseVerificationFieldFailure(input, result)
	}
	if verification == nil {
		g.hardContractFailure = true
	}
	key := name + ":" + code + ":" + contractFailureTarget(input)
	g.repeated[key]++
	if g.repeated[key] >= repeatedContractFailureLimit || g.failures >= contractFailureLimit {
		pending := len(otherToolsPending) > 0 && otherToolsPending[0]
		if verification != nil && !g.hardContractFailure && !pending && g.stopped == nil {
			g.stopped = &verificationHandoffError{failure: verification}
			g.cancel(g.stopped)
			return
		}
		if verification != nil && g.stopped == nil {
			g.stopped = &toolLoopError{category: "mixed_verification_failure", verification: verification}
			g.cancel(g.stopped)
			return
		}
		g.stopLocked("repeated_contract_failure")
	}
}

func (g *toolLoopGuard) Stop(ctx context.Context, messages []llm.Message) (bool, []string, string) {
	if ctx.Err() != nil {
		return false, nil, ""
	}
	g.mu.Lock()
	settling := g.readBudget
	g.mu.Unlock()
	if settling {
		return false, nil, ""
	}
	if g.inner != nil {
		return g.inner.Stop(ctx, messages)
	}
	return false, nil, ""
}
