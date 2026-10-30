#!/usr/bin/env node
/* Cloud-only synthetic notification/tool UI regression. Every mutation and
 * "test" call is mocked; no message, command, model or external target runs. */
"use strict";
const assert = require("node:assert/strict");
const fs = require("node:fs");
const path = require("node:path");
const http = require("node:http");
const { once } = require("node:events");
if (!process.env.CI || !process.env.ARTEX_PLAYWRIGHT_DIR) throw new Error("Run only in cloud CI with the temporary Playwright runtime.");
const { chromium } = require(path.resolve(process.env.ARTEX_PLAYWRIGHT_DIR));
const output = path.resolve(__dirname, "..", "out");
const requests = [], unexpected = new Set(), external = new Set(), allHeld = [];
const pendingMeta = [], pendingTests = [], pendingSaves = [], pendingDeletes = [];
const globalSettings = { enabled: false, base: "https://saved.example.invalid", digest: 30 };
let failMeta = true, holdMeta = false;
const channel = { id: 7, name: "UI_FIXTURE_CHANNEL", kind: "webhook", enabled: true, mode: "realtime", config: {}, filter: {}, rate_per_min: 0, secret_keys: ["url"], created_at: "2026-10-30T00:00:00Z", updated_at: "2026-10-30T00:00:00Z" };
const tool = (key) => ({ key, description: "Synthetic UI fixture", system: false, kind: "command", enabled: true, deferred: false, agents: [], schema: { type: "object", properties: {} }, exec: { command: `UI_FIXTURE_${key}_NOT_EXECUTED`, timeout_ms: 2000 }, calls: 0, usage_available: true });
const tools = [tool("fixture_a"), tool("fixture_b")];
function json(response, code, value) {
  if (response.destroyed || response.writableEnded) return;
  response.writeHead(code, { "Content-Type": "application/json", "Cache-Control": "no-store" });
  response.end(JSON.stringify(value));
}
function held(queue, respond, data) {
  const record = { released: false, data, release(value) { if (!this.released) { this.released = true; respond(value); } } };
  queue.push(record); allHeld.push(record); return record;
}
function meta() {
  return { enabled: globalSettings.enabled, public_base_url: globalSettings.base, digest_interval_min: String(globalSettings.digest), defaults: { digest_interval_min: 30 }, kinds: [{ kind: "webhook", default_rate_per_min: 0, secret_keys: ["url"] }], stats: { channels: 1, channels_on: channel.enabled ? 1 : 0, pending: 0, failed: 0, sent_today: 0, backlog_age_ms: 0 } };
}
async function serveAPI(request, response, url) {
  const endpoint = url.pathname, method = request.method || "GET";
  let body;
  if (!["GET", "HEAD"].includes(method)) {
    let raw = ""; for await (const chunk of request) { raw += chunk; assert.ok(raw.length < 1024 * 1024); }
    body = raw ? JSON.parse(raw) : {};
  }
  requests.push({ endpoint, method, body });
  if (endpoint === "/api/health") return json(response, 200, { ok: true, version: "fixture" });
  if (endpoint === "/api/auth/status") return json(response, 200, { initialized: true });
  if (endpoint === "/api/update/check") return json(response, 200, { current: "fixture", has_update: false });
  if (endpoint === "/api/intercept/pending") return json(response, 200, { pending: [] });
  if (endpoint === "/api/tasks") return json(response, 200, { tasks: [], active: "" });
  if (endpoint === "/api/agents") return json(response, 200, { agents: [] });
  if (endpoint === "/api/skills/missing") return json(response, 200, { missing: [] });
  if (endpoint === "/api/settings" && method === "GET") return json(response, 200, { traffic_capture: false, web_search_enabled: false, show_egress_ip: false, global_proxy: "" });
  if (endpoint === "/api/settings" && method === "PUT") {
    if ("notify_enabled" in body) globalSettings.enabled = body.notify_enabled;
    if ("notify_public_base_url" in body) globalSettings.base = body.notify_public_base_url;
    if ("notify_digest_interval_min" in body) globalSettings.digest = body.notify_digest_interval_min;
    return json(response, 200, { notify_enabled: globalSettings.enabled, notify_public_base_url: globalSettings.base, notify_digest_interval_min: globalSettings.digest });
  }
  if (endpoint === "/api/notify/meta") {
    if (failMeta) return json(response, 503, { error: "UI_FIXTURE_META_UNAVAILABLE" });
    const snapshot = meta();
    if (holdMeta) { held(pendingMeta, () => json(response, 200, snapshot), snapshot); return; }
    return json(response, 200, snapshot);
  }
  if (endpoint === "/api/notify/channels" && method === "GET") return json(response, 200, { channels: [channel] });
  if (endpoint === "/api/notify/channels/7" && method === "PATCH") { Object.assign(channel, body); return json(response, 200, { id: 7 }); }
  if (endpoint === "/api/tools" && method === "GET") return json(response, 200, { tools });
  if (endpoint === "/api/tools/custom/test" && method === "POST") {
    held(pendingTests, (value) => json(response, 200, value || { output: "UI_FIXTURE_TEST_RESULT", is_error: false }), body); return;
  }
  const custom = endpoint.match(/^\/api\/tools\/custom\/(fixture_a|fixture_b)$/);
  if (custom && method === "PUT") {
    held(pendingSaves, () => { const found = tools.find((t) => t.key === custom[1]); if (found) Object.assign(found, body); json(response, 200, { ok: true }); }, body); return;
  }
  if (custom && method === "DELETE") {
    held(pendingDeletes, () => { const index = tools.findIndex((t) => t.key === custom[1]); if (index >= 0) tools.splice(index, 1); json(response, 200, { deleted: custom[1] }); }, custom[1]); return;
  }
  unexpected.add(`${method} ${endpoint}`); return json(response, 404, { error: "Unmocked synthetic API" });
}
const mime = { ".html": "text/html", ".js": "text/javascript", ".css": "text/css", ".json": "application/json", ".svg": "image/svg+xml", ".png": "image/png", ".woff2": "font/woff2", ".ico": "image/x-icon", ".txt": "text/plain" };
const server = http.createServer(async (request, response) => {
  try {
    const url = new URL(request.url || "/", "http://127.0.0.1");
    if (url.pathname.startsWith("/api/")) return await serveAPI(request, response, url);
    let file = path.resolve(output, "." + decodeURIComponent(url.pathname));
    if (file !== output && !file.startsWith(output + path.sep)) { response.writeHead(403); response.end(); return; }
    if (fs.existsSync(file) && fs.statSync(file).isDirectory()) file = path.join(file, "index.html");
    if (!fs.existsSync(file) || !fs.statSync(file).isFile()) { response.writeHead(404); response.end(); return; }
    response.writeHead(200, { "Content-Type": mime[path.extname(file)] || "application/octet-stream", "Cache-Control": "no-store" });
    fs.createReadStream(file).pipe(response);
  } catch { json(response, 500, { error: "Synthetic fixture failed" }); }
});
async function eventually(check, label) {
  const deadline = Date.now() + 15000;
  while (Date.now() < deadline) { if (await check()) return; await new Promise((resolve) => setTimeout(resolve, 50)); }
  assert.fail(label);
}
const countWrites = () => requests.filter((r) => r.endpoint === "/api/settings" && r.method === "PUT").length;
const countToolLoads = () => requests.filter((r) => r.endpoint === "/api/tools" && r.method === "GET").length;
let browser, page;
async function main() {
  server.listen(0, "127.0.0.1"); await once(server, "listening");
  const origin = `http://127.0.0.1:${server.address().port}`;
  browser = await chromium.launch({ headless: true });
  const context = await browser.newContext({ viewport: { width: 1600, height: 1100 }, locale: "zh-CN", timezoneId: "Asia/Shanghai" });
  await context.route("**/*", async (route) => {
    const url = new URL(route.request().url());
    if (url.origin === origin || ["data:", "blob:"].includes(url.protocol)) return route.continue();
    external.add(url.origin); return route.abort("blockedbyclient");
  });
  const token = [Buffer.from('{"alg":"none"}').toString("base64url"), Buffer.from('{"sub":"fixture-admin","exp":4102444800}').toString("base64url"), "fixture"].join(".");
  await context.addInitScript(({ token }) => { localStorage.setItem("artex_token", token); document.cookie = "artex_session=1; path=/; SameSite=Lax"; document.cookie = "sidebar_state=false; path=/; SameSite=Lax"; }, { token });
  page = await context.newPage(); page.setDefaultTimeout(15000);
  const errors = []; page.on("pageerror", (error) => errors.push(error.message));
  await page.goto(origin + "/system/notify/", { waitUntil: "networkidle" });
  await page.getByText(/读取推送配置失败/).waitFor();
  const saveGlobal = page.getByRole("button", { name: "保存全局设置", exact: true });
  assert.equal(await saveGlobal.isDisabled(), true); assert.equal(countWrites(), 0);
  console.log("PASS unknown notification snapshot cannot clear saved global settings");
  failMeta = false;
  await page.getByRole("button", { name: "重新读取", exact: true }).click();
  await eventually(async () => !(await saveGlobal.isDisabled()), "Notification snapshot did not become editable");
  assert.equal(await page.locator("#n-base").inputValue(), globalSettings.base);
  await page.locator("#n-base").fill("https://draft.example.invalid");
  holdMeta = true;
  await page.getByRole("switch", { name: "启用", exact: true }).click();
  await eventually(() => pendingMeta.length === 1, "Missing first channel-triggered metadata refresh");
  globalSettings.enabled = true;
  await eventually(async () => !(await page.getByRole("switch", { name: "启用", exact: true }).isChecked()), "Channel patch did not refresh");
  await page.getByRole("switch", { name: "启用", exact: true }).click();
  await eventually(() => pendingMeta.length === 2, "Missing newer metadata refresh");
  pendingMeta[1].release();
  await eventually(async () => await page.getByRole("switch", { name: "推送总开关", exact: true }).isChecked(), "New snapshot did not apply");
  pendingMeta[0].release(); holdMeta = false;
  await eventually(async () => !(await saveGlobal.isDisabled()), "Metadata refresh remained pending");
  assert.equal(await page.locator("#n-base").inputValue(), "https://draft.example.invalid");
  assert.equal(await page.getByRole("switch", { name: "推送总开关", exact: true }).isChecked(), true);
  console.log("PASS dirty global draft and latest metadata survive out-of-order channel refreshes");
  await page.locator("#n-digest").fill("0"); await saveGlobal.click();
  await page.getByText(/汇总周期需为 1 到 1440/).waitFor(); assert.equal(countWrites(), 0);
  await page.locator("#n-digest").fill("25"); await saveGlobal.click();
  await eventually(() => countWrites() === 1, "Valid notification save missing");
  await eventually(async () => !(await saveGlobal.isDisabled()), "Notification save stayed busy");
  assert.deepEqual(requests.filter((r) => r.endpoint === "/api/settings" && r.method === "PUT")[0].body, { notify_public_base_url: "https://draft.example.invalid", notify_digest_interval_min: 25 });
  console.log("PASS invalid digest is rejected; valid global save contains only explicit settings");

  await page.goto(origin + "/system/tools/", { waitUntil: "networkidle" });
  await page.getByRole("tab", { name: "自定义工具", exact: true }).click();
  const editor = () => page.getByTestId("custom-tool-editor");
  const open = async (key) => { await page.getByTestId(`tool-card-${key}`).click(); await editor().getByText(`编辑 ${key}`, { exact: true }).waitFor(); };
  const close = async () => { await editor().getByRole("button", { name: "Close", exact: true }).click(); await editor().waitFor({ state: "hidden" }); };
  await open("fixture_a");
  await editor().getByRole("button", { name: "测试运行", exact: true }).click();
  await eventually(() => pendingTests.length === 1, "Synthetic test request missing");
  await editor().getByRole("textbox", { name: "自定义工具命令模板", exact: true }).fill("UI_FIXTURE_CHANGED_NOT_EXECUTED");
  pendingTests[0].release({ output: "STALE_RESULT_SHOULD_NOT_APPEAR", is_error: false });
  await eventually(async () => !(await editor().getByRole("button", { name: "测试运行", exact: true }).isDisabled()), "Old test did not finish");
  assert.equal(await page.getByText("STALE_RESULT_SHOULD_NOT_APPEAR", { exact: true }).count(), 0);
  assert.equal(pendingTests.length, 1, "A changed draft caused automatic execution replay");
  await editor().getByRole("button", { name: "测试运行", exact: true }).click();
  await eventually(() => pendingTests.length === 2, "Explicit retest missing");
  pendingTests[1].release({ output: "CURRENT_SPEC_RESULT", is_error: false });
  await editor().getByTestId("custom-tool-test-result").filter({ hasText: "CURRENT_SPEC_RESULT" }).waitFor();
  console.log("PASS test results belong to the submitted spec and execution is never automatically replayed");
  await editor().getByRole("button", { name: "保存", exact: true }).click();
  await eventually(() => pendingSaves.length === 1, "Synthetic save missing");
  await editor().getByRole("textbox", { name: "自定义工具描述", exact: true }).fill("DRAFT_AFTER_SUBMISSION");
  const beforeDraftSaveLoad = countToolLoads(); pendingSaves[0].release();
  await eventually(() => countToolLoads() > beforeDraftSaveLoad, "Saved submission did not refresh the catalog");
  await editor().getByText("编辑 fixture_a", { exact: true }).waitFor();
  assert.equal(await editor().getByRole("textbox", { name: "自定义工具描述", exact: true }).inputValue(), "DRAFT_AFTER_SUBMISSION");
  console.log("PASS a save acknowledgement preserves edits made after submission");
  await eventually(async () => !(await editor().getByRole("button", { name: "保存", exact: true }).isDisabled()), "Save remained pending");
  await editor().getByRole("button", { name: "保存", exact: true }).click();
  await eventually(() => pendingSaves.length === 2, "Second explicit save missing");
  await close(); await open("fixture_b");
  const beforeSaveLoad = countToolLoads(); pendingSaves[1].release();
  await eventually(() => countToolLoads() > beforeSaveLoad, "Old save did not refresh the catalog");
  await editor().getByText("编辑 fixture_b", { exact: true }).waitFor();
  assert.equal(await editor().getByTestId("custom-tool-test-result").count(), 0);
  console.log("PASS old save cannot close or populate a newly opened editor");
  await close(); await open("fixture_a");
  await editor().getByRole("button", { name: "删除", exact: true }).click();
  await eventually(() => pendingDeletes.length === 1, "Synthetic delete missing");
  await close(); await open("fixture_b");
  const beforeDeleteLoad = countToolLoads(); pendingDeletes[0].release();
  await eventually(() => countToolLoads() > beforeDeleteLoad, "Old deletion did not refresh the catalog");
  await editor().getByText("编辑 fixture_b", { exact: true }).waitFor();
  assert.equal(pendingSaves.length, 2); assert.equal(pendingDeletes.length, 1); assert.equal(pendingTests.length, 2);
  assert.equal(requests.filter((r) => r.endpoint.endsWith("/test") && r.endpoint.startsWith("/api/notify/")).length, 0);
  assert.deepEqual(errors, []); assert.deepEqual([...unexpected], []); assert.deepEqual([...external], []);
  console.log("PASS old deletion preserves the new editor; no real notification, command or external request ran");
  await context.close();
}
main().catch(async (error) => {
  console.error(error);
  if (page) console.error("Synthetic UI diagnostics:", { route: page.url(), requests: requests.slice(-15), body: (await page.locator("body").innerText().catch(() => "")).slice(0, 6000) });
  process.exitCode = 1;
}).finally(async () => {
  for (const record of allHeld) record.release();
  if (browser) await browser.close();
  server.closeAllConnections(); await new Promise((resolve) => server.close(resolve));
});
