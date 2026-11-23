package agent

import (
	"context"
	"iter"
	"strings"
	"time"

	"github.com/Autumn-27/norma/agentcore"
	"github.com/Autumn-27/norma/harness"
	"github.com/Autumn-27/norma/llm"
)

const readHandoffNotice = "traffic_read_budget: 本轮读取预算已用完，系统正在自动整理已有记录和未完成项；停止新的工具调用，不需要用户逐页核对。未查完不表示任务完成。"
const readHandoffInstruction = "【本轮自动整理】读取预算耗尽。仅根据当前已保存的对话和工具结果，用简短文本交接已读记录、可确认的观察、未知项与停止位置。不要执行或请求任何工具，不补做测试、不编造事实或声明目标完成，不要求用户逐页核对。Planner会读取本交接和已有活动继续协调；不得原样重派同一宽查询。只输出交接文本。"
const readHandoffFallback = "本轮读取预算耗尽，已停止继续读取。已有工具结果与进度保留在执行记录，尚未查完。系统未生成可用的自动摘要，Planner可直接读取已有记录处理未完成项；不需要用户逐页核对。"

func (g *toolLoopGuard) wrappingUp() bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.readBudget
}

// A read limit is a normal handoff, not an operator/approval failure. Use one
// bounded, tools-free primary completion with the existing model/admission
// wrappers. Never execute model-requested tools during this phase.
func (g *toolLoopGuard) wrapModelCalls(opts *agentcore.Options) {
	if opts.Provider == nil {
		return
	}
	complete, stream := opts.Deps.CallModelSync, opts.Deps.CallModel
	if complete == nil {
		complete = opts.Provider.Complete
	}
	if stream == nil {
		stream = opts.Provider.Stream
	}
	wrap := func(ctx context.Context, req llm.CompletionRequest) (llm.Message, string, llm.Usage, error) {
		g.mu.Lock()
		if g.handoffTaken {
			text := g.handoffText
			g.mu.Unlock()
			if text == "" {
				text = readHandoffFallback
			}
			return llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{llm.TextBlock(text)}}, "end_turn", llm.Usage{}, ctx.Err()
		}
		g.handoffTaken = true
		g.mu.Unlock()
		req.System = append(append([]string(nil), req.System...), readHandoffInstruction)
		req.Tools = []llm.ToolSchema{}
		if req.MaxTokens <= 0 || req.MaxTokens > 1200 {
			req.MaxTokens = 1200
		}
		req.Thinking = "disabled"
		callCtx, cancel := context.WithTimeout(ctx, 25*time.Second)
		defer cancel()
		message, stop, usage, err := complete(callCtx, req)
		if ctx.Err() != nil {
			return llm.Message{}, "", usage, ctx.Err()
		}
		text := strings.TrimSpace(message.Text())
		if err != nil || callCtx.Err() != nil || message.Role != llm.RoleAssistant || len(message.ToolUses()) != 0 || text == "" || len(text) > 12000 || stop == "max_tokens" || stop == "length" {
			text = readHandoffFallback
		}
		g.mu.Lock()
		g.handoffText = text
		g.mu.Unlock()
		return llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{llm.TextBlock(text)}}, "end_turn", usage, nil
	}
	opts.Deps.CallModelSync = func(ctx context.Context, req llm.CompletionRequest) (llm.Message, string, llm.Usage, error) {
		if g.wrappingUp() {
			return wrap(ctx, req)
		}
		return complete(ctx, req)
	}
	opts.Deps.CallModel = func(ctx context.Context, req llm.CompletionRequest) iter.Seq2[llm.StreamEvent, error] {
		return func(yield func(llm.StreamEvent, error) bool) {
			if !g.wrappingUp() {
				for event, err := range committedToolStream(ctx, stream(ctx, req)) {
					if !yield(event, err) {
						return
					}
				}
				return
			}
			message, stop, usage, err := wrap(ctx, req)
			if err != nil {
				yield(llm.StreamEvent{}, err)
				return
			}
			if !yield(llm.StreamEvent{Type: llm.SEMessageStart, Usage: llm.Usage{InputTokens: usage.InputTokens, CacheReadTokens: usage.CacheReadTokens, CacheWriteTokens: usage.CacheWriteTokens}}, nil) {
				return
			}
			if !yield(llm.StreamEvent{Type: llm.SETextDelta, Text: message.Text()}, nil) {
				return
			}
			if !yield(llm.StreamEvent{Type: llm.SEMessageDelta, StopReason: stop, Usage: llm.Usage{OutputTokens: usage.OutputTokens}}, nil) {
				return
			}
			yield(llm.StreamEvent{Type: llm.SEMessageStop}, nil)
		}
	}
}

func readBudgetReached(ctx context.Context) bool {
	g, _ := ctx.Value(toolLoopGuardKey{}).(*toolLoopGuard)
	return g != nil && ctx.Err() == nil && g.wrappingUp()
}

func readBudgetTerminal(ctx context.Context, term *harness.Terminal) *harness.Terminal {
	if !readBudgetReached(ctx) || term == nil || term.Err != nil {
		return term
	}
	copy := *term
	copy.Reason, copy.Err = harness.ReasonMaxTurns, ErrTrafficReadBudget
	if strings.TrimSpace(copy.Text) == "" {
		copy.Text = readHandoffFallback
	}
	copy.Text = "【读取预算已结束，自动交接】\n" + copy.Text
	return &copy
}
