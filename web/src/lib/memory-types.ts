export const MEMORY_KIND_LABELS = {
  environment: "工具与环境经验",
  procedure: "方法经验",
  lesson: "排错经验",
  target: "目标记录（仅原任务）",
} as const;

export const MEMORY_STATUS_LABELS = {
  active: "有效",
  disabled: "已停用",
  expired: "已过期",
  source_invalid: "来源失效",
} as const;

export type MemoryKind = keyof typeof MEMORY_KIND_LABELS;
export type MemoryStatus = keyof typeof MEMORY_STATUS_LABELS;
export type MemoryShareScope = "global_experience" | "source_only";

export const MEMORY_LEARNING_LABELS = {
  disabled: "自动学习已关闭",
  model_unavailable: "总结模型不可用",
  waiting_stage: "等待阶段结束",
  queued: "阶段总结已排队",
  running: "正在总结阶段经验",
  budget_wait: "等待调用预算",
  retry_wait: "等待有限重试",
  idle: "当前无待总结阶段",
} as const;

export const MEMORY_LEARNING_RESULT_LABELS = {
  saved: "曾保存经验",
  empty: "模型明确返回空结果",
  duplicate: "与已有条目重复",
  input_invalid: "来源格式不合格",
  input_sensitive: "来源敏感信息拒绝",
  output_invalid: "输出格式错误",
  output_truncated: "输出被截断",
  output_rejected: "候选筛选未采纳",
  output_sensitive: "候选敏感信息拒绝",
  source_changed: "来源已变化",
  settings_changed: "设置已变化",
  cleared: "受清理操作影响",
  budget_deferred: "等待发送预算",
  provider_error: "模型调用错误",
  timeout: "总结超时",
  storage_error: "存储读取或保存错误",
  lease_expired: "处理租约过期",
  no_experience: "历史未细分未新增",
  failed: "历史未细分失败",
  skipped: "历史未细分跳过",
  legacy_unknown: "历史结果未分类",
} as const;

export type MemoryLearningResult = keyof typeof MEMORY_LEARNING_RESULT_LABELS;

export interface MemoryLearningSnapshot {
  status: keyof typeof MEMORY_LEARNING_LABELS;
  message: string;
  queued: number;
  running: number;
  failed: number;
  processed: number;
  starts_last_hour: number;
  attempts_last_hour: number;
  max_calls_per_hour: number;
  // Absent on older APIs. Missing telemetry is unknown, never an observed zero.
  retryable_failed?: number;
  stopped_failed?: number;
  entry_count?: number;
  outcome_counts?: Partial<Record<MemoryLearningResult, number>>;
  next_budget_at?: string;
  last_completed_at?: string;
  last_result?: MemoryLearningResult;
}

export interface MemoryEntry {
  id: number;
  title: string;
  content: string;
  kind: MemoryKind;
  status: MemoryStatus;
  origin_task_id: number;
  origin_conversation_id: number;
  agent_key: string;
  target: string;
  applicability: string;
  tags: string[];
  source_kind: string;
  source_id: string;
  source_version: number;
  version: number;
  created_at: string;
  updated_at: string;
  expires_at?: string | null;
  last_used_at?: string | null;
  use_count: number;
  automatic: boolean;
  confidence: "observed" | "unverified";
  session_id?: string;
  readonly share_scope: MemoryShareScope;
  readonly evidence_ids?: string[];
}

export interface MemorySettings {
  enabled: boolean;
  auto_extract: boolean;
  global_share: boolean;
  profile_id: number;
  max_entries: number;
  max_tokens: number;
  max_jobs_per_hour: number;
  retention_days: number;
  epoch: number;
  learn_after: string;
  model_available: boolean;
  model_error?: string;
}

export type MemorySettingsInput = Omit<MemorySettings, "learn_after" | "model_available" | "model_error">;

export interface MemoryUsage {
  id: number;
  entry_id: number;
  task_id: number;
  conversation_id?: number;
  session_id: string;
  agent_key: string;
  entry_version: number;
  created_at: string;
  result: string;
}

export interface MemoryEntryInput {
  title: string;
  content: string;
  kind: MemoryKind;
  target: string;
  applicability: string;
  tags: string[];
  expires_at?: string | null;
  origin_task_id?: number;
}

export interface MemoryQuery {
  q?: string;
  status?: MemoryStatus | "";
  kind?: MemoryKind | "";
  task_id?: number;
  limit?: number;
  offset?: number;
}

export interface MemoryUsageQuery {
  task_id?: number;
  conversation_id?: number;
  session_id?: string;
  limit?: number;
}

export function memoryQueryString(query: MemoryQuery | MemoryUsageQuery): string {
  const params = new URLSearchParams();
  for (const [key, value] of Object.entries(query)) {
    if (value === undefined || value === "") continue;
    if (typeof value === "number" && (!Number.isSafeInteger(value) || value < 0 || (key !== "offset" && value === 0))) {
      throw new Error("记忆查询参数须为有效整数");
    }
    params.set(key, String(value));
  }
  const encoded = params.toString();
  return encoded ? `?${encoded}` : "";
}

export function assertMemorySettings(value: unknown): asserts value is MemorySettings {
  if (!value || typeof value !== "object") throw new Error("服务未返回记忆设置");
  const item = value as Record<string, unknown>;
  const flags = ["enabled", "auto_extract", "global_share", "model_available"];
  const numbers = ["profile_id", "max_entries", "max_tokens", "max_jobs_per_hour", "retention_days", "epoch"];
  if (
    flags.some((key) => typeof item[key] !== "boolean") ||
    numbers.some((key) => !Number.isSafeInteger(item[key]) || Number(item[key]) < 0) ||
    Number(item.max_entries) < 1 || Number(item.max_entries) > 10 ||
    Number(item.max_tokens) < 1024 || Number(item.max_tokens) > 2000 ||
    Number(item.max_jobs_per_hour) < 1 || Number(item.max_jobs_per_hour) > 60 ||
    Number(item.retention_days) < 1 || Number(item.retention_days) > 3650 ||
    Number(item.epoch) < 1 ||
    typeof item.learn_after !== "string" ||
    !Number.isFinite(Date.parse(item.learn_after))
  ) {
    throw new Error("记忆设置不完整或无效，请确认后端版本后重试");
  }
}

export function memorySettingsInput(settings: MemorySettings): MemorySettingsInput {
  // Explicit whitelist: new read-only status fields must never leak into PUTs.
  return {
    enabled: settings.enabled,
    auto_extract: settings.auto_extract,
    global_share: settings.global_share,
    profile_id: settings.profile_id,
    max_entries: settings.max_entries,
    max_tokens: settings.max_tokens,
    max_jobs_per_hour: settings.max_jobs_per_hour,
    retention_days: settings.retention_days,
    epoch: settings.epoch,
  };
}

export function memoryConflict(error: unknown): boolean {
  return /409|conflict|冲突|已被其他|已变化|刷新后重试|版本不一致|epoch|stale/i.test(
    error instanceof Error ? error.message : String(error),
  );
}

export function validateMemoryEntryInput(input: MemoryEntryInput, existingTaskID?: number): void {
  const bytes = (value: string) => new TextEncoder().encode(value).byteLength;
  if (!input.title.trim() || bytes(input.title) > 240) throw new Error("标题须为 1–240 个 UTF-8 字节");
  if (!input.content.trim() || bytes(input.content) > 4800) throw new Error("正文须为 1–4800 个 UTF-8 字节，长详情请保留在来源记录");
  if (bytes(input.target) > 512 || bytes(input.applicability) > 2000) throw new Error("目标或适用条件过长");
  if (input.tags.length > 16 || input.tags.some((tag) => Array.from(tag).length > 48)) throw new Error("最多 16 个标签，每个标签最多 48 个字符");
  if (input.kind !== "target" && input.target.trim()) throw new Error("通用经验不填写具体目标，请明确清空目标标识后保存，或另行新建目标记录");
  if (input.origin_task_id !== undefined && (!Number.isSafeInteger(input.origin_task_id) || input.origin_task_id <= 0)) {
    throw new Error("来源任务 ID 须为正整数");
  }
  const taskID = input.origin_task_id ?? existingTaskID;
  if (input.kind === "target" && (!Number.isSafeInteger(taskID) || !taskID || taskID <= 0 || !input.target.trim())) {
    throw new Error("目标记录须填写有效来源任务 ID 和目标标识，仅在原任务使用；无来源任务的已有经验请另行新建目标记录");
  }
}

export function assertMemoryEntrySharing(value: unknown): asserts value is MemoryEntry {
  if (!value || typeof value !== "object") throw new Error("服务未返回完整的经验条目");
  const entry = value as Record<string, unknown>;
  if (!Object.hasOwn(MEMORY_KIND_LABELS, String(entry.kind)) || !["global_experience", "source_only"].includes(String(entry.share_scope))) {
    throw new Error("服务未返回明确的经验共享范围，请确认后端版本");
  }
  if (entry.kind === "target" && entry.share_scope !== "source_only") {
    throw new Error("目标记录的共享范围不一致，请刷新核对；本次未标记为跨任务经验");
  }
}

export function memorySharingLabel(entry: Pick<MemoryEntry, "kind" | "share_scope" | "origin_task_id" | "origin_conversation_id">): string {
  if (entry.kind !== "target" && entry.share_scope === "global_experience") return "通用经验（可跨任务）";
  if (Number.isSafeInteger(entry.origin_task_id) && entry.origin_task_id > 0) return "仅原任务";
  if (Number.isSafeInteger(entry.origin_conversation_id) && entry.origin_conversation_id > 0) return "仅原会话";
  return "不跨任务（来源未登记）";
}

export function assertMemoryLearning(value: unknown): asserts value is MemoryLearningSnapshot {
  if (!value || typeof value !== "object" || Array.isArray(value)) throw new Error("服务未返回学习状态");
  const item = value as Record<string, unknown>;
  const counts = ["queued", "running", "failed", "processed", "starts_last_hour", "attempts_last_hour", "max_calls_per_hour"];
  const optionalCounts = ["retryable_failed", "stopped_failed", "entry_count"];
  const outcomes = item.outcome_counts;
  const invalidOutcomes = outcomes !== undefined && (
    !outcomes || typeof outcomes !== "object" || Array.isArray(outcomes) ||
    Object.entries(outcomes).some(([key, count]) => !Object.hasOwn(MEMORY_LEARNING_RESULT_LABELS, key) || !Number.isSafeInteger(count) || Number(count) < 0)
  );
  if (typeof item.status !== "string" || !Object.hasOwn(MEMORY_LEARNING_LABELS, item.status) || typeof item.message !== "string" ||
      counts.some((key) => !Number.isSafeInteger(item[key]) || Number(item[key]) < 0) ||
      optionalCounts.some((key) => item[key] !== undefined && (!Number.isSafeInteger(item[key]) || Number(item[key]) < 0)) ||
      invalidOutcomes ||
      Number(item.max_calls_per_hour) < 1 || Number(item.max_calls_per_hour) > 60 ||
      (item.last_result !== undefined && (typeof item.last_result !== "string" || !Object.hasOwn(MEMORY_LEARNING_RESULT_LABELS, item.last_result))) ||
      ["last_completed_at", "next_budget_at"].some((key) => item[key] !== undefined && (typeof item[key] !== "string" || !Number.isFinite(Date.parse(item[key] as string))))) {
    throw new Error("学习状态不完整或无效，当前进度未确认");
  }
}

export function memoryLearningOutcome(result?: MemoryLearningSnapshot["last_result"]): string {
  if (!result) return "尚无已确认的阶段总结结果。";
  const messages: Record<MemoryLearningResult, string> = {
    saved: "上次阶段曾保存经验；当前条目是否保留及有效，以记忆列表为准，使用前仍须核对适用条件。",
    empty: "上次模型返回了合法空结果（[]），本阶段未新增通用经验；不代表其他目标无需测试。",
    duplicate: "上次候选经验与已有条目重复，未新增条目；不等于模型明确返回空结果。",
    input_invalid: "上次阶段来源格式不满足总结条件，未将其记为正常空结果。",
    input_sensitive: "上次来源命中敏感信息保护，已停止提炼；未当作正常空结果。",
    output_invalid: "上次模型输出格式不合格，未将其当作“无经验”。",
    output_truncated: "上次模型输出被截断，未完成可信提炼；未记为正常空结果。",
    output_rejected: "上次候选未通过经验筛选，未新增条目；不等于模型明确返回空结果。",
    output_sensitive: "上次候选命中敏感信息保护，未保存；未当作正常空结果。",
    source_changed: "上次阶段来源已变化，原结果未保存；未报告提炼成功。",
    settings_changed: "上次总结期间设置已变化，原结果未保存；未报告提炼成功。",
    cleared: "上次阶段受清理操作影响，未保存新经验。",
    budget_deferred: "上次阶段等待实际 HTTP 发送预算，本轮未发送请求；不是提供方或 API 失败。",
    provider_error: "上次模型调用发生错误，未报告学习完成；这不是渗透任务失败计数。",
    timeout: "上次阶段总结超时，未报告学习完成；这不是渗透任务超时计数。",
    storage_error: "上次阶段结果保存发生错误，未报告保存成功，条目状态需重新核对。",
    lease_expired: "上次阶段处理租约已过期，未报告保存成功。",
    no_experience: "上次记录为历史未细分的“未新增经验”；原记录混合了空结果与拒绝等情况，具体原因未记录。",
    failed: "上次为历史失败记录，未保存具体失败类别；不代表渗透任务失败。",
    skipped: "上次为历史跳过记录，未保存具体跳过类别；未报告产生经验。",
    legacy_unknown: "上次历史结果未分类，未报告提炼成功。",
  };
  return messages[result];
}

export function memoryLearningOutcomeIsError(result?: MemoryLearningResult): boolean {
  return result !== undefined && ["input_invalid", "output_invalid", "output_truncated", "provider_error", "timeout", "storage_error", "lease_expired", "failed"].includes(result);
}

export function memorySourceHref(entry: Pick<MemoryEntry, "origin_task_id" | "origin_conversation_id" | "status">): string | undefined {
  if (entry.status === "source_invalid") return undefined;
  if (Number.isSafeInteger(entry.origin_task_id) && entry.origin_task_id > 0) {
    return `/function/tasks/detail?id=${entry.origin_task_id}`;
  }
  if (Number.isSafeInteger(entry.origin_conversation_id) && entry.origin_conversation_id > 0) {
    return `/chat?c=${entry.origin_conversation_id}`;
  }
  return undefined;
}

export function memorySourceLabel(entry: Pick<MemoryEntry, "origin_task_id" | "origin_conversation_id" | "status">): string {
  if (entry.status === "source_invalid") return "来源已失效";
  if (Number.isSafeInteger(entry.origin_task_id) && entry.origin_task_id > 0) return `任务 #${entry.origin_task_id}`;
  if (Number.isSafeInteger(entry.origin_conversation_id) && entry.origin_conversation_id > 0) {
    return `对话 #${entry.origin_conversation_id}`;
  }
  return "人工记忆";
}

export function memoryTime(value?: string | null): string {
  if (!value) return "—";
  const timestamp = Date.parse(value);
  return Number.isFinite(timestamp) ? new Date(timestamp).toLocaleString("zh-CN") : "时间无效";
}
