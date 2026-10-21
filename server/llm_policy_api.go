package server

import (
	"net/http"

	"github.com/Autumn-27/artex/db"
)

// pgGetLLMRetryPolicy 返回全局重试策略(五层各自的次数+间隔)。未配置过 → 全零，
// 前端把零显示成「默认」。
func (s *Server) pgGetLLMRetryPolicy(w http.ResponseWriter, r *http.Request) {
	pg := s.pg(w)
	if pg == nil {
		return
	}
	writeJSON(w, 200, pg.LLMRetryPolicy())
}

// pgSaveLLMRetryPolicy 保存全局重试策略。三个「跟着端点走」的层(建连/空响应/同
// provider 安全窗口)是 provider 的构建参数或调用参数，改完必须让缓存里的 provider
// 重建；熔断参数则直接推给进程级 Registry。
func (s *Server) pgSaveLLMRetryPolicy(w http.ResponseWriter, r *http.Request) {
	pg := s.pg(w)
	if pg == nil {
		return
	}
	var pol db.LLMRetryPolicy
	if err := decode(r, &pol); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	if err := pg.SetLLMRetryPolicy(pol); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	s.applyRetryPolicy()
	s.invalidateProfileAgents()
	s.reapplyActiveProfile()
	writeJSON(w, 200, pg.LLMRetryPolicy())
}

// pgLLMPoolStatus reports the failover ("轮询") switches, the resolved chain order
// and every profile's circuit-breaker state — what the LLM page renders as the
// "轮询顺序" strip and the per-card health badges.
func (s *Server) pgLLMPoolStatus(w http.ResponseWriter, r *http.Request) {
	if s.pg(w) == nil {
		return
	}
	writeJSON(w, 200, s.llmPoolStatus())
}

// pgLLMPoolReset clears a tripped profile's circuit breaker so the next call
// tries it again immediately ("立即恢复"). id=0 clears every profile.
func (s *Server) pgLLMPoolReset(w http.ResponseWriter, r *http.Request) {
	pg := s.pg(w)
	if pg == nil {
		return
	}
	var body struct {
		ID int64 `json:"id"` // 0 / omitted = all
	}
	if err := decode(r, &body); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	if body.ID > 0 {
		s.llmHealth.Reset(body.ID)
	} else {
		for id := range s.llmHealth.Snapshot() {
			s.llmHealth.Reset(id)
		}
	}
	writeJSON(w, 200, s.llmPoolStatus())
}
