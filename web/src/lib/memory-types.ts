export const MEMORY_KIND_LABELS = {
  environment: "环境与工具",
  procedure: "方法与流程",
  lesson: "排错与经验",
  target: "目标历史",
} as const;

export const MEMORY_STATUS_LABELS = {
  active: "有效",
  disabled: "已停用",
  expired: "已过期",
  source_invalid: "来源失效",
} as const;

export type MemoryKind = keyof typeof MEMORY_KIND_LABELS;
export type MemoryStatus = keyof typeof MEMORY_STATUS_LABELS;

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
  // Runtime model availability is read-only and never echoed into a PUT.
  const { model_available: _available, model_error: _error, learn_after: _learnAfter, ...input } = settings;
  return input;
}

export function memoryConflict(error: unknown): boolean {
  return /409|conflict|冲突|已被其他|已变化|刷新后重试|版本不一致|epoch|stale/i.test(
    error instanceof Error ? error.message : String(error),
  );
}

export function validateMemoryEntryInput(input: MemoryEntryInput): void {
  const bytes = (value: string) => new TextEncoder().encode(value).byteLength;
  if (!input.title.trim() || bytes(input.title) > 240) throw new Error("标题须为 1–240 个 UTF-8 字节");
  if (!input.content.trim() || bytes(input.content) > 4800) throw new Error("正文须为 1–4800 个 UTF-8 字节，长详情请保留在来源记录");
  if (bytes(input.target) > 512 || bytes(input.applicability) > 2000) throw new Error("目标或适用条件过长");
  if (input.tags.length > 16 || input.tags.some((tag) => Array.from(tag).length > 48)) throw new Error("最多 16 个标签，每个标签最多 48 个字符");
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
