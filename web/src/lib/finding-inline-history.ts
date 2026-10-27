export function inlineReportPayload(value: unknown): Record<string, unknown> {
  let payload = value;
  if (typeof payload === "string") {
    try { payload = JSON.parse(payload); } catch { return {}; }
  }
  return payload && typeof payload === "object" && !Array.isArray(payload)
    ? payload as Record<string, unknown>
    : {};
}

// Return text only. The consumer renders it as React text, never HTML/Markdown.
export function inlineReportText(value: unknown): string {
  if (value == null) return "";
  if (typeof value === "string") return value;
  try { return JSON.stringify(value, null, 2) ?? ""; } catch { return ""; }
}

export function inlineHistoryOrder<T extends { id: string; created_at: string }>(events: T[]): T[] {
  const time = (value: string) => {
    const timestamp = Date.parse(value);
    return Number.isFinite(timestamp) ? timestamp : 0;
  };
  return [...events].sort((a, b) => {
    const delta = time(a.created_at) - time(b.created_at);
    if (delta !== 0) return delta;
    // Audit IDs are int64 strings. Never coerce them to lossy JS Numbers.
    if (/^\d+$/.test(a.id) && /^\d+$/.test(b.id)) {
      const left = BigInt(a.id), right = BigInt(b.id);
      return left < right ? -1 : left > right ? 1 : 0;
    }
    return a.id.localeCompare(b.id);
  });
}
