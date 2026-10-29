package server

import (
	"context"
	"errors"
	"github.com/Autumn-27/artex/db"
	"github.com/Autumn-27/artex/llmguard"
	"github.com/Autumn-27/norma/harness"
)

// workerOutcome is intentionally free of raw provider bodies, URLs or tool input.
// Detailed traces remain in their existing access-controlled activity stream.
func workerOutcome(reason harness.TerminalReason, err error) (state, code, message string) {
	if reason == harness.ReasonTimeout && (err == nil || errors.Is(err, context.DeadlineExceeded)) {
		return "exhausted", "run_budget", "运行时间预算耗尽；已保存的结果保留，未完成部分待继续"
	}
	if reason == harness.ReasonMaxTurns && err == nil {
		return "exhausted", "turn_budget", "模型轮数预算耗尽；不代表任务已完成"
	}
	if err == nil && reason == harness.ReasonCompleted {
		return "done", "completed", "本轮执行结束；以事实和验收证据判断目标是否达成"
	}
	if isTaskLLMChainExhausted(err) {
		return "blocked", db.IntentBlockedLLMQuota, "所选模型配置链额度耗尽；补充额度或调整配置后继续"
	}
	switch reason {
	case harness.ReasonBlockingLimit, harness.ReasonPromptTooLong:
		return "blocked", "context_limit", "上下文超限；请缩小任务粒度或工具输出后继续"
	case harness.ReasonImageError:
		return "blocked", "unsupported_content", "当前模型不支持该内容；调整模型或工具返回后继续"
	case harness.ReasonHookStopped, harness.ReasonStopHookPrevented:
		return "blocked", "guard_stopped", "工具或规则停止了执行；请查看最后一条工具记录"
	}
	if err != nil {
		code = string(llmguard.Classify(err))
		messages := map[string]string{
			"model_unavailable":      "模型不存在或中转无可用渠道；检查模型映射与渠道状态后继续",
			"authentication":         "模型鉴权或权限失败；检查配置后继续",
			"quota_exhausted":        "模型额度不足；补充额度或调整配置后继续",
			"rate_limited":           "模型服务限流，本轮调用中断；已有执行记录保留，请稍后继续",
			"provider_failure":       "模型服务或中转返回错误，本轮调用中断；请核对错误详情后继续",
			"transport_failure":      "模型连接或传输失败；已有执行记录保留，请核对后继续",
			"unknown_failure":        "模型调用异常，具体原因尚未确定；已有执行记录保留，请核对错误详情后继续",
			"context_limit":          "上下文超限；缩小任务粒度或工具输出后继续",
			"retry_budget_exhausted": "本次模型请求的总重试预算已耗尽；未自动重放整条任务",
			"cooling_down":           "可用模型正在冷却或窗口不足；冷却结束或调整配置后继续",
			"invalid_request":        "模型请求参数不被接受；调整配置后继续",
			"canceled":               "执行已取消；已有结果保留",
		}
		message = messages[code]
		if message == "" {
			message = "执行未完成；请查看执行记录后继续"
		}
		return "blocked", code, message
	}
	return "blocked", string(reason), "本轮未正常完成；请查看执行记录后继续"
}
