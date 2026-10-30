import type { NotificationMeta } from "./types";

export function notificationDigestMinutes(raw: string, fallback: number): number {
  const value = raw.trim() === "" ? fallback : Number(raw);
  if (!Number.isInteger(value) || value < 1 || value > 1440) {
    throw new Error("汇总周期需为 1 到 1440 分钟的整数，未保存任何设置");
  }
  return value;
}

export function assertNotificationMeta(value: unknown): asserts value is NotificationMeta {
  if (!value || typeof value !== "object" || Array.isArray(value)) {
    throw new Error("通知配置响应不完整，已有设置保持不变");
  }
  const meta = value as Record<string, unknown>;
  const defaults = meta.defaults as Record<string, unknown> | undefined;
  const stats = meta.stats as Record<string, unknown> | undefined;
  if (
    typeof meta.enabled !== "boolean" || typeof meta.public_base_url !== "string" ||
    typeof meta.digest_interval_min !== "string" || !Array.isArray(meta.kinds) ||
    !defaults || !stats || typeof defaults.digest_interval_min !== "number"
  ) throw new Error("通知配置响应不完整，已有设置保持不变");
  notificationDigestMinutes("", defaults.digest_interval_min);
  notificationDigestMinutes(meta.digest_interval_min, defaults.digest_interval_min);
  for (const key of ["channels", "channels_on", "pending", "failed", "sent_today", "backlog_age_ms"]) {
    if (typeof stats[key] !== "number" || !Number.isFinite(stats[key]) || (stats[key] as number) < 0) {
      throw new Error("通知统计响应不完整，已有设置保持不变");
    }
  }
  for (const kind of meta.kinds) {
    if (!kind || typeof kind !== "object" || typeof kind.kind !== "string" ||
      typeof kind.default_rate_per_min !== "number" || !Number.isFinite(kind.default_rate_per_min) ||
      !Array.isArray(kind.secret_keys) || kind.secret_keys.some((key: unknown) => typeof key !== "string")) {
      throw new Error("通知渠道目录响应不完整，暂不接受修改");
    }
  }
}

export function readNotificationSettings(value: unknown): {
  enabled: boolean; publicBaseURL: string; digestMinutes: number;
} {
  if (!value || typeof value !== "object" || Array.isArray(value)) {
    throw new Error("保存响应不完整，请重新读取后确认；未覆盖当前草稿");
  }
  const settings = value as Record<string, unknown>;
  if (typeof settings.notify_enabled !== "boolean" || typeof settings.notify_public_base_url !== "string" ||
    typeof settings.notify_digest_interval_min !== "number") {
    throw new Error("保存响应不完整，请重新读取后确认；未覆盖当前草稿");
  }
  return {
    enabled: settings.notify_enabled,
    publicBaseURL: settings.notify_public_base_url,
    digestMinutes: notificationDigestMinutes(String(settings.notify_digest_interval_min), 30),
  };
}
