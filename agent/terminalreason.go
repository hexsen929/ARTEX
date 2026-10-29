package agent

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/Autumn-27/norma/harness"
)

// runTrace retains the latest tool call so an interrupted run can identify the
// operation that was still in flight.
type runTrace struct {
	startedAt time.Time
	id        string
	name      string
	input     string
	at        time.Time
	pending   bool
}

func (t *runTrace) start(id, name, input string) {
	t.id, t.name, t.input, t.at, t.pending = id, name, input, time.Now(), true
}

func (t *runTrace) done(id string) {
	if id == t.id {
		t.pending = false
	}
}

var reasonHint = map[harness.TerminalReason]string{
	harness.ReasonCompleted:         "模型正常结束了本轮，但没有留下文字总结；事实和资产以本轮工具调用记录为准",
	harness.ReasonMaxTurns:          "达到步数上限(MaxTurns)：已进入收尾流程；是否成功保存以工具记录为准，不代表意图已完成",
	harness.ReasonTimeout:           "达到单次运行的墙钟预算(MaxDuration)：到点停止新探索并尝试收尾；是否成功保存以工具记录为准，未完成部分保留待继续",
	harness.ReasonModelError:        "模型或 API 调用异常，本轮已中断；已完成的工具调用和事实记录保留，不代表此前工作无效；请结合底层错误和执行过程（get_worker_trace）判断原因与后续处理",
	harness.ReasonBlockingLimit:     "上下文长度达到硬上限，请求在发出前被拦截；应收窄意图粒度或压缩工具返回",
	harness.ReasonPromptTooLong:     "提示词过长且上下文压缩重试已经用尽，无法继续执行",
	harness.ReasonImageError:        "当前模型不支持本轮多模态内容；请切换支持视觉的模型或避免工具返回图片",
	harness.ReasonStopHookPrevented: "Stop 钩子阻止本轮结束，随后未能继续；请检查任务 Guard 规则是否过严",
	harness.ReasonHookStopped:       "工具或钩子主动停止继续执行，例如越界目标或禁用命令；请检查最后一条 tool_result 的拦截说明",
	harness.ReasonAbortedStreaming:  "运行在模型输出流式生成阶段被取消",
	harness.ReasonAbortedTools:      "运行在工具执行阶段被取消",
}

// terminalText renders a terminal event with no final text into a compact summary
// and a Markdown detail block.
func terminalText(ctx context.Context, term *harness.Terminal, tr *runTrace) (string, string) {
	term = normalizeCallTimeoutTerminal(ctx, term)
	reason := term.Reason
	aborted := reason == harness.ReasonAbortedStreaming || reason == harness.ReasonAbortedTools
	// Prompt may return ctx.Err directly without a terminal event. Preserve the
	// cancellation cause instead of falling back to an empty/unknown terminal reason.
	if reason == "" && ctx.Err() != nil {
		aborted = true
	}

	var sum string
	if aborted {
		_, short, _, ok := AbortReason(ctx)
		if !ok {
			short = "未能取得取消原因"
		}
		stage := "执行过程中"
		switch reason {
		case harness.ReasonAbortedStreaming:
			stage = "模型输出阶段"
		case harness.ReasonAbortedTools:
			stage = "工具执行阶段"
		}
		sum = "（运行被中断：" + short + "；停在" + stage + progressSuffix(term, tr) + "，未完成）"
	} else if reason == harness.ReasonMaxTurns || reason == harness.ReasonTimeout {
		sum = "（达到运行预算上限(" + string(reason) + ")，已收尾写回事实" + progressSuffix(term, tr) + "；本次无文字总结）"
	} else {
		hint := terminalReasonHint(reason)
		sum = "（无文字总结，终态 " + terminalReasonLabel(reason) + "：" + firstLine(hint, 80) + "）"
	}

	var b strings.Builder
	b.WriteString(sum)
	b.WriteString("\n\n")
	displayReason := terminalReasonLabel(reason)
	fmt.Fprintf(&b, "- **终态**: `%s` - %s\n", displayReason, terminalReasonHint(reason))
	if aborted {
		code, _, why, ok := AbortReason(ctx)
		if ok {
			fmt.Fprintf(&b, "- **中断原因** (`%s`): %s\n", code, why)
		} else {
			b.WriteString("- **中断原因**: 无法取得；取消方可能没有通过 context.WithCancelCause 附加具名原因\n")
		}
	}
	if term.Err != nil {
		fmt.Fprintf(&b, "- **底层错误**: `%v`\n", term.Err)
	}
	if aborted && strings.TrimSpace(term.Text) != "" {
		b.WriteString("- **取消前已生成的部分输出**:\n\n")
		b.WriteString(term.Text)
		b.WriteString("\n\n")
	}
	if term.Turns > 0 {
		fmt.Fprintf(&b, "- **已执行**: %d 轮模型回合\n", term.Turns)
	}
	if !tr.startedAt.IsZero() {
		fmt.Fprintf(&b, "- **本次运行耗时**: %s\n", roundDur(time.Since(tr.startedAt)))
	}
	if u := term.Usage; u.InputTokens+u.OutputTokens+u.CacheReadTokens+u.CacheWriteTokens > 0 {
		fmt.Fprintf(&b, "- **累计 token**: 输入 %d / 输出 %d / 缓存读 %d / 缓存写 %d\n",
			u.InputTokens, u.OutputTokens, u.CacheReadTokens, u.CacheWriteTokens)
	}
	if tr.name == "" {
		b.WriteString("- **工具调用**: 本次运行还没有发出工具调用就结束了\n")
	} else if tr.pending {
		fmt.Fprintf(&b, "- **中断时正在执行的工具**: `%s`（已运行 %s，**未返回结果**）\n\n  ```json\n  %s\n  ```\n",
			tr.name, roundDur(time.Since(tr.at)), firstLine(tr.input, 300))
	} else {
		fmt.Fprintf(&b, "- **中断前最后一个工具**: `%s`（已正常返回）\n", tr.name)
	}
	return sum, b.String()
}

// A single model call can reach its own wall clock while the task context is
// still live. Do not present that provider failure as the task's run budget.
func normalizeCallTimeoutTerminal(ctx context.Context, term *harness.Terminal) *harness.Terminal {
	if term == nil || ctx.Err() != nil {
		return term
	}
	var callTimeout *CallTimeoutError
	if !errors.As(term.Err, &callTimeout) {
		return term
	}
	copy := *term
	copy.Reason = harness.ReasonModelError
	return &copy
}

func terminalReasonLabel(reason harness.TerminalReason) string {
	if reason == "" {
		return "context_canceled"
	}
	return string(reason)
}

func terminalReasonHint(reason harness.TerminalReason) string {
	if hint := reasonHint[reason]; hint != "" {
		return hint
	}
	if reason == "" {
		return "运行的 context 已取消，但底层没有产生 Terminal 事件"
	}
	return "未知终态；harness 可能新增了 TerminalReason，请补充 reasonHint"
}

func progressSuffix(term *harness.Terminal, tr *runTrace) string {
	var parts []string
	if term.Turns > 0 {
		parts = append(parts, fmt.Sprintf("%d 轮", term.Turns))
	}
	if !tr.startedAt.IsZero() {
		parts = append(parts, roundDur(time.Since(tr.startedAt)))
	}
	if len(parts) == 0 {
		return ""
	}
	return "，已运行 " + strings.Join(parts, " / ")
}

func roundDur(d time.Duration) string {
	switch {
	case d < time.Minute:
		return d.Round(100 * time.Millisecond).String()
	case d < time.Hour:
		return d.Round(time.Second).String()
	default:
		return d.Round(time.Minute).String()
	}
}
