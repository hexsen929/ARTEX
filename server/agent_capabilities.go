package server

import (
	"errors"
	"slices"

	"github.com/Autumn-27/artex/db"
)

// These are assembly contracts, not additional grants. A saved binding does
// not turn a prompt variant or a no-tools processor into an execution role.
type agentBindingCapabilities struct {
	ExecutionAgentKey     string   `json:"execution_agent_key"`
	ToolBindingMode       string   `json:"tool_binding_mode"`
	MCPBindingMode        string   `json:"mcp_binding_mode"`
	SkillBindingMode      string   `json:"skill_binding_mode"`
	AllowedToolKeys       []string `json:"allowed_tool_keys,omitempty"`
	Note                  string   `json:"note,omitempty"`
	RuntimeConfigMode     string   `json:"runtime_config_mode"`
	RuntimeConfigAgentKey string   `json:"runtime_config_agent_key"`
}

func bindingCapabilities(key string) agentBindingCapabilities {
	c := agentBindingCapabilities{ExecutionAgentKey: key, ToolBindingMode: "direct", MCPBindingMode: "direct", SkillBindingMode: "direct", RuntimeConfigMode: "direct", RuntimeConfigAgentKey: key}
	switch key {
	case "worker.intranet":
		c.ExecutionAgentKey, c.ToolBindingMode, c.MCPBindingMode = "worker", "inherited", "inherited"
		c.RuntimeConfigMode, c.RuntimeConfigAgentKey = "inherited", "worker"
		c.Note = "执行工具与 MCP 沿用 worker；Skill 按本变体独立授权。绑定数量不是运行时能力或健康检查。"
	case db.CredentialAgentKey:
		c.ToolBindingMode, c.MCPBindingMode, c.SkillBindingMode = "unsupported", "unsupported", "unsupported"
		c.RuntimeConfigMode = "llm_only"
		c.Note = "仅对提供内容作单次结构化整理，不装配工具、MCP 或 Skill。"
	case "goals":
		c.ToolBindingMode, c.MCPBindingMode, c.SkillBindingMode = "limited", "unsupported", "readonly"
		c.RuntimeConfigMode = "llm_only"
		c.AllowedToolKeys = []string{"set_goals", "set_constraints", "add_task_scope"}
		c.Note = "只使用目标与约束工具，并可读取已授权 Skill 指导；不开放 Bash、MCP 或依赖执行能力。"
	default:
		c.Note = "显示已启用的直接绑定；默认 SDK 工具、运行时开关及延迟开放状态另行决定实际能力。"
	}
	return c
}

func toolBindingSupported(agentKey, toolKey string) bool {
	c := bindingCapabilities(agentKey)
	return c.ToolBindingMode == "direct" || c.ToolBindingMode == "limited" && slices.Contains(c.AllowedToolKeys, toolKey)
}

var errUnsupportedBinding = errors.New("该角色不支持新增此类直接绑定，请按角色能力或继承关系配置")

// Preserve existing legacy selections; reject only unsupported NEW grants.
// Removing an old grant remains an explicit administrator action.
func validateNewToolBindings(pg *db.DB, toolKey string, previous, next []string) error {
	for _, key := range next {
		if slices.Contains(previous, key) {
			continue
		}
		a, err := pg.GetAgentByKey(key)
		if err != nil {
			return errors.New("读取 Agent 绑定信息失败")
		}
		if a == nil || !toolBindingSupported(key, toolKey) {
			return errUnsupportedBinding
		}
	}
	return nil
}

func hasAddedResource[T comparable](previous, next []T) bool {
	for _, value := range next {
		if !slices.Contains(previous, value) {
			return true
		}
	}
	return false
}

func validateResourceBindingPatch(pg *db.DB, a *db.Agent, nextMCP *[]int64, nextSkills *[]string) error {
	c := bindingCapabilities(a.Key)
	if nextMCP != nil {
		previous, err := pg.AgentVisible(a.ID, "mcp")
		if err != nil {
			return errors.New("读取已有 MCP 授权失败")
		}
		if c.MCPBindingMode != "direct" && hasAddedResource(previous, *nextMCP) {
			return errUnsupportedBinding
		}
		for _, id := range *nextMCP {
			if slices.Contains(previous, id) {
				continue
			}
			var exists bool
			if id <= 0 {
				return errors.New("MCP 资源编号无效")
			}
			if err := pg.QueryRow(`SELECT EXISTS(SELECT 1 FROM mcp_servers WHERE id=$1)`, id).Scan(&exists); err != nil {
				return errors.New("MCP 目录读取失败")
			}
			if !exists {
				return errors.New("MCP 资源不存在")
			}
		}
	}
	if nextSkills != nil {
		previous, err := pg.AgentSkillNames(a.ID)
		if err != nil {
			return errors.New("读取已有 Skill 授权失败")
		}
		if c.SkillBindingMode == "unsupported" && hasAddedResource(previous, *nextSkills) {
			return errUnsupportedBinding
		}
		for _, name := range *nextSkills {
			if !slices.Contains(previous, name) && !validSkillName(name) {
				return errors.New("Skill 目录名称无效")
			}
		}
	}
	return nil
}

func conversationAgentBlockReason(pg *db.DB, key string) string {
	if key == "worker.intranet" {
		return "内网提示词变体通过任务中的 worker 使用，不独立运行"
	}
	if pg == nil {
		return "Agent 元数据暂不可读取，本轮未执行"
	}
	a, err := pg.GetAgentByKey(key)
	if err != nil {
		return "Agent 元数据读取失败，本轮未执行"
	}
	if a == nil {
		return "Agent 已删除，历史对话仍可查看"
	}
	if !a.Enabled {
		return "Agent 已停用，历史对话仍可查看"
	}
	return ""
}
