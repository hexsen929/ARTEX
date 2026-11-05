import assert from "node:assert/strict";
import test from "node:test";
import { assertProxyDiagnosis, proxyProbeStatus } from "./proxy-probe-status.ts";

const entry = { id: "fixture", url: "socks5://proxy.invalid:1080", enabled: true, health: "unknown", consecutive_failures: 0 };

test("a checked failure below threshold is never labeled untested or healthy", () => {
  const failed = { ...entry, last_error: "全部探测地址均失败", consecutive_failures: 1 };
  assert.deepEqual(proxyProbeStatus(failed, 3), { label: "检测失败（1/3）", tone: "failed" });
  assert.equal(proxyProbeStatus({ ...failed, health: "healthy", egress_ip: "192.0.2.4" }, 3).tone, "failed");
  assert.equal(proxyProbeStatus({ ...failed, health: "unhealthy-cooling" }, 3).label, "失效冷却中");
  assert.equal(proxyProbeStatus({ ...failed, enabled: false }, 3).label, "已停用");
  assert.equal(proxyProbeStatus(entry, 3).label, "尚未检测");
  assert.equal(proxyProbeStatus({ ...entry, health: "healthy" }, 3).label, "健康");
});

const report = { id: "fixture", https: { ok: false, error: "TLS 证书链不受信任", latency_ms: 10, probes: [] }, http: { ok: true, exit_ip: "192.0.2.5", latency_ms: 12, probes: [] }, checked_at: "2026-11-05T00:00:00Z", health_updated: false, scope: "fixture" };
test("HTTP-only success preserves the failed HTTPS result", () => {
  assert.equal(assertProxyDiagnosis(report, "fixture"), report);
  assert.equal(report.https.ok, false);
  assert.equal(report.health_updated, false);
});
test("partial or wrong-member responses are rejected", () => {
  for (const patch of [{ id: "other" }, { health_updated: true }, { checked_at: "invalid" }, { https: null }, { http: { ...report.http, exit_ip: "" } }, { https: { ...report.https, probes: null } }]) assert.throws(() => assertProxyDiagnosis({ ...report, ...patch }, "fixture"));
});
