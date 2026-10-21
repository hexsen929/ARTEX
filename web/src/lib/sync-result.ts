import type { SSSyncResult, SSSyncStatus } from "./types.ts";

export const syncStatusLabels: Record<SSSyncStatus, string> = {
  running: "同步中",
  success: "成功",
  partial: "部分成功",
  failed: "失败",
  interrupted: "已中断",
};

// Retain truthful feedback when connected to an older backend without status.
export function syncResultStatus(result: SSSyncResult): SSSyncStatus {
  if (result.status && result.status !== "success") return result.status;
  const writes = Object.values(result.synced).reduce((sum, count) => sum + count, 0);
  if (
    (result.errors?.length ?? 0) > 0 ||
    (result.stats?.failed ?? 0) > 0 ||
    (result.stats?.derived_ip_failed ?? 0) > 0
  ) {
    return writes > 0 ? "partial" : "failed";
  }
  if (
    (result.warnings?.length ?? 0) > 0 ||
    (result.stats?.skipped ?? 0) > 0 ||
    result.targets?.some((target) => target.truncated || !!target.error)
  )
    return "partial";
  return "success";
}
