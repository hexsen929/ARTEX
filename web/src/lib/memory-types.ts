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
  last_completed_at?: string;
  last_result?: "saved" | "no_experience" | "failed" | "skipped";
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
  if (!value || typeof value !== "object") throw new Error("服务未返回学习状态");
  const item = value as Record<string, unknown>;
  const counts = ["queued", "running", "failed", "processed", "starts_last_hour", "attempts_last_hour", "max_calls_per_hour"];
  if (!Object.hasOwn(MEMORY_LEARNING_LABELS, String(item.status)) || typeof item.message !== "string" ||
      counts.some((key) => !Number.isSafeInteger(item[key]) || Number(item[key]) < 0) ||
      Number(item.max_calls_per_hour) < 1 || Number(item.max_calls_per_hour) > 60 ||
      (item.last_result !== undefined && !["saved", "no_experience", "failed", "skipped"].includes(String(item.last_result))) ||
      (item.last_completed_at !== undefined && (typeof item.last_completed_at !== "string" || !Number.isFinite(Date.parse(item.last_completed_at))))) {
    throw new Error("学习状态不完整或无效，当前进度未确认");
  }
}

export function memoryLearningOutcome(result?: MemoryLearningSnapshot["last_result"]): string {
  if (result === "saved") return "上次阶段总结已保存经验，使用前仍须核对适用条件。";
  if (result === "no_experience") return "上次阶段总结已完成，未发现值得保存的通用经验；这不是学习失败。";
  if (result === "failed") return "上次阶段总结失败，未报告学习完成。";
  if (result === "skipped") return "上次阶段总结已跳过，未报告产生经验。";
  return "尚无已确认的阶段总结结果。";
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
