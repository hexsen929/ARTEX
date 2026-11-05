package agent

import (
	"encoding/json"
	"errors"

	"github.com/Autumn-27/artex/db"
	"github.com/Autumn-27/norma/agentcore"
)

func workerContinuationContext(intent *db.Node) (string, bool) {
	if intent == nil {
		return "", false
	}
	var p struct {
		Checkpoint      *db.ExecutionCheckpoint `json:"execution_checkpoint"`
		Note            string                  `json:"continuation_note"`
		HistoryRequired bool                    `json:"execution_history_required"`
	}
	if json.Unmarshal(intent.Payload, &p) != nil {
		return "", false
	}
	if p.Checkpoint == nil && p.Note == "" && !p.HistoryRequired {
		return "", false
	}
	data, _ := json.Marshal(p)
	return "\n\n【持久续接点】\n" + string(data) + "\n这是工具调用与返回记录，不是目标检查成功证明。复用原会话与产物；pending或incomplete表示结果不明，先核对最后活动/原始记录，不盲目重放。continuation_note是本轮下一步，不替换原验收与授权。", p.HistoryRequired || (p.Checkpoint != nil && p.Checkpoint.ToolCalls > 0)
}

var ErrWorkerHistoryUnavailable = errors.New("execution_history_unavailable: 历史执行记录读取失败，本轮未继续执行；请修复记录后重试")

func resumeWorkerSession(s *agentcore.Session, id string) error {
	if err := s.Resume(id); err != nil {
		return ErrWorkerHistoryUnavailable
	}
	return nil
}
