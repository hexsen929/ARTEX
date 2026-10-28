package server

import (
	"context"
	"time"

	"github.com/Autumn-27/artex/db"
)

type toolUsageView struct {
	*db.Tool
	Calls          *int   `json:"calls"`
	UsageKind      string `json:"usage_kind"`
	UsageAvailable bool   `json:"usage_available"`
	UsageError     string `json:"usage_error,omitempty"`
}

func projectToolUsage(rows []*db.Tool, counts map[string]int, usageErr error) []toolUsageView {
	out := make([]toolUsageView, 0, len(rows))
	for _, row := range rows {
		item := toolUsageView{Tool: row, UsageKind: "callable", UsageAvailable: usageErr == nil}
		switch {
		case row.Kind == "shell":
			item.UsageKind, item.UsageAvailable = "shell_declaration", false
		case usageErr != nil:
			item.UsageError = "目录调用统计暂不可用"
		default:
			calls := counts[row.Key]
			item.Calls = &calls
		}
		out = append(out, item)
	}
	return out
}

type agentAutomationStatus struct {
	Kind       string `json:"kind"`
	Available  bool   `json:"available"`
	Enabled    bool   `json:"enabled"`
	Configured bool   `json:"configured"`
	State      string `json:"state"`
	Reason     string `json:"reason"`
}

// Report the switches without starting work, creating a trigger or replaying
// any historical finding. Unknown is intentionally distinct from disabled.
func decorateAgentAutomation(pg *db.DB, dto AgentDTO) AgentDTO {
	if dto.Key != db.FindingVerifierAgentKey && dto.Key != db.CredentialAgentKey {
		return dto
	}
	status := &agentAutomationStatus{State: "unavailable", Reason: "流程开关读取失败，当前状态未知"}
	dto.Automation = status
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if dto.Key == db.FindingVerifierAgentKey {
		status.Kind = "finding_verification"
		var configured, active bool
		if err := pg.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM agent_triggers WHERE agent_key=$1 AND on_finding),EXISTS(SELECT 1 FROM agent_triggers WHERE agent_key=$1 AND on_finding AND enabled)`, dto.Key).Scan(&configured, &active); err != nil {
			return dto
		}
		mode, _, err := pg.GetSetting(settingFindingVerifyMode)
		if err != nil {
			return dto
		}
		status.Available, status.Configured = true, configured
		switch {
		case !dto.Enabled:
			status.State, status.Reason = "agent_disabled", "验证 Agent 已停用；不会自动派发"
		case !configured:
			status.State, status.Reason = "not_configured", "未配置新发现触发器，不会自动验证；已有设置不会被升级自动重建"
		case !findingVerifyAutoEnabled(mode):
			status.State, status.Reason = "mode_off", "自动验证模式已关闭；显式手动验证另行发起"
		case !active:
			status.State, status.Reason = "trigger_disabled", "新发现触发器已停用；合并增量也不会自动重验"
		default:
			status.Enabled = true
			status.State, status.Reason = "ready", "自动验证已启用，仅处理任务运行中的新事件；暂停时不执行，也不自动补验历史发现"
		}
		return dto
	}
	status.Kind = "credential_processing"
	var intelligence, processor bool
	if err := pg.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM settings WHERE key=$1 AND value IN ('true','1')),EXISTS(SELECT 1 FROM settings WHERE key=$2 AND value IN ('true','1'))`, db.SettingTaskIntelligence, db.SettingCredentialAgent).Scan(&intelligence, &processor); err != nil {
		return dto
	}
	status.Available, status.Configured = true, true
	if dto.Enabled && intelligence && processor {
		status.Enabled, status.State, status.Reason = true, "ready", "仅整理已上报情报；单次结构化处理，不主动登录或验证凭据"
	} else {
		status.State, status.Reason = "pipeline_off", "情报、凭据处理或 Agent 开关未全部开启；Agent 启用不等于处理流程已启用"
	}
	return dto
}
