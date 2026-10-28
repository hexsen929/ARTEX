import type { Agent, AgentRuntimeCapabilities, Tool } from "./types";

const GOAL_TOOL_KEYS = ["set_goals", "set_constraints", "add_task_scope"];

/** Older services have no capability projection; these fixed roles still have
 * known runtime semantics. Unknown conversational roles retain direct binding. */
export function agentRuntimeCapabilities(agent: Pick<Agent, "key" | "capabilities">): AgentRuntimeCapabilities {
  const defaults = defaultRuntimeCapabilities(agent.key);
  return { ...defaults, ...agent.capabilities };
}

function defaultRuntimeCapabilities(key: string): AgentRuntimeCapabilities {
  if (key === "worker.intranet") {
    return { execution_agent_key: "worker", tool_binding_mode: "inherited", mcp_binding_mode: "inherited", skill_binding_mode: "direct", runtime_config_mode: "inherited", runtime_config_agent_key: "worker", note: "执行工具、MCP 与运行配置沿用 worker；提示词正文与 Skill 单独设置。" };
  }
  if (key === "credential_processor") {
    return { execution_agent_key: key, tool_binding_mode: "unsupported", mcp_binding_mode: "unsupported", skill_binding_mode: "unsupported", runtime_config_mode: "llm_only", runtime_config_agent_key: key, note: "凭据整理仅做无工具的纯 JSON 结构化模型调用，仅支持模型绑定，不执行命令或加载执行依赖。" };
  }
  if (key === "goals") {
    return { execution_agent_key: key, tool_binding_mode: "limited", mcp_binding_mode: "unsupported", skill_binding_mode: "readonly", runtime_config_mode: "llm_only", runtime_config_agent_key: key, allowed_tool_keys: GOAL_TOOL_KEYS, note: "目标拆解使用固定受控预算、有限基础工具与只读 Skill 指导；可绑定模型，不开放 Bash、交互 Shell 或 MCP。" };
  }
  return { execution_agent_key: key, tool_binding_mode: "direct", mcp_binding_mode: "direct", skill_binding_mode: "direct", runtime_config_mode: "direct", runtime_config_agent_key: key };
}

export function agentBindingSupport(agent: Pick<Agent, "key" | "capabilities">, kind: "tool" | "mcp" | "skill", toolKey?: string): { allowed: boolean; reason: string } {
  const capabilities = agentRuntimeCapabilities(agent);
  const mode = kind === "tool" ? capabilities.tool_binding_mode : kind === "mcp" ? capabilities.mcp_binding_mode : capabilities.skill_binding_mode;
  if (mode === "inherited") return { allowed: false, reason: `沿用 ${capabilities.execution_agent_key} 的${kind === "mcp" ? " MCP" : "工具"}绑定，请到执行角色设置。` };
  if (mode === "unsupported") return { allowed: false, reason: capabilities.note || "该角色不使用此类资源，已有配置仅保留展示。" };
  if (kind === "tool" && mode === "limited" && !capabilities.allowed_tool_keys?.includes(toolKey || "")) {
    return { allowed: false, reason: "目标拆解只接入 set_goals、set_constraints、add_task_scope，不开放额外执行工具。" };
  }
  return { allowed: true, reason: mode === "readonly" ? "仅加载已授权 Skill 的知识指导，不解锁 MCP 或执行工具。" : "" };
}

export function toolUsageDisplay(tool: Pick<Tool, "kind" | "usage_kind" | "usage_available" | "calls">): { label: string; detail: string; applicable: boolean } {
  if (tool.usage_kind === "shell_declaration" || tool.kind === "shell") {
    return { label: "环境声明", detail: "经 Bash 使用，不产生独立目录调用；次数不适用。请到命令记录核对实际执行。", applicable: false };
  }
  if (tool.usage_available === false || typeof tool.calls !== "number") {
    return { label: "统计不可用", detail: "当前未取得调用统计，不代表零次调用。", applicable: true };
  }
  return { label: `目录调用 ${tool.calls}`, detail: "目录工具调用尝试次数，不代表执行成功；不包含 Bash 内部调用的外部程序。", applicable: true };
}

export function bindingCountLabel(count: number | undefined, available?: boolean): string {
  return available === false || typeof count !== "number" ? "—" : String(count);
}
