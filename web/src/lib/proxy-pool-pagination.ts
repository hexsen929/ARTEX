export const PROXY_POOL_PAGE_SIZE = 50;
export const PROXY_IMPORT_ERROR_LIMIT = 5;

export function proxyLastCheckFailed(entry: { last_checked_at?: string; last_error?: string; consecutive_failures: number }): boolean {
  return !!entry.last_checked_at && !entry.last_checked_at.startsWith("0001-") &&
    Number.isFinite(Date.parse(entry.last_checked_at)) && !!entry.last_error && entry.consecutive_failures > 0;
}

export function proxyCleanupEligible(entry: { health: string; last_checked_at?: string; last_error?: string; consecutive_failures: number }, threshold: number): boolean {
  return (entry.health === "unhealthy-cooling" || entry.health === "disabled") &&
    proxyLastCheckFailed(entry) &&
    entry.consecutive_failures >= Math.max(1, threshold);
}

type SearchableProxy = {
  id: string;
  url: string;
  name?: string;
  remark?: string;
  egress_ip?: string;
  health?: string;
  enabled: boolean;
};

// Search only the address that is already masked by the server. Defensively
// discard URL userinfo too, so a mock/older response cannot make secrets
// searchable. Never add the original import text to this index.
export function proxySearchAddress(url: string): string {
  return url.replace(/^(\w+:\/\/)[^/]*@/, "$1***@");
}

export function filterProxyPool<T extends SearchableProxy>(entries: T[], query: string): T[] {
  const words = query.trim().toLowerCase().split(/\s+/).filter(Boolean);
  if (words.length === 0) return entries;
  return entries.filter((entry) => {
    const text = [
      entry.id, proxySearchAddress(entry.url), entry.name, entry.remark,
      entry.egress_ip, entry.health, entry.enabled ? "已启用" : "已停用",
    ].filter(Boolean).join(" ").toLowerCase();
    return words.every((word) => text.includes(word));
  });
}

export function proxyPoolPage<T>(entries: T[], requestedPage: number) {
  const pages = Math.max(1, Math.ceil(entries.length / PROXY_POOL_PAGE_SIZE));
  const page = Math.min(pages, Math.max(1, Number.isFinite(requestedPage) ? Math.floor(requestedPage) : 1));
  const start = (page - 1) * PROXY_POOL_PAGE_SIZE;
  return { items: entries.slice(start, start + PROXY_POOL_PAGE_SIZE), page, pages };
}

export function summarizeProxyImportErrors(errors: Array<{ line: number; error: string }>): string {
  const sample = errors.slice(0, PROXY_IMPORT_ERROR_LIMIT)
    .map((error) => `第${error.line}行：${error.error.slice(0, 160)}`).join("；");
  return `共 ${errors.length} 行导入失败${errors.length > PROXY_IMPORT_ERROR_LIMIT ? "（仅显示前 5 条）" : ""}：${sample}`;
}
