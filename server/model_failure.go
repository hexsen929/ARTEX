package server

import (
	"context"
	"errors"
	"net"
	"strings"

	"github.com/Autumn-27/artex/agent"
	"github.com/Autumn-27/artex/db"
	"github.com/Autumn-27/norma/llm"
)

type modelFailureInfo struct {
	Kind      string
	Action    string
	Retryable bool
}

func modelFailure(err error) modelFailureInfo {
	switch {
	case err == nil:
		return modelFailureInfo{Kind: "none"}
	case errors.Is(err, context.Canceled):
		return modelFailureInfo{Kind: "cancelled", Action: "resume_after_control"}
	case isQuotaExhaustedError(err):
		return modelFailureInfo{Kind: "quota", Action: "switch_profile_or_replenish"}
	}
	var request *llm.RequestError
	var history *llm.HistoryError
	if errors.As(err, &request) {
		return modelFailureInfo{Kind: "configuration", Action: "repair_configuration"}
	}
	if errors.As(err, &history) {
		return modelFailureInfo{Kind: "protocol", Action: "repair_history"}
	}
	var httpError *llm.HTTPError
	if errors.As(err, &httpError) {
		body := strings.ToLower(httpError.Body)
		if strings.Contains(body, "content_policy_violation") || strings.Contains(body, "sensitive words") || strings.Contains(body, "敏感词") {
			return modelFailureInfo{Kind: "refusal", Action: "review_provider_policy"}
		}
		switch httpError.StatusCode {
		case 401, 404:
			return modelFailureInfo{Kind: "configuration", Action: "repair_credentials_or_endpoint"}
		case 403:
			return modelFailureInfo{Kind: "refusal", Action: "review_provider_policy"}
		case 400, 422:
			return modelFailureInfo{Kind: "protocol", Action: "repair_request"}
		case 429:
			return modelFailureInfo{Kind: "rate_limit", Action: "bounded_backoff", Retryable: true}
		default:
			if httpError.StatusCode >= 500 {
				return modelFailureInfo{Kind: "transport", Action: "bounded_backoff", Retryable: true}
			}
		}
	}
	var network net.Error
	if errors.As(err, &network) || errors.Is(err, context.DeadlineExceeded) {
		return modelFailureInfo{Kind: "transport", Action: "bounded_backoff", Retryable: true}
	}
	message := strings.ToLower(err.Error())
	if strings.Contains(message, "decode response") || strings.Contains(message, "incomplete tool input") || strings.Contains(message, "invalid signature") || strings.Contains(message, "thinking") && strings.Contains(message, "400") {
		return modelFailureInfo{Kind: "protocol", Action: "repair_request_or_history"}
	}
	return modelFailureInfo{Kind: "unknown", Action: "inspect_saved_error", Retryable: true}
}

func (e *Engine) pauseForReview(t *Task, reason string) {
	if err := e.m.SetTaskPaused(t.ID, true); err != nil {
		return
	}
	e.Pause(t.ID, agent.Causef("run_needs_review", "本次运行需要复核", "%s；已保存成果和检查点，目标仍保留未完成状态", reason))
	e.emitActivity(t, db.Activity{Worker: "system", Kind: "text", Summary: "本次运行已暂停，保留成果与缺口待复核", Detail: reason})
}
