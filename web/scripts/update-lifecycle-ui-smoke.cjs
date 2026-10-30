#!/usr/bin/env node
/* Cloud-only update UI regression. The Go service, updater, downloads and
 * restarts are never executed: static pages use only synthetic APIs/SSE. */
"use strict";
const assert = require("node:assert/strict");
const fs = require("node:fs"), http = require("node:http"), path = require("node:path");
const { once } = require("node:events");
if (!process.env.CI || !process.env.ARTEX_PLAYWRIGHT_DIR) throw new Error("Run only in cloud CI with its temporary Playwright runtime.");
const { chromium } = require(path.resolve(process.env.ARTEX_PLAYWRIGHT_DIR));
const output = path.resolve(__dirname, "..", "out");
const settings = {
  roe_enforcement: "strict", llm_records_retention_days: 30, finding_verify_mode: "auto",
  finding_inline_merge_enabled: false, credential_agent_enabled: false, credential_traffic_binding: false,
  task_intelligence_enabled: false, task_auto_title_enabled: false, chat_auto_title_enabled: false,
  traffic_capture: false, browser_screenshot_enabled: false, browser_screenshot_timestamp_enabled: false,
  opsec_enabled: false, agent_traffic_binding: false, llm_record: false,
  web_search_enabled: false, web_search_backend: "ddgs", web_search_proxy: "",
  brave_key_set: false, tavily_key_set: false, global_proxy: "", show_egress_ip: false,
  python_interpreter: "", workers: 3, worker_pools: [], callback_addr: "", callback_addr_source: "unset", callback_addr_locked: false,
  task_concurrency_enabled: false, task_concurrency_limit: 5, llm_pool_enabled: false, llm_pool_bind_fallback: false,
  constraints_inject_planner: true, constraints_inject_worker: true, noa_compaction: false,
  notify_enabled: true, notify_public_base_url: "", notify_digest_interval_min: 30,
};
const requests = [], unexpected = new Set(), external = new Set(), errors = [], scenarios = [];
let state;
function scenario(name) {
  const value = { name, healthVersion: "2.4.6", target: "v2.4.7", proxy: "", streams: [], apply: [], checks: [], heldHealth: [], holdFirstApply: false, holdForcedCheck: false, holdHealth: false, abortApply: false, applyAttempts: 0, rawHealth: 0, documents: 0, rollbackRequests: 0 };
  scenarios.push(value); state = value; return value;
}
function json(response, code, value) {
  if (response.destroyed || response.writableEnded) return;
  response.writeHead(code, { "Content-Type": "application/json", "Cache-Control": "no-store" }); response.end(JSON.stringify(value));
}
function checkInfo(s) {
  return { current: s.healthVersion, latest: s.target, mode: "binary", os: "darwin", arch: "arm64", repo: "synthetic", has_backup: true, comparable: true, has_update: s.healthVersion.replace(/^v/, "") !== s.target.replace(/^v/, ""), asset_available: true, asset: "UI_FIXTURE_NOT_DOWNLOADED.zip", size: 100 };
}
async function serveAPI(request, response, url) {
  const s = state, endpoint = url.pathname, method = request.method || "GET";
  let body = {};
  if (!["GET", "HEAD"].includes(method)) {
    let raw = ""; for await (const chunk of request) { raw += chunk; assert.ok(raw.length < 8192); }
    if (raw) body = JSON.parse(raw);
  }
  requests.push({ scenario: s.name, endpoint, method });
  if (endpoint === "/api/health") {
    if (!request.headers.authorization) {
      s.rawHealth++;
      if (s.holdHealth) {
        const pending = { response, aborted: false, release: () => json(response, 200, { ok: true, version: s.healthVersion }) };
        response.once("close", () => { pending.aborted = !response.writableEnded; });
        s.heldHealth.push(pending); return;
      }
    }
    return json(response, 200, { ok: true, service: "synthetic", version: s.healthVersion });
  }
  if (endpoint === "/api/update/settings") {
    if (method === "PUT") s.proxy = body.proxy;
    return json(response, 200, { proxy: s.proxy });
  }
  if (endpoint === "/api/update/check") {
    const snapshot = checkInfo(s);
    if (s.holdForcedCheck && url.searchParams.get("force") === "1") {
      s.checks.push({ response, release: () => json(response, 200, snapshot) }); return;
    }
    return json(response, 200, snapshot);
  }
  if (endpoint === "/api/sse-ticket") return json(response, 200, { ticket: `fixture-${s.name}-${s.streams.length + 1}` });
  if (endpoint === "/api/update/stream") {
    response.writeHead(200, { "Content-Type": "text/event-stream", "Cache-Control": "no-cache", Connection: "keep-alive" });
    response.write(": synthetic update stream; no updater is running\n\n");
    const stream = {
      response,
      emit(value) { if (!response.destroyed && !response.writableEnded) response.write(`data: ${JSON.stringify(value)}\n\n`); },
      close() { if (!response.destroyed && !response.writableEnded) response.end(); },
    };
    s.streams.push(stream); return;
  }
  if (endpoint === "/api/update/apply" && method === "POST") {
    const entry = { response, fail: () => json(response, 503, { error: "STALE_APPLY_ERROR_SHOULD_BE_IGNORED" }) };
    s.apply.push(entry);
    if (s.holdFirstApply && s.apply.length === 1) return;
    return json(response, 200, { ok: true });
  }
  if (endpoint === "/api/update/rollback" && method === "POST") { s.rollbackRequests++; return json(response, 200, { ok: true }); }
  if (endpoint === "/api/settings") return json(response, 200, settings);
  const control = endpoint.match(/^\/api\/settings\/runtime-controls\/(roe_enforcement|llm_records_retention_days|finding_verify_mode)$/);
  if (control && method === "GET") return json(response, 200, { [control[1]]: settings[control[1]] });
  if (endpoint === "/api/settings/basic-auth") return json(response, 200, { enabled: false, username: "", password_set: false });
  if (endpoint === "/api/auth/status") return json(response, 200, { initialized: true });
  if (endpoint === "/api/tasks") return json(response, 200, { tasks: [], active: "" });
  if (endpoint === "/api/agents") return json(response, 200, { agents: [] });
  if (endpoint === "/api/llm/profiles") return json(response, 200, { profiles: [] });
  if (endpoint === "/api/intercept/pending") return json(response, 200, { pending: [] });
  if (endpoint === "/api/skills/missing") return json(response, 200, { missing: [] });
  unexpected.add(`${method} ${endpoint}`); return json(response, 404, { error: "Unmocked synthetic update API" });
}
const mime = { ".html": "text/html", ".js": "text/javascript", ".css": "text/css", ".json": "application/json", ".svg": "image/svg+xml", ".png": "image/png", ".woff2": "font/woff2", ".ico": "image/x-icon", ".txt": "text/plain" };
const server = http.createServer(async (request, response) => {
  try {
    const url = new URL(request.url || "/", "http://127.0.0.1");
    if (url.pathname.startsWith("/api/")) return await serveAPI(request, response, url);
    if (request.headers["sec-fetch-dest"] === "document" && url.pathname === "/system/settings/") state.documents++;
    let file = path.resolve(output, "." + decodeURIComponent(url.pathname));
    if (file !== output && !file.startsWith(output + path.sep)) { response.writeHead(403); response.end(); return; }
    if (fs.existsSync(file) && fs.statSync(file).isDirectory()) file = path.join(file, "index.html");
    if (!fs.existsSync(file) || !fs.statSync(file).isFile()) { response.writeHead(404); response.end("Fixture missing"); return; }
    response.writeHead(200, { "Content-Type": mime[path.extname(file)] || "application/octet-stream", "Cache-Control": "no-store" });
    fs.createReadStream(file).pipe(response);
  } catch (error) { unexpected.add(`fixture error: ${error.message}`); json(response, 500, { error: "Synthetic fixture failed" }); }
});
async function eventually(check, message) {
  const deadline = Date.now() + 15000;
  while (Date.now() < deadline) { if (await check()) return; await new Promise((resolve) => setTimeout(resolve, 40)); }
  assert.fail(message);
}
let browser, page;
async function main() {
  server.listen(0, "127.0.0.1"); await once(server, "listening");
  const origin = `http://127.0.0.1:${server.address().port}`;
  browser = await chromium.launch({ headless: true });
  const context = await browser.newContext({ viewport: { width: 1500, height: 1100 }, locale: "zh-CN", timezoneId: "Asia/Shanghai" });
  await context.route("**/*", async (route) => {
    const url = new URL(route.request().url());
    if (url.origin !== origin && !["data:", "blob:"].includes(url.protocol)) { external.add(url.origin); return route.abort("blockedbyclient"); }
    if (url.origin === origin && url.pathname === "/api/update/apply" && state.abortApply) return route.abort("failed");
    return route.continue();
  });
  const token = [Buffer.from('{"alg":"none"}').toString("base64url"), Buffer.from('{"sub":"fixture-admin","exp":4102444800}').toString("base64url"), "fixture"].join(".");
  await context.addInitScript(({ token }) => { localStorage.setItem("artex_token", token); document.cookie = "artex_session=1; path=/; SameSite=Lax"; document.cookie = "sidebar_state=false; path=/; SameSite=Lax"; }, { token });
  async function open(s) {
    page = await context.newPage(); page.setDefaultTimeout(15000);
    page.on("pageerror", (error) => errors.push(error.message));
    page.on("request", (request) => { if (request.method() === "POST" && new URL(request.url()).pathname === "/api/update/apply") s.applyAttempts++; });
    page.on("dialog", (dialog) => dialog.accept());
    await page.goto(origin + "/system/settings/", { waitUntil: "networkidle" });
    await page.getByRole("textbox", { name: "搜索系统设置", exact: true }).fill("更新");
    await eventually(async () => !(await page.getByRole("button", { name: `更新到 ${s.target}`, exact: true }).isDisabled()), "Synthetic update card not ready");
  }
  const search = () => page.getByRole("textbox", { name: "搜索系统设置", exact: true });
  const update = () => page.getByRole("button", { name: `更新到 ${state.target}`, exact: true });
  const noFalseSuccess = async () => assert.equal(await page.getByText(/已更新到 .*正在重新加载页面/).count(), 0);
  const start = async (s) => { const count = s.streams.length; await update().click(); await eventually(() => s.streams.length > count && s.applyAttempts > 0, "Synthetic update was not requested"); return s.streams.at(-1); };

  const oldCheck = scenario("old-check"); await open(oldCheck);
  oldCheck.holdForcedCheck = true;
  await page.getByRole("button", { name: "检查更新", exact: true }).click();
  await eventually(() => oldCheck.checks.length === 1, "Manual old check was not held");
  await search().fill("roe_enforcement");
  await page.getByRole("button", { name: "更新到 v2.4.7", exact: true }).waitFor({ state: "hidden" });
  oldCheck.target = "v2.4.8"; oldCheck.holdForcedCheck = false;
  await search().fill("更新");
  await page.getByRole("button", { name: "更新到 v2.4.8", exact: true }).waitFor();
  oldCheck.checks[0].release();
  await page.waitForTimeout(150);
  await page.getByRole("button", { name: "更新到 v2.4.8", exact: true }).waitFor();
  assert.equal(await page.getByText("发现新版本 v2.4.7", { exact: true }).count(), 0);
  assert.equal(oldCheck.applyAttempts, 0); await page.close();
  console.log("PASS stale check from an unmounted card cannot replace or toast over the new check");

  const wrong = scenario("unexpected-target"); await open(wrong);
  const wrongStream = await start(wrong); wrong.healthVersion = "2.4.8";
  wrongStream.emit({ phase: "staged", percent: -1, message: "Synthetic staged" });
  await page.getByText(/与请求目标 .* 不一致，未判定更新成功/).first().waitFor();
  await noFalseSuccess(); assert.equal(wrong.documents, 1); assert.equal(wrong.applyAttempts, 1);
  await page.waitForTimeout(1000); assert.equal(wrong.documents, 1); await page.close();
  console.log("PASS an unrelated healthy version is not update success and does not auto-reload");

  const success = scenario("expected-target"); await open(success);
  const successStream = await start(success); success.healthVersion = "2.4.7";
  successStream.emit({ phase: "staged", percent: -1, message: "Synthetic staged" });
  await eventually(() => success.documents === 2, "Expected target was not confirmed by a page reload");
  await page.waitForLoadState("networkidle"); assert.equal(success.applyAttempts, 1); await page.close();
  console.log("PASS only the requested target confirms update and reloads once");

  const rollback = scenario("rollback-observation"); await open(rollback);
  await page.getByRole("button", { name: "回滚到上一版本", exact: true }).click();
  rollback.healthVersion = "2.4.5";
  await page.getByText(/尚未核实是否为预期回滚目标/).first().waitFor();
  await noFalseSuccess(); assert.equal(rollback.documents, 1); assert.equal(rollback.rollbackRequests, 1); assert.equal(rollback.applyAttempts, 0); await page.close();
  console.log("PASS a changed rollback version remains explicitly unverified, never an update-success notice");

  const late = scenario("old-apply-error"); late.holdFirstApply = true; await open(late);
  const first = await start(late); await eventually(() => late.apply.length === 1, "First apply not held");
  first.emit({ phase: "failed", percent: -1, message: "UI_FIXTURE_FIRST_FAILED", error: "UI_FIXTURE_FIRST_FAILED" });
  await eventually(async () => !(await update().isDisabled()), "Synthetic failure did not unlock a second operation");
  const second = await start(late);
  await eventually(() => late.apply.length === 2, "Second explicit apply missing");
  second.emit({ phase: "staged", percent: -1, message: "Synthetic second staged" });
  await page.getByText("正在重启并应用新版本，请稍候（页面会自动刷新）…", { exact: true }).waitFor();
  late.apply[0].fail(); await page.waitForTimeout(200);
  assert.equal(await page.getByText(/STALE_APPLY_ERROR_SHOULD_BE_IGNORED/).count(), 0);
  assert.equal(await update().isDisabled(), true);
  await page.getByText("正在重启并应用新版本，请稍候（页面会自动刷新）…", { exact: true }).waitFor();
  late.healthVersion = "2.4.7";
  await eventually(() => late.documents === 2, "Late old failure cancelled the current expected-target polling");
  assert.equal(late.applyAttempts, 2); await page.close();
  console.log("PASS a late failure from an older apply cannot cancel the newer operation");

  const interrupted = scenario("transport-disconnect"); interrupted.abortApply = true; await open(interrupted);
  const interruptedStream = await start(interrupted); interruptedStream.close();
  await page.getByText(/连接中断，结果未确认/).first().waitFor();
  await eventually(() => interrupted.rawHealth >= 2, "Disconnect did not continue read-only version verification");
  assert.equal(interrupted.applyAttempts, 1); assert.equal(interrupted.streams.length, 1, "One-use SSE ticket was reconnected");
  interrupted.healthVersion = "2.4.7";
  await eventually(() => interrupted.documents === 2, "Read-only disconnect verification did not observe target");
  assert.equal(interrupted.applyAttempts, 1); await page.close();
  console.log("PASS disconnected apply/SSE verifies health without repeating POST or one-use ticket");

  const unmount = scenario("unmount-poll"); await open(unmount);
  const unmountStream = await start(unmount); unmount.holdHealth = true;
  unmountStream.emit({ phase: "staged", percent: -1, message: "Synthetic staged" });
  await eventually(() => unmount.heldHealth.length === 1, "Health poll was not held");
  await search().fill("roe_enforcement");
  await page.getByText("版本与更新", { exact: true }).waitFor({ state: "hidden" });
  await eventually(() => unmount.heldHealth[0].aborted, "Unmount did not abort the in-flight health fetch");
  unmount.healthVersion = "2.4.7"; unmount.heldHealth[0].release();
  await page.waitForTimeout(4500);
  assert.equal(unmount.rawHealth, 1, "Unmounted card kept polling health");
  assert.equal(unmount.documents, 1, "Late health result reloaded an unmounted update card");
  assert.equal(unmount.applyAttempts, 1); await noFalseSuccess(); await page.close();
  console.log("PASS unmount aborts in-flight verification and prevents later polls/reloads");
  assert.deepEqual(errors, []); assert.deepEqual([...unexpected], []); assert.deepEqual([...external], []);
  await context.close();
}
main().catch(async (error) => {
  console.error(error);
  if (page && !page.isClosed()) console.error("Synthetic update diagnostics:", { scenario: state.name, documents: state.documents, apply: state.applyAttempts, health: state.rawHealth, recent: requests.slice(-20), body: (await page.locator("body").innerText().catch(() => "")).slice(0, 6500) });
  process.exitCode = 1;
}).finally(async () => {
  if (browser) await browser.close();
  for (const s of scenarios) { for (const stream of s.streams) stream.close(); for (const item of [...s.apply, ...s.checks, ...s.heldHealth]) if (item.response && !item.response.writableEnded && !item.response.destroyed) item.response.end(); }
  server.closeAllConnections(); await new Promise((resolve) => server.close(resolve));
});
