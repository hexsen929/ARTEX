import type { EgressProbeResult, ProxyCapabilityCheck, ProxyPoolDiagnosis, ProxyPoolEntry } from "./types";

export function proxyProtocolStatus(entry: ProxyPoolEntry) {
  if (!entry.protocol_auto) return "";
  if (entry.detected_protocol) return `已识别 ${entry.detected_protocol.toUpperCase()}`;
  const recognized = entry.protocol_checks?.filter((check) => check.recognized);
  if (recognized?.length) return `协议响应：${recognized.map((check) => `${check.protocol.toUpperCase()}${check.supported ? "" : "（暂不支持转发）"}`).join("、")}；尚未通过完整访问验证`;
  if (entry.protocol_checks?.some((check) => check.http.ok)) return "仅 HTTP 回显通过；未确认代理身份";
  return "协议待识别";
}

export function proxyProbeStatus(entry: ProxyPoolEntry, threshold: number) {
  if (!entry.enabled) return { label: "已停用", tone: "muted" };
  if (entry.protocol_auto && !entry.detected_protocol && entry.protocol_checks?.some((check) => check.http.ok || check.https.ok)) return { label: "部分检查通过 · 未完整可用", tone: "limited" };
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

function validCapability(check: ProxyCapabilityCheck) {
  return !!check && typeof check.checked === "boolean" && typeof check.ok === "boolean" &&
    Number.isFinite(check.latency_ms) && check.latency_ms >= 0 &&
    (check.checked ? (check.ok ? typeof check.ip === "string" && check.ip.length > 0 : typeof check.error === "string") : !check.ok);
}

export function assertProxyDiagnosis(result: ProxyPoolDiagnosis, id: string): ProxyPoolDiagnosis {
  if (!result || result.id !== id || result.health_updated !== false || !Number.isFinite(Date.parse(result.checked_at)) ||
    !validResult(result.https) || !validResult(result.http)) throw new Error("诊断结果未能核对，请重试");
  const checks = result.https.protocol_checks;
  if (checks !== undefined && (!Array.isArray(checks) || checks.length !== 4 || new Set(checks.map((check) => check.protocol)).size !== 4 || checks.some((check) =>
    !["http", "https", "socks5", "socks4"].includes(check.protocol) || typeof check.recognized !== "boolean" ||
    typeof check.supported !== "boolean" || !validCapability(check.http) || !validCapability(check.https)))) throw new Error("协议诊断结果不完整，请重试");
  return result;
}
