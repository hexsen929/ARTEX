package server

import (
	"github.com/Autumn-27/artex/agent"
	"strconv"
	"time"
)

const (
	settingCompletionReviewSeconds = "completion_review_seconds"
	settingCompletionReviewTurns   = "completion_review_turns"
)

func (s *Server) completionReviewOptions() agent.ReviewOptions {
	opts := (agent.ReviewOptions{}).Normalized()
	if raw, ok, _ := s.m.pg.GetSetting(settingCompletionReviewSeconds); ok {
		if n, err := strconv.Atoi(raw); err == nil && n >= 15 && n <= 900 {
			opts.Duration = time.Duration(n) * time.Second
		}
	}
	if raw, ok, _ := s.m.pg.GetSetting(settingCompletionReviewTurns); ok {
		if n, err := strconv.Atoi(raw); err == nil && n >= 1 && n <= 64 {
			opts.MaxTurns = n
		}
	}
	return opts
}
