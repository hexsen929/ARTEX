package server

import (
	"net/http"
	"time"

	"github.com/Autumn-27/artex/db"
)

type memoryLearningView struct {
	Status           string           `json:"status"`
	Message          string           `json:"message"`
	Queued           int64            `json:"queued"`
	Running          int64            `json:"running"`
	Failed           int64            `json:"failed"`
	RetryableFailed  int64            `json:"retryable_failed"`
	StoppedFailed    int64            `json:"stopped_failed"`
	EntryCount       int64            `json:"entry_count"`
	OutcomeCounts    map[string]int64 `json:"outcome_counts"`
	NextBudgetAt     *time.Time       `json:"next_budget_at,omitempty"`
	Processed        int64            `json:"processed"`
	StartsLastHour   int64            `json:"starts_last_hour"`
	AttemptsLastHour int64            `json:"attempts_last_hour"`
	MaxCallsPerHour  int              `json:"max_calls_per_hour"`
	LastCompletedAt  *time.Time       `json:"last_completed_at,omitempty"`
	LastResult       string           `json:"last_result,omitempty"`
}

// This is an observation, not a command to start learning. A processed stage
// can legitimately produce zero experience; model availability alone is not
// evidence that anything has been learned.
func memoryLearningStatus(settings db.MemorySettings, snapshot db.MemoryLearningSnapshot, modelAvailable bool) memoryLearningView {
	if snapshot.BudgetLimit > 0 {
		settings.MaxJobsPerHour = snapshot.BudgetLimit
	}
	counts := make(map[string]int64, len(snapshot.OutcomeCounts))
	for result, count := range snapshot.OutcomeCounts {
		counts[db.NormalizeMemoryResult(result)] += count
	}
	v := memoryLearningView{Status: "idle", Message: "暂无待总结阶段；只从已结束阶段的原生事实提炼通用经验。",
		Queued: snapshot.Queued, Running: snapshot.Running, Failed: snapshot.Failed, Processed: snapshot.Done,
		StartsLastHour: snapshot.StartsLastHour, AttemptsLastHour: snapshot.AttemptsLastHour, MaxCallsPerHour: settings.MaxJobsPerHour,
		RetryableFailed: snapshot.RetryableFailed, StoppedFailed: snapshot.StoppedFailed,
		EntryCount: snapshot.EntryCount, OutcomeCounts: counts, NextBudgetAt: snapshot.NextBudgetAt}
	if snapshot.LastJob != nil {
		v.LastResult = db.NormalizeMemoryResult(snapshot.LastJob.Result)
		stamp := snapshot.LastJob.UpdatedAt
		v.LastCompletedAt = &stamp
	}
	switch {
	case !settings.Enabled || !settings.AutoExtract:
		v.Status, v.Message = "disabled", "后续自动总结已关闭；已在途请求可能结束，结果仍会重新核验。"
	case !modelAvailable:
		v.Status, v.Message = "model_unavailable", "总结模型暂不可用；未开始新调用，原任务测试不依赖此功能。"
	case snapshot.Running > 0:
		v.Status, v.Message = "running", "正在有界总结已结束阶段；总结不是目标测试，也不代表已新增经验。"
	case (snapshot.Queued+snapshot.RetryableFailed > 0) &&
		(snapshot.StartsLastHour >= int64(settings.MaxJobsPerHour) || snapshot.AttemptsLastHour >= int64(settings.MaxJobsPerHour)):
		v.Status, v.Message = "budget_wait", "等待小时预算恢复；领取阶段与实际 HTTP 尝试分别受限，重试也计入调用预算。"
	case snapshot.Queued > 0:
		v.Status, v.Message = "queued", "已结束阶段等待后台总结；不会逐条事实调用模型。"
	case snapshot.RetryableFailed > 0:
		v.Status, v.Message = "retry_wait", "上次总结未完成，正在等待退避重试；尚未确认产生新经验。"
	case snapshot.WaitingStage:
		v.Status, v.Message = "waiting_stage", "等待任务或执行阶段结束并稳定约 30 秒；运行中不逐条提炼事实。"
	case v.LastResult == "empty":
		v.Message = "最近阶段模型返回了有效空结果，未新增通用经验；这不代表其他目标无需测试。"
	case v.LastResult == "no_experience" || v.LastResult == "legacy_unknown":
		v.Message = "最近记录属于历史未细分的未新增结果；旧记录未区分格式错误、筛选拒绝与真实空结果，未自动改写或重新学习。"
	case v.LastResult == "failed":
		v.Message = "最近阶段总结失败，未确认新增经验；可检查模型或主动回顾已结束阶段。"
	case v.LastResult == "saved":
		v.Message = "最近阶段曾保存条件性经验；当前是否仍有效以条目列表为准，不当作新目标的证据。"
	case v.LastResult == "duplicate":
		v.Message = "最近阶段候选与同来源已有条目重复，未新增；没有把重复记录当作新的学习成果。"
	case v.LastResult == "output_invalid":
		v.Message = "最近阶段模型输出格式不合格，未保存经验；不是已确认没有经验，按原有限重试与预算处理。"
	case v.LastResult == "output_truncated":
		v.Message = "最近阶段模型输出被截断，未保存不完整经验；剩余重试会逐步增加输出空间，仍受三次尝试和小时调用额度限制。"
	case v.LastResult == "input_sensitive" || v.LastResult == "output_sensitive":
		v.Message = "最近阶段因敏感内容检查未采纳；未在学习诊断中保存正文或密钥。"
	case v.LastResult == "input_invalid":
		v.Message = "最近阶段来源投影不合格，未进行有效提炼；未把来源错误标为正常空结果。"
	case v.LastResult == "output_rejected":
		v.Message = "最近阶段候选未通过证据、完整适用条件或共享规则检查；未把筛选拒绝记成正常空结果。"
	case v.LastResult == "provider_error" || v.LastResult == "timeout":
		v.Message = "最近阶段模型请求失败或超时，未确认新增经验；仅保存安全分类，不回显提供方正文。"
	case v.LastResult == "storage_error":
		v.Message = "最近阶段存储读取或保存未完成，进入有界退避；不把存储故障当作无经验。"
	case v.LastResult == "lease_expired":
		v.Message = "最近阶段最后一次运行租约过期，已停止重试；未发送第四次模型请求，也未复活旧运行。"
	case v.LastResult == "budget_deferred":
		v.Message = "最近阶段因 HTTP 额度等待；被拦截的请求未发送。本次运行若已有请求发出仍计入尝试，否则保留重试次数。"
	case v.LastResult == "source_changed" || v.LastResult == "settings_changed" || v.LastResult == "cleared":
		v.Message = "最近阶段因来源、设置变化或清空操作未继续保存；不沿用旧快照，也不自动重放历史阶段。"
	case v.LastResult == "skipped":
		v.Message = "最近阶段未采纳，可能因为来源、条件或设置已变化。"
	}
	return v
}

func (s *Server) getMemoryLearning(w http.ResponseWriter, r *http.Request) {
	pg := s.pg(w)
	if pg == nil {
		return
	}
	settings, err := pg.GetMemorySettings(r.Context())
	if err != nil {
		memoryAPIError(w, err)
		return
	}
	snapshot, err := pg.MemoryLearningSnapshot(r.Context())
	if err != nil {
		memoryAPIError(w, err)
		return
	}
	_, _, modelErr := s.memoryModel(settings)
	writeJSON(w, http.StatusOK, memoryLearningStatus(settings, snapshot, modelErr == nil))
}
