#!/usr/bin/env node
/* Cloud-only regression: static export plus synthetic API responses. No ARTEX
 * process, real database, model endpoint, target or system clipboard is used. */
"use strict";
const assert = require("node:assert/strict");
const fs = require("node:fs");
const http = require("node:http");
const path = require("node:path");
const { once } = require("node:events");
if (!process.env.CI || !process.env.ARTEX_PLAYWRIGHT_DIR) throw new Error("Run in GitHub Actions with the temporary Playwright runtime.");
const { chromium } = require(path.resolve(process.env.ARTEX_PLAYWRIGHT_DIR));
const output = path.resolve(__dirname, "..", "out");
const stamp = "2026-10-30T00:00:00Z";
const state = { recordsFail: true, settingsFail: true, tasksFail: true, secondDetailFail: true, conversationUsageFail: true };
const requests = [], unexpected = new Set(), external = new Set();
const records = [1, 2].map((id) => ({ id, ts: stamp, model: `UI_FIXTURE_MODEL_${id}`, profile_name: `UI_FIXTURE_PROFILE_${id}`, session_id: `fixture-${id}`, task_id: "42", worker: "worker", latency_ms: 100, input_tokens: 10, output_tokens: 2, cache_read: 0, cache_write: 0, status: "ok" }));
function json(response, code, value) {
  response.writeHead(code, { "Content-Type": "application/json", "Cache-Control": "no-store" });
  response.end(JSON.stringify(value));
}
function error(response, message) { json(response, 503, { error: message }); }
async function serveAPI(request, response, url) {
  const endpoint = url.pathname, method = request.method || "GET";
  requests.push({ endpoint, method, query: Object.fromEntries(url.searchParams) });
  if (endpoint === "/api/health") return json(response, 200, { ok: true, service: "synthetic", version: "fixture" });
  if (endpoint === "/api/auth/status") return json(response, 200, { initialized: true });
  if (endpoint === "/api/update/check") return json(response, 200, { has_update: false, current: "fixture" });
  if (endpoint === "/api/intercept/pending") return json(response, 200, { pending: [] });
  if (endpoint === "/api/skills/missing") return json(response, 200, { missing: [] });
  if (endpoint === "/api/settings" && method === "GET") {
    if (state.settingsFail) return error(response, "UI_FIXTURE_SETTINGS_UNAVAILABLE");
    return json(response, 200, { llm_record: false, show_egress_ip: false, global_proxy: "", traffic_capture: false });
  }
  if (endpoint === "/api/settings" && method === "PUT") return error(response, "UI_FIXTURE_RECORDING_SAVE_FAILED");
  if (endpoint === "/api/llm/records" && method === "GET") {
    if (state.recordsFail) return error(response, "UI_FIXTURE_LIST_UNAVAILABLE");
    return json(response, 200, { records, total: records.length });
  }
  if (endpoint === "/api/llm/records" && method === "DELETE") return error(response, "UI_FIXTURE_DELETE_FAILED");
  if (endpoint === "/api/llm/records/tasks") {
    if (state.tasksFail) return error(response, "UI_FIXTURE_TASK_LIST_UNAVAILABLE");
    return json(response, 200, { tasks: [{ task_id: "42", count: 2 }] });
  }
  if (endpoint === "/api/llm/records/1") {
    await new Promise((resolve) => setTimeout(resolve, 1500));
    return json(response, 200, { ...records[0], request_body: "UI_FIXTURE_OLD_REQUEST", response_body: "UI_FIXTURE_OLD_RESPONSE" });
  }
  if (endpoint === "/api/llm/records/2") {
    if (state.secondDetailFail) return error(response, "UI_FIXTURE_DETAIL_UNAVAILABLE");
    return json(response, 200, { ...records[1], request_body: "UI_FIXTURE_CURRENT_REQUEST", response_body: "UI_FIXTURE_CURRENT_RESPONSE" });
  }
  if (endpoint === "/api/tasks") return json(response, 200, { tasks: [], active: "" });
  if (endpoint === "/api/exploration/findings") return json(response, 200, []);
  if (endpoint === "/api/stats") return json(response, 200, { assets: 0, findings_confirmed: 0, engine_mode: "idle", llm_configured: false, roe_enabled: true });
  if (endpoint === "/api/exploration/activity") return json(response, 200, { items: [], cursor: 0 });
  if (endpoint === "/api/exploration/tokens") return json(response, 200, { workers: [], sessions: [], total: { input_tokens: 0, output_tokens: 0, cache_read_tokens: 0, cache_write_tokens: 0 } });
  if (endpoint === "/api/tokens/conversations") {
    if (state.conversationUsageFail) return error(response, "UI_FIXTURE_USAGE_UNAVAILABLE");
    return json(response, 200, [{ llm_profile_id: null, created_at: stamp, input_tokens: 134, output_tokens: 12, cache_read_tokens: 0, cache_write_tokens: 0 }]);
  }
  if (endpoint === "/api/tokens/usage") return json(response, 200, { by_profile: [], daily: [] });
  if (endpoint === "/api/traffic") return json(response, 200, { exchanges: [], total: 0 });
  if (endpoint === "/api/assets/counts") return json(response, 200, {});
  for (const [route, key] of [["/api/agents", "agents"], ["/api/mcp", "servers"], ["/api/skills", "skills"], ["/api/tools", "tools"], ["/api/llm/profiles", "profiles"]]) {
    if (endpoint === route) return json(response, 200, { [key]: [] });
  }
  unexpected.add(`${method} ${endpoint}`);
  return json(response, 404, { error: "Unmocked synthetic endpoint" });
}
const mime = { ".html": "text/html", ".js": "text/javascript", ".css": "text/css", ".json": "application/json", ".svg": "image/svg+xml", ".png": "image/png", ".woff2": "font/woff2", ".ico": "image/x-icon", ".txt": "text/plain" };
const server = http.createServer(async (request, response) => {
  try {
    const url = new URL(request.url || "/", "http://127.0.0.1");
    if (url.pathname.startsWith("/api/")) return await serveAPI(request, response, url);
    let file = path.resolve(output, "." + decodeURIComponent(url.pathname));
    if (file !== output && !file.startsWith(output + path.sep)) { response.writeHead(403); response.end(); return; }
    if (fs.existsSync(file) && fs.statSync(file).isDirectory()) file = path.join(file, "index.html");
    if (!fs.existsSync(file) || !fs.statSync(file).isFile()) { response.writeHead(404); response.end("Fixture file missing"); return; }
    response.writeHead(200, { "Content-Type": mime[path.extname(file)] || "application/octet-stream", "Cache-Control": "no-store" });
    fs.createReadStream(file).pipe(response);
  } catch { json(response, 500, { error: "Fixture server failed" }); }
});
async function eventually(check, label) {
  const deadline = Date.now() + 20000;
  while (Date.now() < deadline) { if (await check()) return; await new Promise((resolve) => setTimeout(resolve, 50)); }
  assert.fail(label);
}
let browser, page;
async function main() {
  server.listen(0, "127.0.0.1"); await once(server, "listening");
  const origin = `http://127.0.0.1:${server.address().port}`;
  browser = await chromium.launch({ headless: true });
  const context = await browser.newContext({ viewport: { width: 1680, height: 1100 }, locale: "zh-CN", timezoneId: "Asia/Shanghai" });
  await context.route("**/*", async (route) => {
    const url = new URL(route.request().url());
    if (url.origin === origin || ["data:", "blob:"].includes(url.protocol)) return route.continue();
    external.add(url.origin); return route.abort("blockedbyclient");
  });
  const token = [Buffer.from('{"alg":"none"}').toString("base64url"), Buffer.from('{"sub":"fixture-admin","exp":4102444800}').toString("base64url"), "fixture"].join(".");
  await context.addInitScript((token) => { localStorage.setItem("artex_token", token); document.cookie = "artex_session=1; path=/; SameSite=Lax"; document.cookie = "sidebar_state=false; path=/; SameSite=Lax"; }, token);
  page = await context.newPage(); page.setDefaultTimeout(20000);
  const errors = []; page.on("pageerror", (error) => errors.push(error.message));
  await page.goto(origin + "/function/llm-records/", { waitUntil: "networkidle" });
  await page.getByText("状态未知", { exact: true }).waitFor();
  assert.ok(await page.locator("#llm-rec-toggle").isDisabled());
  await page.getByText("当前记录读取失败，请重试", { exact: true }).waitFor();
  assert.equal(await page.getByText("暂无 LLM 调用记录", { exact: true }).count(), 0);
  assert.equal(requests.filter((r) => r.method === "PUT").length, 0);
  console.log("PASS recording status/list unknown is not disabled/empty success");
  state.settingsFail = state.recordsFail = state.tasksFail = false;
  await page.getByRole("button", { name: "刷新记录与状态", exact: true }).click();
  await page.getByText("UI_FIXTURE_MODEL_2", { exact: true }).waitFor();
  await eventually(async () => !(await page.locator("#llm-rec-toggle").isDisabled()), "Recorder state did not become known");
  await page.getByRole("row").filter({ hasText: "UI_FIXTURE_MODEL_2" }).click();
  await page.getByText(/详情读取失败：UI_FIXTURE_DETAIL_UNAVAILABLE/).waitFor();
  state.secondDetailFail = false;
  await page.getByRole("button", { name: "刷新记录与状态", exact: true }).click();
  await page.getByText("UI_FIXTURE_CURRENT_REQUEST", { exact: true }).waitFor();
  await page.getByRole("row").filter({ hasText: "UI_FIXTURE_MODEL_1" }).click();
  await eventually(() => requests.some((r) => r.endpoint === "/api/llm/records/1"), "Delayed detail was never requested");
  await page.getByRole("row").filter({ hasText: "UI_FIXTURE_MODEL_2" }).click();
  await page.getByText("UI_FIXTURE_CURRENT_REQUEST", { exact: true }).waitFor();
  await new Promise((resolve) => setTimeout(resolve, 1700));
  assert.equal(await page.getByText("UI_FIXTURE_OLD_REQUEST", { exact: true }).count(), 0);
  console.log("PASS detail failures are visible and late payloads do not cross records");
  await page.locator("#llm-rec-toggle").click();
  await page.getByText("状态未知", { exact: true }).waitFor();
  assert.ok(await page.locator("#llm-rec-toggle").isDisabled());
  assert.equal(await page.locator("#llm-rec-toggle").getAttribute("data-state"), "unchecked");
  console.log("PASS failed recorder save keeps prior state and requires refresh");
  await page.goto(origin + "/dashboard/", { waitUntil: "networkidle" });
  const summary = page.getByTestId("dashboard-token-summary");
  await summary.getByText("未知", { exact: true }).waitFor();
  await page.getByTestId("dashboard-token-usage").getByText(/当前用量未知，不表示零消耗/).waitFor();
  state.conversationUsageFail = false;
  await eventually(async () => (await summary.innerText()).includes("146"), "Conversation usage did not recover");
  state.conversationUsageFail = true;
  await page.getByTestId("dashboard-token-usage").getByText(/保留上次成功数据，数值可能已过期/).waitFor();
  assert.ok((await summary.innerText()).includes("146"));
  console.log("PASS usage failures preserve last successful totals rather than zero");
  assert.deepEqual(errors, [], "Browser exceptions");
  assert.deepEqual([...unexpected], [], "Unexpected API contracts");
  assert.deepEqual([...external], [], "External request attempted");
  await context.close();
}
main().catch(async (error) => {
  console.error(error);
  if (page) { console.error("Recent fixture requests:", JSON.stringify(requests.slice(-16))); console.error((await page.locator("body").innerText().catch(() => "")).slice(0, 5000)); }
  process.exitCode = 1;
}).finally(async () => { if (browser) await browser.close(); await new Promise((resolve) => server.close(resolve)); });
