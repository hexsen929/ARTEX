"use strict";
const assert = require("node:assert/strict");
const fs = require("node:fs");
const path = require("node:path");
const http = require("node:http");
const { once } = require("node:events");
if (!process.env.ARTEX_PLAYWRIGHT_DIR) throw new Error("An isolated Playwright runtime is required");
const { chromium } = require(path.resolve(process.env.ARTEX_PLAYWRIGHT_DIR));
const output = path.resolve(__dirname, "../out");
const stamp = "2026-11-11T00:00:00Z";
const task = { id: "701", name: "恢复机制合成任务", description: "Synthetic recovery only", goal: "fixture", status: "running", paused: false, queued: false, source_task_ids: [], company_ids: [], llm_profile_ids: [], created_at: stamp, goals_total: 1, goals_met: 0, in_flight: 0, run_seconds: 0, findings: { critical: 0, high: 0, medium: 0, low: 0 } };
const node = { id: "901", type: "intent", payload: "RECOVERY_FIXTURE_WORKER", state: "blocked", priority: 5, origin: "fixture", ts: stamp,
  worker_result: { code: "transport_failure", message: "模型连接中断", at: stamp },
  model_recovery: { attempts: 0, limit: 2, status: "retry_wait", code: "transport_failure", message: "模型调用中断，系统正在退避等待，随后续接原会话，无需新建意图。" },
  execution_attempt: 1, execution_checkpoint: { tool_calls: 1, tool_results: 1, pending: {}, settled: true } };
let retries = 0;
const requests = [], unexpectedWrites = [], external = [], errors = [];
function json(res, value, code = 200) { res.writeHead(code, { "Content-Type": "application/json", "Cache-Control": "no-store" }); res.end(JSON.stringify(value)); }
async function api(req, res, url) {
  const p = url.pathname; requests.push(p);
  if (req.method === "POST" && p === "/api/tasks/701/intents/901/rerun") {
    retries++; node.state = "open"; return json(res, { id: "701", reopened: 901, queued: false });
  }
  if (req.method !== "GET") {
    if (p === "/api/sse-ticket") return json(res, { ticket: "fixture" });
    unexpectedWrites.push(p); return json(res, { error: "Unexpected fixture mutation" }, 409);
  }
  if (p === "/api/tasks/701") return json(res, task);
  if (p === "/api/tasks") return json(res, { tasks: [task], active: "" });
  if (p === "/api/auth/status") return json(res, { initialized: true });
  if (p === "/api/version" || p === "/api/health") return json(res, { ok: true, version: "fixture" });
  if (p === "/api/update/check") return json(res, { has_update: false, current: "fixture" });
  if (p === "/api/settings/egress") return json(res, { show_egress_ip: false });
  if (p === "/api/intercept/pending") return json(res, { pending: [] });
  if (p === "/api/exploration/intents") return json(res, { items: [node], has_more: false });
  if (p === "/api/exploration/nodes") return json(res, { items: [node], total: 1, page: 1, size: 20, edges: [], refs: {}, assets: {} });
  if (p === "/api/exploration/activity/history") return json(res, { items: [], snapshot_cursor: 0, earliest_cursor: 0, has_more: false });
  if (p === "/api/exploration/main-sessions") return json(res, { sessions: [{ seq: 0, created_at: stamp }], current: 0 });
  if (p === "/api/exploration/activity") return json(res, { items: [], cursor: 0 });
  if (p === "/api/skills/missing") return json(res, { missing: [] });
  if (p.includes("/stream") || p.includes("/events")) { res.writeHead(200, { "Content-Type": "text/event-stream" }); res.write(": fixture\n\n"); return; }
  if (p === "/api/llm/profiles") return json(res, { profiles: [] });
  if (p === "/api/agents") return json(res, { agents: [] });
  return json(res, { items: [], assets: [], tasks: [], entries: [], total: 0, active: 0 });
}
const server = http.createServer((req, res) => {
  const url = new URL(req.url, "http://127.0.0.1");
  if (url.pathname.startsWith("/api/")) { void api(req, res, url).catch((e) => { errors.push(String(e)); json(res, {}, 500); }); return; }
  let f = path.join(output, decodeURIComponent(url.pathname));
  if (url.pathname.endsWith("/")) f = path.join(f, "index.html");
  if (!f.startsWith(output + path.sep) || !fs.existsSync(f) || !fs.statSync(f).isFile()) { res.writeHead(404); res.end(); return; }
  const ext = path.extname(f);
  res.writeHead(200, { "Content-Type": ({ ".html": "text/html", ".js": "text/javascript", ".css": "text/css", ".json": "application/json", ".svg": "image/svg+xml", ".png": "image/png" })[ext] || "application/octet-stream" });
  fs.createReadStream(f).pipe(res);
});
(async () => {
  let browser;
  try {
    server.listen(0, "127.0.0.1"); await once(server, "listening");
    const origin = `http://127.0.0.1:${server.address().port}`;
    browser = await chromium.launch({ headless: true, ...(process.env.ARTEX_PLAYWRIGHT_EXECUTABLE ? { executablePath: process.env.ARTEX_PLAYWRIGHT_EXECUTABLE } : {}) });
    const page = await browser.newPage({ viewport: { width: 1440, height: 1000 } });
    page.on("pageerror", (e) => errors.push(String(e)));
    await page.route("**/*", (route) => { const u = route.request().url(); if (u.startsWith(origin + "/") || u.startsWith("data:")) return route.continue(); external.push(u); return route.abort(); });
    await page.addInitScript(() => { localStorage.setItem("artex_token", "synthetic-fixture"); document.cookie = "artex_session=1; path=/"; });
    await page.goto(origin + "/function/tasks/detail/?id=701", { waitUntil: "domcontentloaded" });
    const worker = page.getByText("RECOVERY_FIXTURE_WORKER", { exact: false });
    await worker.first().click({ timeout: 15000 });
    await page.getByText(node.model_recovery.message, { exact: true }).waitFor({ timeout: 15000 });
    const button = page.getByRole("button", { name: "恢复此意图", exact: true });
    await button.waitFor();
    assert.equal(retries, 0, "Viewing recovery status must not manually replay work");
    if (process.env.ARTEX_UI_ARTIFACTS) { fs.mkdirSync(process.env.ARTEX_UI_ARTIFACTS, { recursive: true }); await page.screenshot({ path: path.join(process.env.ARTEX_UI_ARTIFACTS, "automatic-recovery.png") }); }
    await button.click();
    await page.getByText("Worker #901 已安排恢复，将续接原会话与已有结果", { exact: true }).waitFor();
    assert.equal(retries, 1, "Manual fallback must recover the same original intent once");
    assert.deepEqual(unexpectedWrites, []); assert.deepEqual(external, []); assert.deepEqual(errors, []);
    console.log("PASS recovery status is visible in the worker session and its fallback reopens only the original intent");
  } finally {
    if (browser) await browser.close();
    server.closeAllConnections(); await new Promise((resolve) => server.close(resolve));
  }
})().catch((error) => { console.error(error); console.error(JSON.stringify({ requests, unexpectedWrites, external, errors })); process.exitCode = 1; });
