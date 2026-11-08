#!/usr/bin/env node
/* Cloud-only upstream merge regression. All settings, tasks, proxy records and
 * probes are synthetic. No ARTEX process, production database, model, real
 * credential or external target is contacted. */
"use strict";
const assert = require("node:assert/strict");
const fs = require("node:fs"), http = require("node:http"), path = require("node:path");
const { once } = require("node:events");
if (!process.env.CI || !process.env.ARTEX_PLAYWRIGHT_DIR) throw new Error("Run only in GitHub Actions with its temporary browser runtime.");
const { chromium } = require(path.resolve(process.env.ARTEX_PLAYWRIGHT_DIR));
const output = path.resolve(__dirname, "..", "out");
for (const route of ["system/settings", "function/tasks"]) {
  if (!fs.existsSync(path.join(output, route, "index.html"))) throw new Error("Missing cloud static export");
}
const savedURL = "https://echo.fixture.invalid/saved-ip";
const nextURL = "http://10.23.45.67/fixture-ip";
const temporaryURL = "https://temporary.fixture.invalid/ip";
const proxyCount = 125;
const proxies = Array.from({ length: proxyCount }, (_, i) => ({
  id: `upstream-proxy-${i}`, url: `http://proxy-${i}.fixture.invalid:8080`,
  name: `FIXTURE_PROXY_${i}`, enabled: true, health: "unknown", consecutive_failures: 0,
  protocol_auto: i === 1,
}));
const settings = {
  roe_enforcement: "strict", finding_inline_merge_enabled: false,
  llm_records_retention_days: 30, finding_verify_mode: "auto",
  credential_agent_enabled: false, credential_traffic_binding: false,
  task_intelligence_enabled: false, task_auto_title_enabled: false, chat_auto_title_enabled: false,
  traffic_capture: false, browser_screenshot_enabled: false, browser_screenshot_timestamp_enabled: false,
  opsec_enabled: false, agent_traffic_binding: false, llm_record: false, deepseek_search_key_set: false,
  proxy_pool: proxies, proxy_pool_active_id: proxies[0].id, proxy_pool_switch_log: [],
  proxy_rotate_timed_enabled: false, proxy_rotate_interval_seconds: 30,
  proxy_rotate_on_failure: false, proxy_rotate_strategy: "sequential",
  proxy_health_fail_threshold: 3, proxy_health_cooldown_seconds: 60,
  proxy_health_check_interval_seconds: 30, proxy_reconnect_max_attempts: 5,
  proxy_reconnect_interval_seconds: 30, egress_probe_url: savedURL, proxy_health_check_url: "",
  web_search_enabled: false, web_search_backend: "ddgs", web_search_proxy: "",
  brave_key_set: false, tavily_key_set: false, global_proxy: proxies[0].url, show_egress_ip: false,
  python_interpreter: "", workers: 3, worker_pools: [],
  callback_addr: "", callback_addr_source: "unset", callback_addr_locked: false,
  task_concurrency_enabled: false, task_concurrency_limit: 5,
  llm_pool_enabled: false, llm_pool_bind_fallback: false,
  constraints_inject_planner: true, constraints_inject_worker: true, noa_compaction: false,
  notify_enabled: true, notify_public_base_url: "", notify_digest_interval_min: 30,
};
function task(id, name, share) {
  return {
    id: String(id), name, remark: "", description: "Synthetic task description", goal: "Synthetic task goal",
    status: "paused", paused: true, pause_reason: "manual", created_at: "2026-11-05T00:00:00Z", created_unix: 1793836800,
    source_task_ids: [], company_ids: [], llm_profile_ids: [], coverage_enabled: true, share_global_assets: share,
    goals_total: 0, goals_met: 0, in_flight: 0, run_seconds: 0,
    findings: { critical: 0, high: 0, medium: 0, low: 0 },
  };
}
const tasks = [task(201, "FIXTURE_ISOLATED_TASK", false), task(202, "FIXTURE_SHARED_TASK", true), task(203, "FIXTURE_UNKNOWN_POLICY_TASK", undefined)];
const state = { failProbeSave: false, ignoreProbeSave: false, omitProbeSave: false, failTaskSave: false, ignoreTaskSave: false, holdTaskSave: false };
const requests = [], unexpected = new Set(), external = new Set(), errors = [];
const held = [];
function json(response, code, value) {
  if (response.destroyed || response.writableEnded) return;
  response.writeHead(code, { "Content-Type": "application/json", "Cache-Control": "no-store" });
  response.end(JSON.stringify(value));
}
const probeReport = () => ({
  ok: true, capture: false, configured: settings.global_proxy, agent_proxy: settings.global_proxy,
  checks: ["direct", "global"].map((key) => ({
    key, label: key === "direct" ? "① 直连 IP（不经过代理）" : "② 经全局代理的出口 IP",
    result: { ok: true, exit_ip: key === "direct" ? "192.0.2.1" : "192.0.2.2", latency_ms: 1, probes: [] },
  })),
});
async function serveAPI(request, response, url) {
  const endpoint = url.pathname, method = request.method || "GET";
  const chunks = [];
  for await (const chunk of request) chunks.push(chunk);
  const raw = Buffer.concat(chunks).toString();
  const body = raw ? JSON.parse(raw) : {};
  requests.push({ endpoint, method, body });
  if (endpoint === "/api/settings/egress" && method === "GET") return json(response, 200, { show_egress_ip: false, global_proxy: settings.global_proxy });
  if (endpoint === "/api/settings" && method === "GET") return json(response, 200, settings);
  if (endpoint === "/api/settings/proxy-pool/progress" && method === "GET") {
    const ids = (url.searchParams.get("ids") || "").split(",").filter(Boolean);
    assert.ok(ids.length <= 50, "Upstream settings fetched progress beyond the visible page");
    return json(response, 200, { id: 0, state: "idle", total: 0, completed: 0, running: 0, succeeded: 0, failed: 0, concurrency: 12, items: {} });
  }
  const diagnostic = endpoint.match(/^\/api\/settings\/proxy-pool\/([^/]+)\/diagnose$/);
  if (diagnostic && method === "POST") {
    const entry = settings.proxy_pool.find((item) => item.id === diagnostic[1]);
    assert.ok(entry); assert.deepEqual(body, {});
    const scheme = "https:";
    const missing = { checked: false, ok: false, latency_ms: 0 };
    const observed = (protocol) => ({ checked: true, ok: protocol === "http", status_code: protocol === "http" ? 204 : undefined, error: protocol === "http" ? undefined : "协议请求失败", latency_ms: 1 });
    const checks = ["http", "https", "socks5", "socks4"].map((protocol) => ({ protocol, recognized: protocol === "http" && scheme === "https:", supported: protocol !== "socks4",
      evidence: "仅检查已保存回显服务", https: scheme === "https:" && protocol !== "socks4" ? observed(protocol) : missing,
      http: scheme === "http:" && protocol !== "socks4" ? observed(protocol) : missing }));
    return json(response, 200, { id: entry.id, probe_kind:"connectivity", probe_mode: settings.proxy_health_check_url ? "configured" : "default", probe_url: settings.proxy_health_check_url, http_skipped: true, health_updated: false, checked_at: "2026-11-08T00:00:00Z", scope: "fixture",
      https: { kind:"connectivity", ok: true, via: "http", status_code:204, latency_ms: 1, probes: [], ...(entry.protocol_auto ? { protocol_checks: checks } : {}) },
      http: { ok: false, error: "未请求额外公共回显", latency_ms: 0, probes: [] } });
  }
  if (endpoint === "/api/settings" && method === "PUT") {
    if (Object.hasOwn(body,"proxy_health_check_url")) {
      assert.deepEqual(Object.keys(body),["proxy_health_check_url"]);
      settings.proxy_health_check_url=body.proxy_health_check_url;
      return json(response,200,settings);
    }
    assert.deepEqual(Object.keys(body), ["egress_probe_url"], "Probe service save changed unrelated settings");
    assert.equal(typeof body.egress_probe_url, "string");
    if (state.failProbeSave) { state.failProbeSave = false; return json(response, 503, { error: "FIXTURE_PROBE_SAVE_FAILED" }); }
    if (state.ignoreProbeSave) { state.ignoreProbeSave = false; return json(response, 200, settings); }
    if (state.omitProbeSave) {
      state.omitProbeSave = false;
      const partial = { ...settings }; delete partial.egress_probe_url;
      return json(response, 200, partial);
    }
    settings.egress_probe_url = body.egress_probe_url;
    return json(response, 200, settings);
  }
  if ((endpoint === "/api/settings/proxy/test" || endpoint === "/api/settings/proxy/health-test") && method === "POST") {
    assert.equal(body.resume, undefined, "An IP service test silently resumed tasks");
    assert.equal(typeof body.probe_url, "string", "Temporary test lost explicit empty/default semantics");
    return json(response, 200, probeReport());
  }
  if (endpoint === "/api/tasks" && method === "GET") return json(response, 200, { tasks, active: "" });
  const metadata = endpoint.match(/^\/api\/tasks\/(\d+)$/);
  if (metadata && method === "PATCH") {
    const item = tasks.find((entry) => entry.id === metadata[1]);
    assert.ok(item, "Task sharing write used an unknown task");
    assert.deepEqual(Object.keys(body), ["share_global_assets"], "Task sharing write changed unrelated metadata");
    assert.equal(typeof body.share_global_assets, "boolean");
    if (state.failTaskSave) { state.failTaskSave = false; return json(response, 503, { error: "FIXTURE_TASK_SAVE_FAILED" }); }
    if (state.ignoreTaskSave) { state.ignoreTaskSave = false; return json(response, 200, item); }
    const commit = () => { item.share_global_assets = body.share_global_assets; json(response, 200, item); };
    if (state.holdTaskSave) { state.holdTaskSave = false; held.push(commit); return; }
    return commit();
  }
  const control = endpoint.match(/^\/api\/settings\/runtime-controls\/(roe_enforcement|finding_verify_mode|llm_records_retention_days)$/);
  if (control && method === "GET") return json(response, 200, { [control[1]]: settings[control[1]] });
  if (endpoint === "/api/settings/basic-auth") return json(response, 200, { enabled: false, username: "", password_set: false });
  if (endpoint === "/api/update/settings") return json(response, 200, { proxy: "" });
  if (endpoint === "/api/update/check") return json(response, 200, { current: "fixture", mode: "binary", os: "darwin", arch: "arm64", repo: "fixture", has_backup: false, has_update: false, comparable: false });
  if (endpoint === "/api/version" || endpoint === "/api/health") return json(response, 200, { ok: true, service: "fixture", version: "fixture" });
  if (endpoint === "/api/auth/status") return json(response, 200, { initialized: true });
  if (endpoint === "/api/task-categories") return json(response, 200, { categories: [] });
  if (endpoint === "/api/task-templates") return json(response, 200, { templates: [] });
  if (endpoint === "/api/task-archives") return json(response, 200, { archives: [], total: 0, page: 1, size: 20 });
  if (endpoint === "/api/companies") return json(response, 200, []);
  if (endpoint === "/api/agents") return json(response, 200, { agents: [] });
  if (endpoint === "/api/llm/profiles") return json(response, 200, { profiles: [] });
  if (endpoint === "/api/intercept/pending") return json(response, 200, { pending: [] });
  if (endpoint === "/api/skills/missing") return json(response, 200, { missing: [] });
  if (endpoint === "/api/schedules") return json(response, 200, { schedules: [] });
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
  } catch (error) {
    unexpected.add(`fixture error: ${error.message}`);
    json(response, 500, { error: "Synthetic fixture failed" });
  }
});
const writes = () => requests.filter((r) => ["PATCH", "POST", "PUT", "DELETE"].includes(r.method));
const probeWrites = () => requests.filter((r) => r.endpoint === "/api/settings" && r.method === "PUT");
const taskWrites = () => requests.filter((r) => /^\/api\/tasks\/\d+$/.test(r.endpoint) && r.method === "PATCH");
const probes = () => requests.filter((r) => ["/api/settings/proxy/test","/api/settings/proxy/health-test"].includes(r.endpoint));
async function eventually(check, label) {
  const deadline = Date.now() + 15000;
  while (Date.now() < deadline) { if (await check()) return; await new Promise((resolve) => setTimeout(resolve, 40)); }
  assert.fail(label);
}
let browser, page;
async function main() {
  server.listen(0, "127.0.0.1"); await once(server, "listening");
  const origin = `http://127.0.0.1:${server.address().port}`;
  browser = await chromium.launch({ headless: true });
  const context = await browser.newContext({ viewport: { width: 1550, height: 1100 }, locale: "zh-CN", timezoneId: "Asia/Shanghai" });
  await context.route("**/*", (route) => {
    const url = new URL(route.request().url());
    if (url.origin === origin || ["data:", "blob:"].includes(url.protocol)) return route.continue();
    external.add(url.origin); return route.abort("blockedbyclient");
  });
  const token = [Buffer.from('{"alg":"none"}').toString("base64url"), Buffer.from('{"sub":"fixture-admin","exp":4102444800}').toString("base64url"), "fixture"].join(".");
  await context.addInitScript(({ token }) => {
    localStorage.setItem("artex_token", token);
    document.cookie = "artex_session=1; path=/; SameSite=Lax";
    document.cookie = "sidebar_state=false; path=/; SameSite=Lax";
  }, { token });
  page = await context.newPage(); page.setDefaultTimeout(15000);
  page.on("pageerror", (error) => errors.push(error.message));
  const gotoProbe = async () => {
    await page.goto(origin + "/system/settings/", { waitUntil: "networkidle" });
    await page.getByRole("button", { name: /^流量与安全/ }).click();
    await page.locator("summary").filter({ hasText: "自定义 IP 探测服务（可选）" }).click();
    await page.locator("#egress-probe-url").waitFor();
    await eventually(async () => await page.locator("#egress-probe-url").isEnabled(), "Probe settings remained read-only after load");
  };
  const input = () => page.getByRole("textbox", { name: "IP 探测服务地址", exact: true });
  const save = () => page.getByRole("button", { name: "保存探测服务", exact: true });
  try {
    await gotoProbe();
    assert.equal(await input().inputValue(), savedURL);
    assert.equal(writes().length, 0, "Opening settings mutated state or probed an external endpoint");
    await eventually(async () => await page.getByTestId("proxy-pool-row").count() === 50, "Merged probe settings lost fixed proxy pagination");
    const initialProxies = JSON.stringify(settings.proxy_pool);
    await input().fill(nextURL);
    assert.equal(probeWrites().length, 0, "Editing a probe draft wrote it without explicit save");
    await save().click();
    await eventually(() => settings.egress_probe_url === nextURL, "Probe URL was not saved");
    await eventually(async () => await save().isEnabled(), "Probe save remained busy");
    assert.deepEqual(probeWrites().at(-1).body, { egress_probe_url: nextURL });
    assert.equal(probes().length, 0, "Saving while egress banner was disabled silently probed");
    await gotoProbe(); assert.equal(await input().inputValue(), nextURL);
    console.log("PASS saved custom egress probe URL loads, saves and survives reload without changing proxy state");

    const diagnosisDialog = () => page.getByRole("dialog", { name: "代理 HTTP／HTTPS 诊断", exact: true });
    const explicitRow = page.getByTestId("proxy-pool-row").filter({ hasText: "FIXTURE_PROXY_0 ·" });
    await explicitRow.getByRole("button", { name: "诊断", exact: true }).click();
    await diagnosisDialog().getByText("HTTPS 连通性（校验证书）：通过", { exact: true }).waitFor();
    await diagnosisDialog().getByText("仅检查 HTTPS 连通性，未查询出口 IP。", { exact: true }).waitFor();
    assert.equal(await diagnosisDialog().getByText("访问 HTTPS 网站（校验证书）：回显成功", { exact: true }).count(), 0);
    await diagnosisDialog().getByRole("button", { name: "关闭诊断", exact: true }).click();
    const automaticRow = page.getByTestId("proxy-pool-row").filter({ hasText: "FIXTURE_PROXY_1 ·" });
    await automaticRow.getByRole("button", { name: "诊断", exact: true }).click();
    await diagnosisDialog().getByTestId("proxy-protocol-checks").waitFor();
    assert.equal(await diagnosisDialog().getByText("访问 HTTP 网站（明文回显）：未执行", { exact: true }).count(), 3);
    await diagnosisDialog().getByText("访问 HTTPS 网站（校验证书）：连通性通过 · HTTP 204", { exact: true }).waitFor();
    await diagnosisDialog().getByRole("button", { name: "关闭诊断", exact: true }).click();
    assert.equal(JSON.stringify(settings.proxy_pool), initialProxies, "Configured diagnostics published protocol/health state");
    console.log("PASS configured diagnostics distinguish website schemes for explicit and automatic proxy protocols");

    const healthInput = () => page.getByRole("textbox", { name: "连通性检测网址", exact: true });
    await healthInput().fill(temporaryURL);
    const beforeTemporary = probeWrites().length;
    await page.getByRole("button", { name: "测试代理", exact: true }).click();
    await eventually(() => probes().length === 1, "Temporary health check missing");
    await eventually(async () => await page.getByRole("button", { name: "测试代理", exact: true }).isEnabled(), "Health test remained busy");
    assert.equal(probes()[0].endpoint, "/api/settings/proxy/health-test");
    assert.deepEqual(probes()[0].body, {global_proxy:settings.global_proxy,probe_url:temporaryURL});
    assert.equal(settings.egress_probe_url,nextURL); assert.equal(probeWrites().length,beforeTemporary);
    await page.getByRole("button",{name:"保存检测网址",exact:true}).click();
    await eventually(()=>settings.proxy_health_check_url===temporaryURL,"Health setting was not saved");
    await eventually(async()=>await page.getByRole("button",{name:"保存检测网址",exact:true}).isEnabled(),"Health save remained busy");
    await healthInput().fill("");
    await page.getByRole("button", { name: "测试代理", exact: true }).click();
    await eventually(()=>probes().length===2,"Default health request missing");
    await eventually(async()=>await page.getByRole("button",{name:"测试代理",exact:true}).isEnabled(),"Health request remained busy");
    assert.equal(probes()[1].body.probe_url,"");
    assert.equal(settings.egress_probe_url,nextURL);
    await input().fill(temporaryURL);
    await page.getByRole("button",{name:"查询出口 IP",exact:true}).click();
    await eventually(()=>probes().length===3,"Explicit IP query missing");
    await eventually(async()=>await page.getByRole("button",{name:"查询出口 IP",exact:true}).isEnabled(),"IP query remained busy");
    assert.equal(probes()[2].endpoint,"/api/settings/proxy/test");
    assert.equal(settings.proxy_health_check_url,temporaryURL);
    console.log("PASS independent health and optional IP query settings, requests and defaults");

    state.failProbeSave = true;
    await input().fill(temporaryURL); await save().click();
    await eventually(async () => await save().isEnabled(), "Failed probe save remained busy");
    await page.locator("[data-sonner-toast]").filter({ hasText: "FIXTURE_PROBE_SAVE_FAILED" }).first().waitFor();
    assert.equal(settings.egress_probe_url, nextURL); assert.equal(await input().inputValue(), temporaryURL);
    state.ignoreProbeSave = true; await save().click();
    await eventually(async () => await save().isEnabled(), "Ignored probe save remained busy");
    assert.equal(settings.egress_probe_url, nextURL); assert.equal(await input().inputValue(), temporaryURL, "Ignored response discarded unsaved probe draft");
    state.omitProbeSave = true; await save().click();
    await eventually(async () => await save().isEnabled(), "Unknown probe response remained busy");
    assert.equal(settings.egress_probe_url, nextURL); assert.equal(await input().inputValue(), temporaryURL, "Omitted probe field was mistaken for clearing it");
    await input().fill(""); await save().click();
    await eventually(() => settings.egress_probe_url === "", "Explicit clear did not restore default service");
    await gotoProbe(); assert.equal(await input().inputValue(), "");
    assert.equal(JSON.stringify(settings.proxy_pool), initialProxies); assert.equal(settings.global_proxy, proxies[0].url);
    assert.equal(settings.opsec_enabled, false); assert.equal(settings.roe_enforcement, "strict");
    console.log("PASS failed, ignored and partial save responses preserve draft; explicit clear is durable and isolated");

    await page.goto(origin + "/function/tasks/", { waitUntil: "networkidle" });
    const isolatedRow = () => page.getByRole("row").filter({ hasText: "FIXTURE_ISOLATED_TASK" });
    const sharingButton = (enabled) => isolatedRow().getByRole("button", { name: enabled ? "共享全局资产：已开启，点击关闭" : "共享全局资产：未开启，点击开启", exact: true });
    await sharingButton(false).waitFor();
    await page.getByRole("row").filter({ hasText: "FIXTURE_SHARED_TASK" }).getByRole("button", { name: "共享全局资产：已开启，点击关闭", exact: true }).waitFor();
    assert.equal(taskWrites().length, 0, "Loading tasks changed their asset policy");
    const unknown = page.getByRole("row").filter({ hasText: "FIXTURE_UNKNOWN_POLICY_TASK" }).getByRole("button", { name: "共享全局资产：状态未知，请刷新", exact: true });
    await unknown.waitFor(); assert.equal(await unknown.isDisabled(), true, "Missing task policy became a writable false default");
    state.holdTaskSave = true;
    await sharingButton(false).click();
    await eventually(() => held.length === 1, "Task sharing write was not submitted");
    assert.equal(tasks[0].share_global_assets, false, "Task policy changed before the acknowledged response");
    assert.equal(await sharingButton(false).isDisabled(), true, "In-flight sharing button permits duplicate submission");
    held.shift()(); await sharingButton(true).waitFor();
    assert.deepEqual(taskWrites().at(-1).body, { share_global_assets: true });
    assert.equal(tasks[0].status, "paused"); assert.equal(tasks[0].paused, true);
    console.log("PASS task asset sharing is opt-in, explicit, identity-bound and acknowledges without resuming tasks");

    state.failTaskSave = true;
    await sharingButton(true).click();
    await page.locator("[data-sonner-toast]").filter({ hasText: "FIXTURE_TASK_SAVE_FAILED" }).first().waitFor();
    await eventually(async () => await sharingButton(true).isEnabled(), "Failed task sharing save remained busy");
    assert.equal(tasks[0].share_global_assets, true);
    state.ignoreTaskSave = true; await sharingButton(true).click();
    await eventually(async () => await sharingButton(true).isEnabled(), "Ignored task sharing response replaced known policy");
    assert.equal(tasks[0].share_global_assets, true);
    await sharingButton(true).click(); await sharingButton(false).waitFor();
    assert.equal(tasks[0].share_global_assets, false); assert.equal(tasks[1].share_global_assets, true);
    await page.reload({ waitUntil: "networkidle" }); await sharingButton(false).waitFor();
    console.log("PASS failed/ignored sharing writes preserve known policy and explicit disable persists across reload");

    await page.getByRole("button", { name: "新建任务", exact: true }).click();
    const sharingCheckbox = page.locator("#share-global-assets");
    await sharingCheckbox.waitFor(); assert.equal(await sharingCheckbox.getAttribute("aria-checked"), "false");
    assert.equal(requests.filter((r) => r.endpoint === "/api/tasks" && r.method === "POST").length, 0, "Inspecting new-task defaults launched a task");
    await page.getByRole("dialog").getByRole("button", { name: "取消", exact: true }).click();
    assert.equal(requests.filter((r) => /\/control|\/resume|\/pause|\/archive|\/delete/.test(r.endpoint) && r.method !== "GET").length, 0);
    assert.deepEqual(errors, [], "Unexpected browser exceptions");
    assert.deepEqual([...unexpected], [], "Unexpected API requests");
    assert.deepEqual([...external], [], "External requests attempted");
    console.log("PASS new tasks default to isolated assets; no synthetic test executes tasks or contacts external services");
  } catch (error) {
    console.error("Upstream merge UI error:", error.message.split("\n")[0]);
    const body = await page.locator("body").innerText({ timeout: 1000 }).catch(() => "[body unavailable]");
    console.error(JSON.stringify({ route: page.url(), recent: requests.slice(-15).map(({ endpoint, method }) => ({ endpoint, method })), unexpected: [...unexpected], body: body.slice(0, 5000) }, null, 2));
    throw error;
  } finally { await context.close(); }
}
main().catch((error) => { console.error(error.message.split("\n")[0]); process.exitCode = 1; }).finally(async () => {
  for (const release of held) release();
  if (browser) await browser.close();
  server.closeAllConnections();
  if (server.listening) await new Promise((resolve) => server.close(resolve));
});
