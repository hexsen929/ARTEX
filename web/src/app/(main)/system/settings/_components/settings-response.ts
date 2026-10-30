import type { Settings } from "@/lib/types";

export const MAX_LLM_RECORDS_RETENTION_DAYS = 106751;

export function isLLMRecordsRetentionDays(value: unknown): value is number {
  return typeof value === "number" && Number.isInteger(value) && value >= 0 && value <= MAX_LLM_RECORDS_RETENTION_DAYS;
}

export function parseLLMRecordsRetentionDraft(value: string): number | null {
  if (!/^\d+$/.test(value)) return null;
  const days = Number(value);
  return isLLMRecordsRetentionDays(days) ? days : null;
}

export function readLLMRecordsRetentionDays(value: unknown): number {
  if (!value || typeof value !== "object" || Array.isArray(value)) throw new Error("服务未返回有效的 LLM 记录保留期，当前值未确认");
  const days = (value as Record<string, unknown>).llm_records_retention_days;
  if (!isLLMRecordsRetentionDays(days)) throw new Error("服务未返回有效的 LLM 记录保留期，当前值未确认");
  return days;
}

export function isFindingVerifyMode(value: unknown): value is Settings["finding_verify_mode"] {
  return value === "auto" || value === "off";
}

export function readFindingVerifyMode(value: unknown): Settings["finding_verify_mode"] {
  if (!value || typeof value !== "object" || Array.isArray(value)) throw new Error("服务未返回有效的自动验证模式，当前值未确认");
  const mode = (value as Record<string, unknown>).finding_verify_mode;
  if (!isFindingVerifyMode(mode)) throw new Error("服务未返回有效的自动验证模式，当前值未确认");
  return mode;
}

export function isRoEEnforcement(value: unknown): value is Settings["roe_enforcement"] {
  return value === "off" || value === "warn" || value === "strict";
}

// Never turn an omitted or malformed response into a permissive local default.
// The independent RoE card also uses this reader when explicitly repairing a
// stored invalid value; it writes only the field the user selected.
export function readRoEEnforcement(value: unknown): Settings["roe_enforcement"] {
  if (!value || typeof value !== "object" || Array.isArray(value)) {
    throw new Error("服务未返回有效的 RoE 授权范围模式，当前值未确认");
  }
  const mode = (value as Record<string, unknown>).roe_enforcement;
  if (!isRoEEnforcement(mode)) throw new Error("服务未返回有效的 RoE 授权范围模式，当前值未确认");
  return mode;
}

const requiredBooleanSettings = [
  "credential_agent_enabled",
  "credential_traffic_binding",
  "task_intelligence_enabled",
  "traffic_capture",
  "agent_traffic_binding",
  "web_search_enabled",
  "brave_key_set",
  "tavily_key_set",
  "task_auto_title_enabled",
  "chat_auto_title_enabled",
  "browser_screenshot_enabled",
  "browser_screenshot_timestamp_enabled",
  "opsec_enabled",
  "finding_inline_merge_enabled",
  "show_egress_ip",
  "constraints_inject_planner",
  "constraints_inject_worker",
  "noa_compaction",
  "llm_record",
  "task_concurrency_enabled",
  "llm_pool_enabled",
  "llm_pool_bind_fallback",
  "notify_enabled",
  "callback_addr_locked",
] as const;

const requiredStringSettings = [
  "web_search_backend",
  "web_search_proxy",
  "global_proxy",
  "python_interpreter",
  "callback_addr",
  "notify_public_base_url",
] as const;

// Settings GET/PUT return a complete snapshot. Treat an unknown/older response
// as read-only instead of coercing missing fields to false or an empty pool.
export function assertSettingsSnapshot(value: unknown, maxWorkers: number): asserts value is Settings {
  if (!value || typeof value !== "object" || Array.isArray(value)) {
    throw new Error("服务未返回完整设置，请确认后端版本并刷新页面");
  }
  const settings = value as Record<string, unknown>;
  if (requiredBooleanSettings.some((key) => typeof settings[key] !== "boolean")) {
    throw new Error("服务未返回完整设置，已有配置保持不变");
  }
  if (requiredStringSettings.some((key) => typeof settings[key] !== "string")) {
    throw new Error("服务未返回完整代理和地址设置，已有配置保持不变");
  }
  readRoEEnforcement(settings);
  readLLMRecordsRetentionDays(settings);
  readFindingVerifyMode(settings);
  for (const key of ["task_concurrency_limit", "notify_digest_interval_min"] as const) {
    const count = settings[key];
    if (typeof count !== "number" || !Number.isInteger(count) || count < 1) {
      throw new Error("服务返回的并发或通知周期设置无效，已有配置保持不变");
    }
  }
  if (
    typeof settings.callback_addr_source !== "string" ||
    !["env", "settings", "config", "unset"].includes(settings.callback_addr_source) ||
    (settings.callback_addr_source === "env" && settings.callback_addr_locked !== true) ||
    (Object.hasOwn(settings, "callback_addr_error") && typeof settings.callback_addr_error !== "string")
  ) {
    throw new Error("服务未返回完整回连来源和锁定状态，已有配置保持不变");
  }
  const workers = settings.workers;
  if (typeof workers !== "number" || !Number.isInteger(workers) || workers < 1 || workers > maxWorkers) {
    throw new Error("服务返回的 Worker 并发数无效，已有配置保持不变");
  }
  const pools = settings.worker_pools;
  if (!Array.isArray(pools)) {
    throw new Error("服务未返回完整 Worker 池，已有模型池保持不变");
  }
  const profileIDs = new Set<string>();
  let total = 0;
  for (const pool of pools) {
    if (!pool || typeof pool !== "object" || Array.isArray(pool)) {
      throw new Error("服务返回的 Worker 池无效，已有模型池保持不变");
    }
    const { profile_id: id, workers: count } = pool as Record<string, unknown>;
    if (
      typeof id !== "string" || id.trim() === "" || profileIDs.has(id) ||
      typeof count !== "number" || !Number.isInteger(count) || count < 1 || count > maxWorkers
    ) {
      throw new Error("服务返回的 Worker 池无效，已有模型池保持不变");
    }
    profileIDs.add(id);
    total += count;
  }
  if (total > maxWorkers) {
    throw new Error("服务返回的 Worker 池总数超限，已有模型池保持不变");
  }
}

export function egressSettingsSnapshot(value: unknown): { enabled: boolean; globalProxy: string } {
  if (!value || typeof value !== "object" || Array.isArray(value)) {
    throw new Error("服务未返回出口显示设置，保持关闭");
  }
  const settings = value as Record<string, unknown>;
  if (typeof settings.show_egress_ip !== "boolean" || typeof settings.global_proxy !== "string") {
    throw new Error("服务未返回完整出口显示设置，不发起探测");
  }
  return { enabled: settings.show_egress_ip, globalProxy: settings.global_proxy };
}
