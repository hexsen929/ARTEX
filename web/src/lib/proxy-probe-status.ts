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
  if (result.probe_mode !== undefined) {
    if (!["configured", "default"].includes(result.probe_mode) || typeof result.probe_url !== "string" || typeof result.http_skipped !== "boolean") {
      throw new Error("诊断探测服务未能核对，请重试");
    }
    if (result.probe_mode === "configured") {
      if (!/^https?:\/\//i.test(result.probe_url) || result.http_skipped !== true) throw new Error("诊断探测服务未能核对，请重试");
    } else if (result.probe_url !== "" || result.http_skipped !== false) throw new Error("诊断探测服务未能核对，请重试");
  } else if (result.probe_url !== undefined || result.http_skipped !== undefined) {
    throw new Error("诊断探测服务未能核对，请重试");
  }
  return result;
}

export function proxyDiagnosisPrimaryLabel(result: ProxyPoolDiagnosis): string {
  if (result.probe_mode !== "configured") return "HTTPS（验证证书）";
  return /^https:\/\//i.test(result.probe_url ?? "") ? "已保存回显服务（HTTPS，验证证书）" : "已保存回显服务（HTTP，明文回显）";
}
