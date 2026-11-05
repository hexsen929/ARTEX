package agent

import (
	"errors"

	"github.com/Autumn-27/norma/agentcore"
)

var ErrWorkerHistoryUnavailable = errors.New("execution_history_unavailable: 历史执行记录读取失败，本轮未继续执行；请修复记录后重试")

func resumeWorkerSession(s *agentcore.Session, id string) error {
	if err := s.Resume(id); err != nil {
		return ErrWorkerHistoryUnavailable
	}
	return nil
}
