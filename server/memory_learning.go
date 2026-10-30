package server

import (
	"net/http"
	"time"

	"github.com/Autumn-27/artex/db"
)

type memoryLearningView struct {
	Status           string     `json:"status"`
	Message          string     `json:"message"`
	Queued           int64      `json:"queued"`
	Running          int64      `json:"running"`
	Failed           int64      `json:"failed"`
	Processed        int64      `json:"processed"`
	StartsLastHour   int64      `json:"starts_last_hour"`
	AttemptsLastHour int64      `json:"attempts_last_hour"`
	MaxCallsPerHour  int        `json:"max_calls_per_hour"`
	LastCompletedAt  *time.Time `json:"last_completed_at,omitempty"`
	LastResult       string     `json:"last_result,omitempty"`
}

// This is an observation, not a command to start learning. A processed stage
// can legitimately produce zero experience; model availability alone is not
// evidence that anything has been learned.
func memoryLearningStatus(settings db.MemorySettings, snapshot db.MemoryLearningSnapshot, modelAvailable bool) memoryLearningView {
	v := memoryLearningView{Status: "idle", Message: "暂无待总结阶段；只从已结束阶段的原生事实提炼通用经验。",
		Queued: snapshot.Queued, Running: snapshot.Running, Failed: snapshot.Failed, Processed: snapshot.Done,
		StartsLastHour: snapshot.StartsLastHour, AttemptsLastHour: snapshot.AttemptsLastHour, MaxCallsPerHour: settings.MaxJobsPerHour}
	if snapshot.LastJob != nil {
		v.LastResult = snapshot.LastJob.Result
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
	case v.LastResult == "no_experience":
		v.Message = "最近阶段已处理，没有值得保留的通用经验；这是正常结果。"
	case v.LastResult == "failed":
		v.Message = "最近阶段总结失败，未确认新增经验；可检查模型或主动回顾已结束阶段。"
	case v.LastResult == "saved":
		v.Message = "最近阶段已保存条件性经验；后续只按相关性召回，不当作新目标的证据。"
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
