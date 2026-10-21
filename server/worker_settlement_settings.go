package server

import (
	"strconv"
	"time"

	"github.com/Autumn-27/artex/agent"
)

const settingWorkerSettlementSeconds = "worker_settlement_seconds"

func (s *Server) workerSettlementTimeout() time.Duration {
	if s != nil && s.m != nil && s.m.pg != nil {
		if raw, ok, err := s.m.pg.GetSetting(settingWorkerSettlementSeconds); err == nil && ok {
			if n, err := strconv.Atoi(raw); err == nil && n >= int(agent.MinWorkerSettlementTimeout/time.Second) && n <= int(agent.MaxWorkerSettlementTimeout/time.Second) {
				d := time.Duration(n) * time.Second
				if d >= agent.MinWorkerSettlementTimeout && d <= agent.MaxWorkerSettlementTimeout {
					return d
				}
			}
		}
	}
	return agent.DefaultWorkerSettlementTimeout
}
