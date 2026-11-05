import type { EgressProbeResult, ProxyPoolDiagnosis, ProxyPoolEntry } from "./types";

export function proxyProbeStatus(entry: ProxyPoolEntry, threshold: number) {
  if (!entry.enabled) return { label: "已停用", tone: "muted" };
  if (entry.last_error && entry.consecutive_failures > 0) {
    return entry.health === "unhealthy-cooling"
      ? { label: "失效冷却中", tone: "failed" }
      : { label: `检测失败（${entry.consecutive_failures}/${threshold}）`, tone: "failed" };
  }
  if (entry.health === "unhealthy-cooling") return { label: "失效冷却中", tone: "failed" };
  if (entry.last_error) return { label: "最近检测失败", tone: "failed" };
  if (entry.health === "healthy") return { label: "健康", tone: "healthy" };
  return { label: "尚未检测", tone: "muted" };
}

function validResult(result: EgressProbeResult) {
  return !!result && typeof result.ok === "boolean" && Number.isFinite(result.latency_ms) && result.latency_ms >= 0 &&
    (result.ok ? typeof result.exit_ip === "string" && result.exit_ip.length > 0 : typeof result.error === "string") &&
    Array.isArray(result.probes) && result.probes.length <= 2 && result.probes.every((attempt) =>
      !!attempt && typeof attempt.url === "string" && typeof attempt.ok === "boolean" && Number.isFinite(attempt.latency_ms));
}

export function assertProxyDiagnosis(result: ProxyPoolDiagnosis, id: string): ProxyPoolDiagnosis {
  if (!result || result.id !== id || result.health_updated !== false || !Number.isFinite(Date.parse(result.checked_at)) ||
    !validResult(result.https) || !validResult(result.http)) throw new Error("诊断结果未能核对，请重试");
  return result;
}
