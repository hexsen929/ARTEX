#!/usr/bin/env node
/* Cloud-only proxy scale regression. All proxies, imports, probes and writes
 * are synthetic and served by this local fixture. No ARTEX service, database,
 * model, real credential or external target is contacted. */
"use strict";

const assert = require("node:assert/strict");
const fs = require("node:fs");
const http = require("node:http");
const path = require("node:path");
const { once } = require("node:events");

if (!process.env.CI || !process.env.ARTEX_PLAYWRIGHT_DIR) {
  throw new Error("Run only in GitHub Actions with its temporary Playwright runtime.");
}
const { chromium } = require(path.resolve(process.env.ARTEX_PLAYWRIGHT_DIR));
const output = path.resolve(__dirname, "..", "out");
if (!fs.existsSync(path.join(output, "system", "settings", "index.html"))) {
  throw new Error("Missing cloud static export");
}

const initialCount = 10001;
const proxyID = (i) => `fixture-proxy-${String(i).padStart(5, "0")}`;
const proxyEntry = (i) => ({
  id: proxyID(i), url: `http://p${String(i).padStart(5, "0")}.fixture.invalid:8080`,
  name: i === 10000 ? "FIXTURE_ACTIVE_TAIL" : `FIXTURE_PROXY_${String(i).padStart(5, "0")}`,
  remark: i >= 9949 && i <= 9999 ? "FIXTURE_LAST_PAGE_GROUP" : "",
  enabled: true, health: "healthy", consecutive_failures: 0,
  latency_ms: 9, egress_ip: "192.0.2.1", last_checked_at: "2026-11-05T00:00:00Z",
});
const settings = {
  roe_enforcement: "warn", finding_inline_merge_enabled: false,
  llm_records_retention_days: 30, finding_verify_mode: "auto",
  credential_agent_enabled: false, credential_traffic_binding: false,
  task_intelligence_enabled: false, task_auto_title_enabled: false, chat_auto_title_enabled: false,
  traffic_capture: false, browser_screenshot_enabled: false, browser_screenshot_timestamp_enabled: false,
  opsec_enabled: false, agent_traffic_binding: false, llm_record: false, deepseek_search_key_set: false,
  proxy_pool: Array.from({ length: initialCount }, (_, i) => proxyEntry(i)),
  proxy_pool_active_id: proxyID(10000), proxy_pool_switch_log: [],
  proxy_rotate_timed_enabled: false, proxy_rotate_interval_seconds: 30,
  proxy_rotate_on_failure: false, proxy_rotate_strategy: "sequential",
  proxy_health_fail_threshold: 3, proxy_health_cooldown_seconds: 60,
  proxy_health_check_interval_seconds: 30, proxy_reconnect_max_attempts: 5,
  proxy_reconnect_interval_seconds: 30,
  web_search_enabled: false, web_search_backend: "ddgs", web_search_proxy: "",
  brave_key_set: false, tavily_key_set: false, global_proxy: proxyEntry(10000).url,
  show_egress_ip: false, python_interpreter: "", workers: 3, worker_pools: [],
  callback_addr: "", callback_addr_source: "unset", callback_addr_locked: false,
  task_concurrency_enabled: false, task_concurrency_limit: 5,
  llm_pool_enabled: false, llm_pool_bind_fallback: false,
  constraints_inject_planner: true, constraints_inject_worker: true, noa_compaction: false,
  notify_enabled: true, notify_public_base_url: "", notify_digest_interval_min: 30,
};
const state = { holdEgress: false, liveEgressReads: 0, peakEgressReads: 0, failBulk: false, recoverOnCleanup: "", rotateOnBulk: "" };
const requests = [], unexpected = new Set(), external = new Set(), errors = [], heldEgress = [];

function json(response, code, value) {
  if (response.destroyed || response.writableEnded) return;
  response.writeHead(code, { "Content-Type": "application/json", "Cache-Control": "no-store" });
  response.end(JSON.stringify(value));
}
function egressSettings() {
  return { show_egress_ip: settings.show_egress_ip, global_proxy: settings.global_proxy };
}
function proxySnapshot() {
  return { entries: settings.proxy_pool, active_id: settings.proxy_pool_active_id, active_proxy: settings.global_proxy, switch_log: settings.proxy_pool_switch_log };
}
function probeReport() {
  return {
    ok: true, capture: false, configured: settings.global_proxy, agent_proxy: settings.global_proxy,
    checks: ["direct", "global"].map((key) => ({
      key, label: key, result: { ok: true, exit_ip: key === "direct" ? "192.0.2.2" : "192.0.2.1", latency_ms: 1, probes: [] },
    })),
  };
}
async function serveAPI(request, response, url) {
  const endpoint = url.pathname, method = request.method || "GET";
  const chunks = [];
  for await (const chunk of request) chunks.push(chunk);
  const raw = Buffer.concat(chunks).toString();
  assert.ok(raw.length < 1024 * 1024, "Synthetic request grew beyond the fixture bound");
  const body = raw ? JSON.parse(raw) : {};
  requests.push({ endpoint, method, body, query: url.search });
  if (endpoint === "/api/settings/egress" && method === "GET") {
    state.liveEgressReads += 1;
    state.peakEgressReads = Math.max(state.peakEgressReads, state.liveEgressReads);
    let counted = true;
    response.once("close", () => { if (counted) { counted = false; state.liveEgressReads -= 1; } });
    const snapshot = egressSettings();
    assert.deepEqual(Object.keys(snapshot).sort(), ["global_proxy", "show_egress_ip"]);
    if (state.holdEgress) {
      const held = { released: false, release() { if (!this.released) { this.released = true; json(response, 200, snapshot); } } };
      heldEgress.push(held); return;
    }
    return json(response, 200, snapshot);
  }
  if (endpoint === "/api/settings" && method === "GET") return json(response, 200, settings);
  if (endpoint === "/api/settings" && method === "PUT") {
    assert.deepEqual(Object.keys(body), ["show_egress_ip"], "Proxy UI changed an unrelated setting");
    settings.show_egress_ip = body.show_egress_ip;
    return json(response, 200, settings);
  }
  if (endpoint === "/api/settings/proxy/test" && method === "POST") {
    assert.equal(body.resume, undefined, "A probe silently requested task resume");
    return json(response, 200, probeReport());
  }
  if (endpoint === "/api/settings/proxy-pool" && method === "GET") return json(response, 200, proxySnapshot());
  if (endpoint === "/api/settings/proxy-pool/import" && method === "POST") {
    assert.equal(typeof body.text, "string");
    const importErrors = Array.from({ length: 5000 }, (_, i) => ({
      line: i + 1, input: "http://import.fixture.invalid:8080", error: `FIXTURE_IMPORT_ERROR_${i + 1}_${"X".repeat(260)}`,
    }));
    return json(response, 200, {
      ...(url.searchParams.get("summary") === "1" ? { ok: true, imported: 0, count: settings.proxy_pool.length } : proxySnapshot()),
      errors: importErrors, probe_queued: true,
    });
  }
  if (endpoint === "/api/settings/proxy-pool/test-all" && method === "POST") {
    return json(response, 200, {
      ...(url.searchParams.get("summary") === "1" ? { ok: true, count: settings.proxy_pool.length } : proxySnapshot()),
      probe_queued: true,
    });
  }
  if (endpoint === "/api/settings/proxy-pool/bulk" && method === "POST") {
    assert.ok(["delete", "disable"].includes(body.action));
    assert.ok(Array.isArray(body.ids) && body.ids.length > 0);
    if (state.failBulk) { state.failBulk = false; return json(response, 500, { error: "FIXTURE_BULK_STORAGE_FAILURE" }); }
    if (state.rotateOnBulk) {
      const next = settings.proxy_pool.find((entry) => entry.id === state.rotateOnBulk);
      settings.proxy_pool_active_id = next.id; settings.global_proxy = next.url; state.rotateOnBulk = "";
    }
    const ids = new Set(body.ids), active = settings.proxy_pool_active_id;
    if (!body.only_unhealthy && ids.has(active) && !(body.allow_active && body.expected_active_id === active)) return json(response, 409, { error: "当前使用代理已变更，请刷新后重试" });
    if (state.recoverOnCleanup) {
      const recovered = settings.proxy_pool.find((entry) => entry.id === state.recoverOnCleanup);
      recovered.health = "healthy"; recovered.consecutive_failures = 0; recovered.last_error = ""; state.recoverOnCleanup = "";
    }
    let changed = 0;
    settings.proxy_pool = settings.proxy_pool.filter((entry) => {
      if (!ids.has(entry.id)) return true;
      if (body.only_unhealthy && (entry.id === active || !["unhealthy-cooling", "disabled"].includes(entry.health) || !entry.last_checked_at || !entry.last_error || entry.consecutive_failures < settings.proxy_health_fail_threshold)) return true;
      if (body.action === "delete") { changed++; return false; }
      if (entry.enabled) { changed++; entry.enabled = false; entry.health = "disabled"; }
      return true;
    });
    if (!settings.proxy_pool.some((entry) => entry.id === active && entry.enabled)) {
      const next = settings.proxy_pool.find((entry) => entry.enabled && entry.health !== "unhealthy-cooling") ?? settings.proxy_pool.find((entry) => entry.enabled);
      settings.proxy_pool_active_id = next?.id ?? ""; settings.global_proxy = next?.url ?? "";
    }
    return json(response, 200, { ok: true, changed, skipped: ids.size - changed, active_id: settings.proxy_pool_active_id, active_changed: settings.proxy_pool_active_id !== active });
  }
  const entry = endpoint.match(/^\/api\/settings\/proxy-pool\/([^/]+)(?:\/(activate|test))?$/);
  if (entry) {
    const id = decodeURIComponent(entry[1]);
    const found = settings.proxy_pool.find((p) => p.id === id);
    assert.ok(found, "Proxy action did not refer to a known stable ID");
    if (entry[2] === "activate" && method === "POST") {
      settings.proxy_pool_active_id = id; settings.global_proxy = found.url;
      return json(response, 200, { ok: true, active_id: id, active_proxy: found.url });
    }
    if (entry[2] === "test" && method === "POST") return json(response, 200, { ok: true, result: probeReport().checks[1].result });
    if (!entry[2] && method === "PUT") {
      assert.deepEqual(Object.keys(body), ["enabled"]);
      found.enabled = body.enabled; return json(response, 200, { entry: found });
    }
    if (!entry[2] && method === "DELETE") {
      assert.notEqual(id, settings.proxy_pool_active_id, "Fixture should delete a non-active last-page row");
      settings.proxy_pool = settings.proxy_pool.filter((p) => p.id !== id);
      return json(response, 200, { ok: true });
    }
  }
  const control = endpoint.match(/^\/api\/settings\/runtime-controls\/(roe_enforcement|finding_verify_mode|llm_records_retention_days)$/);
  if (control && method === "GET") return json(response, 200, { [control[1]]: settings[control[1]] });
  if (endpoint === "/api/settings/basic-auth") return json(response, 200, { enabled: false, username: "", password_set: false });
  if (endpoint === "/api/update/settings") return json(response, 200, { proxy: "" });
  if (endpoint === "/api/update/check") return json(response, 200, { current: "2.4.11-fixture", mode: "binary", os: "darwin", arch: "arm64", repo: "fixture", has_backup: false, has_update: false, comparable: false });
  if (["/api/version", "/api/health"].includes(endpoint)) return json(response, 200, { ok: true, service: "fixture", version: "2.4.11-fixture" });
  if (endpoint === "/api/auth/status") return json(response, 200, { initialized: true });
  if (endpoint === "/api/tasks") return json(response, 200, { tasks: [], active: "" });
  if (endpoint === "/api/agents") return json(response, 200, { agents: [] });
  if (endpoint === "/api/llm/profiles") return json(response, 200, { profiles: [] });
  if (endpoint === "/api/intercept/pending") return json(response, 200, { pending: [] });
  if (endpoint === "/api/skills/missing") return json(response, 200, { missing: [] });
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
async function eventually(check, message) {
  const deadline = Date.now() + 15000;
  while (Date.now() < deadline) {
    if (await check()) return;
    await new Promise((resolve) => setTimeout(resolve, 40));
  }
  assert.fail(message);
}
function callCount(endpoint, method = "GET") { return requests.filter((r) => r.endpoint === endpoint && r.method === method).length; }
let browser, page;
const importDraft = () => Array.from({ length: 5000 }, (_, i) => `http://import${i}.fixture.invalid:8080`).join("\n");
async function main() {
  server.listen(0, "127.0.0.1"); await once(server, "listening");
  const origin = `http://127.0.0.1:${server.address().port}`;
  browser = await chromium.launch({ headless: true });
  const context = await browser.newContext({ viewport: { width: 1500, height: 1100 }, locale: "zh-CN", timezoneId: "Asia/Shanghai" });
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
    // Shorten only the application's ten-second poll for a bounded CI test.
    // The callback and its single-flight logic are unchanged.
    const setIntervalOriginal = window.setInterval.bind(window);
    window.setInterval = (callback, delay, ...args) => setIntervalOriginal(callback, delay === 10000 ? 250 : delay, ...args);
    window.fixturePeakProxyRows = 0;
    new MutationObserver(() => {
      window.fixturePeakProxyRows = Math.max(window.fixturePeakProxyRows, document.querySelectorAll('[data-testid="proxy-pool-row"]').length);
    }).observe(document, { childList: true, subtree: true });
  }, { token });
  // A plain editable control using the same browser and input helper separates
  // driver/Chromium insertion failures from ARTEX component behavior.
  const plainPage = await context.newPage();
  plainPage.setDefaultTimeout(15000);
  try {
    await plainPage.setContent('<textarea aria-label="plain proxy input" wrap="off" spellcheck="false" style="field-sizing:fixed;height:128px;width:700px"></textarea>');
    console.log("STAGE before plain 5,000-line native text insertion");
    await plainPage.locator("textarea").focus();
    await plainPage.keyboard.insertText(importDraft());
    assert.equal(await plainPage.locator("textarea").inputValue(), importDraft());
    console.log("PASS plain 5,000-line native text insertion completes in the cloud browser");
  } finally { await plainPage.close(); }
  page = await context.newPage(); page.setDefaultTimeout(15000);
  page.on("pageerror", (error) => errors.push(error.message));
  try {
    await page.goto(origin + "/system/settings/", { waitUntil: "networkidle" });
    await page.getByRole("textbox", { name: "搜索系统设置", exact: true }).waitFor();
    await eventually(() => callCount("/api/settings") === 1 && callCount("/api/settings/egress") >= 1, "Initial settings or lightweight egress snapshot missing");
    assert.equal(await page.getByTestId("proxy-pool-row").count(), 0, "Default category mounted hidden proxy rows");
    assert.equal(await page.getByTestId("proxy-pool-list").count(), 0, "Default category mounted the large proxy subtree");
    assert.equal(settings.proxy_pool.length, initialCount);
    assert.equal(requests.filter((r) => ["PUT", "POST", "DELETE"].includes(r.method)).length, 0, "Mounting the page performed a mutation or probe");
    console.log("PASS 10,001 proxies do not mount on the default settings category");

    await page.getByRole("button", { name: /^流量与安全/ }).click();
    const list = () => page.getByTestId("proxy-pool-list");
    const rows = () => page.getByTestId("proxy-pool-row");
    const search = () => page.getByRole("textbox", { name: "搜索代理", exact: true });
    await eventually(async () => await rows().count() === 50, "Visible proxy list did not use its 50-row page bound");
    await list().getByText("当前：FIXTURE_ACTIVE_TAIL", { exact: true }).waitFor();
    assert.equal(await rows().filter({ hasText: "当前使用" }).count(), 0, "An off-page active proxy was assigned to a visible row");
    await list().getByRole("button", { name: "下一页代理", exact: true }).click();
    await page.locator(`[data-testid="proxy-pool-row"][data-proxy-id="${proxyID(50)}"]`).waitFor();
    assert.equal(await rows().count(), 50);
    console.log("PASS visible rows stay paged and the active proxy is resolved from the entire pool");

    await search().fill("FIXTURE_LAST_PAGE_GROUP");
    await list().getByText("第 1 / 2 页", { exact: true }).waitFor();
    assert.equal(await rows().count(), 50);
    await list().getByRole("button", { name: "下一页代理", exact: true }).click();
    const tailRow = page.locator(`[data-testid="proxy-pool-row"][data-proxy-id="${proxyID(9999)}"]`);
    await tailRow.waitFor(); assert.equal(await rows().count(), 1);
    await tailRow.getByRole("button", { name: "删除", exact: true }).click();
    await list().getByText("第 1 / 1 页", { exact: true }).waitFor();
    await eventually(async () => await rows().count() === 50, "Deleting the sole last-page row did not clamp the page");
    assert.equal(await list().getByRole("button", { name: "下一页代理", exact: true }).isDisabled(), true);
    assert.equal(settings.proxy_pool.length, initialCount - 1);
    assert.equal(settings.proxy_pool_active_id, proxyID(10000));
    assert.equal(requests.filter((r) => r.method === "DELETE").length, 1);
    console.log("PASS search resets pagination; last-page deletion clamps without changing the active route");

    await search().fill("FIXTURE_ACTIVE_TAIL");
    const activeRow = page.locator(`[data-testid="proxy-pool-row"][data-proxy-id="${proxyID(10000)}"]`);
    await activeRow.getByText("当前使用", { exact: true }).waitFor();
    assert.equal(await rows().count(), 1);
    assert.equal(await activeRow.getByRole("button", { name: "使用", exact: true }).isDisabled(), true);
    await search().fill(proxyID(17));
    const selectedRow = page.locator(`[data-testid="proxy-pool-row"][data-proxy-id="${proxyID(17)}"]`);
    await selectedRow.waitFor(); assert.equal(await rows().count(), 1);
    await selectedRow.getByRole("button", { name: "使用", exact: true }).click();
    await selectedRow.getByText("当前使用", { exact: true }).waitFor();
    assert.equal(settings.proxy_pool_active_id, proxyID(17));
    assert.equal(settings.global_proxy, proxyEntry(17).url);
    assert.equal(callCount(`/api/settings/proxy-pool/${proxyID(17)}/activate`, "POST"), 1);
    console.log("PASS filtered row actions use stable proxy IDs and refresh the persisted route");

    await list().getByRole("button", { name: "批量导入代理", exact: true }).click();
    const importText = () => page.locator('textarea[aria-label="批量代理文本"]');
    const draft = importDraft();
    await importText().waitFor({ state: "visible" });
    const editorStyle = await importText().evaluate((element) => ({
      fieldSizing: getComputedStyle(element).getPropertyValue("field-sizing"),
      wrap: element.wrap, spellcheck: element.spellcheck,
    }));
    assert.equal(editorStyle.fieldSizing, "fixed", "Bulk import editor used content-based field sizing");
    assert.equal(editorStyle.wrap, "off", "Bulk import editor used line wrapping");
    assert.equal(editorStyle.spellcheck, false, "Bulk import editor enabled expensive spellchecking");
    await importText().evaluate((element) => {
      window.fixtureProxyInputEvents = [];
      element.addEventListener("input", () => window.fixtureProxyInputEvents.push({ length: element.value.length, at: performance.now() }));
    });
    console.log("STAGE before single-line CSS fill");
    await importText().fill("http://single.fixture.invalid:8080");
    assert.equal(await importText().inputValue(), "http://single.fixture.invalid:8080");
    console.log("STAGE after single-line CSS fill");
    console.log("STAGE before large import paste");
    await importText().focus();
    await page.keyboard.press("ControlOrMeta+A");
    await page.keyboard.insertText(draft);
    assert.equal(await importText().inputValue(), draft, "The complete 5,000-line paste was not accepted");
    const inputEvents = await page.evaluate(() => window.fixtureProxyInputEvents);
    assert.ok(inputEvents.some((event) => event.length === draft.length), "The bulk editor did not receive the complete input event");
    console.log("STAGE bulk input lengths", inputEvents.map((event) => event.length).join(","));
    console.log("STAGE after large import paste");
    const textareaBox = await importText().boundingBox();
    assert.ok(textareaBox && textareaBox.height <= 140, "Large import draft expanded the textarea beyond its fixed height");
    console.log("STAGE before switching away from proxy category");
    await page.getByRole("button", { name: /^系统与账号/ }).click();
    console.log("STAGE after switching away from proxy category");
    assert.equal(await rows().count(), 0, "Switching away retained hidden proxy rows");
    console.log("STAGE before remounting proxy category");
    await page.getByRole("button", { name: /^流量与安全/ }).click();
    console.log("STAGE after remounting proxy category");
    await list().getByRole("button", { name: "批量导入代理", exact: true }).click();
    assert.equal(await importText().inputValue(), draft, "Unmounting the category discarded the unsaved import draft");
    await list().getByRole("button", { name: "导入", exact: true }).click();
    const errorToast = page.locator("[data-sonner-toast]").filter({ hasText: "共 5000 行导入失败" }).first();
    await errorToast.waitFor();
    const errorText = await errorToast.innerText();
    assert.ok(errorText.length < 1100, "Import errors created an unbounded toast");
    assert.match(errorText, /仅显示前 5 条/);
    assert.match(errorText, /FIXTURE_IMPORT_ERROR_5_/);
    assert.doesNotMatch(errorText, /FIXTURE_IMPORT_ERROR_6_|FIXTURE_IMPORT_ERROR_5000_/);
    assert.equal(callCount("/api/settings/proxy-pool/import", "POST"), 1, "Import was replayed automatically");
    assert.equal(requests.find((r) => r.endpoint === "/api/settings/proxy-pool/import").query, "?summary=1", "Import retrieved a redundant full proxy pool");
    assert.equal(settings.proxy_pool.length, initialCount - 1);
    console.log("PASS large drafts survive category unmount and 5,000 errors produce a bounded sample notification");

    await eventually(async () => await list().getByRole("button", { name: "测试全部", exact: true }).isEnabled(), "Import kept all proxy operations disabled");
    await list().getByRole("button", { name: "测试全部", exact: true }).click();
    await eventually(() => callCount("/api/settings/proxy-pool/test-all", "POST") === 1, "Batch probe acknowledgement missing");
    await eventually(async () => await list().getByRole("button", { name: "测试全部", exact: true }).isEnabled(), "Queued batch probe was treated as a long-running UI request");
    const batchToast = page.locator("[data-sonner-toast]").filter({ hasText: /后台.*探测|探测.*后台|代理探测已排队/ }).first();
    await batchToast.waitFor();
    assert.equal(await page.locator("[data-sonner-toast]").filter({ hasText: "已完成全部代理探测" }).count(), 0, "Queued batch was mislabeled complete");
    assert.equal(requests.find((r) => r.endpoint === "/api/settings/proxy-pool/test-all").query, "?summary=1", "Batch probe retrieved a redundant full pool");
    assert.equal(settings.proxy_pool_active_id, proxyID(17));
    console.log("PASS batch probe returns a small queued acknowledgement without claiming completion or changing the route");

    await eventually(async () => await page.locator("#show-egress-ip").isEnabled(), "Egress preference remained busy after import");
    await page.locator("#show-egress-ip").click();
    await page.getByRole("button", { name: "刷新出口 IP", exact: true }).waitFor();
    await eventually(async () => await page.getByRole("button", { name: "刷新出口 IP", exact: true }).isEnabled(), "Synthetic initial egress probe did not finish");
    state.holdEgress = true;
    await eventually(() => heldEgress.length === 1, "The egress poll did not use its lightweight endpoint");
    const readsDuringHold = callCount("/api/settings/egress");
    const fullReadsDuringHold = callCount("/api/settings");
    await new Promise((resolve) => setTimeout(resolve, 850));
    assert.equal(callCount("/api/settings/egress"), readsDuringHold, "Egress polling overlapped an unresolved lightweight request");
    assert.equal(callCount("/api/settings"), fullReadsDuringHold, "Egress polling reloaded the complete proxy pool");
    assert.equal(state.peakEgressReads, 1, "More than one egress settings read was in flight");
    state.holdEgress = false; heldEgress[0].release();
    await eventually(async () => await page.locator("#show-egress-ip").isEnabled(), "Released egress poll stayed busy");
    await page.locator("#show-egress-ip").click();
    await eventually(() => settings.show_egress_ip === false, "Explicit egress preference close did not persist");
    console.log("PASS egress refresh is lightweight and single-flight while a response is held");

    // Cross-page selection and cancellation submit no individual mutations.
    const bulkPath = "/api/settings/proxy-pool/bulk";
    const choose = (i) => page.getByRole("checkbox", { name: `选择代理 ${proxyID(i)}`, exact: true });
    const dialog = () => page.getByRole("alertdialog");
    await search().fill("");
    await choose(0).waitFor(); await choose(0).click();
    await list().getByRole("button", { name: "下一页代理", exact: true }).click();
    await choose(50).waitFor(); await choose(50).click();
    await list().getByText("已选 2 条（跨页保留）", { exact: true }).waitFor();
    await list().getByRole("button", { name: "删除选中", exact: true }).click();
    await dialog().getByRole("heading", { name: "确认删除 2 条代理？", exact: true }).waitFor();
    await dialog().getByRole("button", { name: "取消", exact: true }).click();
    assert.equal(callCount(bulkPath, "POST"), 0);
    await list().getByRole("button", { name: "禁用选中", exact: true }).click();
    await dialog().getByRole("button", { name: "确认禁用", exact: true }).click();
    await list().getByText("已选 0 条（跨页保留）", { exact: true }).waitFor();
    assert.equal(callCount(bulkPath, "POST"), 1);
    assert.deepEqual([...requests.filter((r) => r.endpoint === bulkPath).at(-1).body.ids].sort(), [proxyID(0), proxyID(50)].sort());
    assert.equal(settings.proxy_pool.find((entry) => entry.id === proxyID(0)).enabled, false);
    assert.equal(settings.proxy_pool.find((entry) => entry.id === proxyID(50)).enabled, false);
    assert.equal(settings.proxy_pool_active_id, proxyID(17));
    console.log("PASS cross-page disable uses one request; cancellation does not mutate or change the active exit");

    await choose(50).click();
    await list().getByRole("button", { name: "下一页代理", exact: true }).click();
    await choose(100).waitFor(); await choose(100).click();
    state.failBulk = true;
    await list().getByRole("button", { name: "删除选中", exact: true }).click();
    await dialog().getByRole("button", { name: "确认删除", exact: true }).click();
    await page.locator("[data-sonner-toast]").filter({ hasText: "FIXTURE_BULK_STORAGE_FAILURE" }).first().waitFor();
    await eventually(async () => await list().getByRole("button", { name: "删除选中", exact: true }).isEnabled(), "Failed batch kept controls busy");
    await list().getByText("已选 2 条（跨页保留）", { exact: true }).waitFor();
    assert.equal(callCount(bulkPath, "POST"), 2, "Failed batch automatically replayed");
    assert.ok(settings.proxy_pool.some((entry) => entry.id === proxyID(50)) && settings.proxy_pool.some((entry) => entry.id === proxyID(100)));
    await list().getByRole("button", { name: "删除选中", exact: true }).click();
    await dialog().getByRole("button", { name: "确认删除", exact: true }).click();
    await list().getByText("已选 0 条（跨页保留）", { exact: true }).waitFor();
    assert.equal(callCount(bulkPath, "POST"), 3);
    console.log("PASS failed bulk delete preserves selection and requires an explicit retry");

    for (let i = 200; i < 300; i++) settings.proxy_pool.find((entry) => entry.id === proxyID(i)).name = "FIXTURE_BULK_GROUP";
    await list().getByRole("button", { name: "刷新状态", exact: true }).click();
    await search().fill("FIXTURE_BULK_GROUP");
    await list().getByText("第 1 / 2 页", { exact: true }).waitFor();
    await list().getByRole("button", { name: "全选匹配（100）", exact: true }).click();
    await list().getByText("已选 100 条（跨页保留）", { exact: true }).waitFor();
    assert.equal(await rows().count(), 50);
    await search().fill("NO_FIXTURE_MATCH");
    await list().getByText("已选 0 条（跨页保留）", { exact: true }).waitFor();
    await search().fill("FIXTURE_BULK_GROUP");
    await list().getByRole("button", { name: "全选匹配（100）", exact: true }).click();
    await list().getByRole("button", { name: "删除选中", exact: true }).click();
    await dialog().getByRole("heading", { name: "确认删除 100 条代理？", exact: true }).waitFor();
    await dialog().getByRole("button", { name: "确认删除", exact: true }).click();
    await list().getByText("没有匹配的代理。", { exact: true }).waitFor();
    assert.equal(requests.filter((r) => r.endpoint === bulkPath).at(-1).body.ids.length, 100);
    console.log("PASS select-all matching includes every filtered page with only 50 mounted rows");

    await search().fill("");
    const failedPatch = { health: "unhealthy-cooling", last_checked_at: "2026-11-05T00:00:00Z", last_error: "FIXTURE_FAILED_PROXY", consecutive_failures: 3 };
    for (const i of [17, 400, 401, 403, 404]) Object.assign(settings.proxy_pool.find((entry) => entry.id === proxyID(i)), failedPatch);
    settings.proxy_pool.find((entry) => entry.id === proxyID(403)).consecutive_failures = 2;
    settings.proxy_pool.find((entry) => entry.id === proxyID(404)).health = "healthy";
    await list().getByRole("button", { name: "刷新状态", exact: true }).click();
    await list().getByRole("button", { name: "清理失效（2）", exact: true }).waitFor();
    await page.locator("details").filter({ has: page.getByText("轮换与健康检查策略", { exact: true }) }).locator("summary").click();
    await page.locator("#proxy-pool-threshold").fill("1");
    state.recoverOnCleanup = proxyID(401);
    const probesBeforeCleanup = callCount("/api/settings/proxy-pool/test-all", "POST");
    await list().getByRole("button", { name: "清理失效（2）", exact: true }).click();
    await dialog().getByRole("heading", { name: "确认清理 2 条失效代理？", exact: true }).waitFor();
    await dialog().getByRole("button", { name: "确认删除", exact: true }).click();
    await page.locator("[data-sonner-toast]").filter({ hasText: "已删除 1 条代理，跳过 1 条" }).first().waitFor();
    const cleanup = requests.filter((r) => r.endpoint === bulkPath).at(-1).body;
    assert.equal(cleanup.only_unhealthy, true); assert.equal(cleanup.allow_active, false);
    assert.deepEqual([...cleanup.ids].sort(), [proxyID(400), proxyID(401)].sort());
    for (const i of [17, 401, 402, 403, 404]) assert.ok(settings.proxy_pool.some((entry) => entry.id === proxyID(i)));
    assert.equal(settings.proxy_pool_active_id, proxyID(17));
    assert.equal(callCount("/api/settings/proxy-pool/test-all", "POST"), probesBeforeCleanup);
    console.log("PASS cleanup uses the saved threshold, skips current/unchecked/recovered proxies and starts no probe");

    await page.getByRole("checkbox", { name: "只看最近检测失败的代理", exact: true }).click();
    await search().fill("fixture-proxy-004");
    await list().getByRole("button", { name: "全选匹配（2）", exact: true }).waitFor();
    await list().getByRole("button", { name: "全选匹配（2）", exact: true }).click();
    await list().getByRole("button", { name: "删除选中", exact: true }).click();
    await dialog().getByRole("button", { name: "确认删除", exact: true }).click();
    await list().getByText("没有匹配的代理。", { exact: true }).waitFor();
    assert.deepEqual([...requests.filter((r) => r.endpoint === bulkPath).at(-1).body.ids].sort(), [proxyID(403), proxyID(404)].sort());
    assert.ok(settings.proxy_pool.some((entry) => entry.id === proxyID(401)));
    await page.getByRole("checkbox", { name: "只看最近检测失败的代理", exact: true }).click();
    console.log("PASS explicit recent-failure filtering supports deletion below the automatic cleanup threshold");

    await search().fill("fixture-proxy-0001");
    await choose(17).waitFor(); await choose(17).click(); await choose(18).click();
    await list().getByRole("button", { name: "删除选中", exact: true }).click();
    assert.match(await dialog().innerText(), /包含当前使用的代理/);
    state.rotateOnBulk = proxyID(18);
    await dialog().getByRole("button", { name: "确认删除", exact: true }).click();
    await page.locator("[data-sonner-toast]").filter({ hasText: "当前使用代理已变更" }).first().waitFor();
    await eventually(async () => await list().getByRole("button", { name: "刷新状态", exact: true }).isEnabled(), "Stale active acknowledgement stayed busy");
    assert.ok(settings.proxy_pool.some((entry) => entry.id === proxyID(17)) && settings.proxy_pool.some((entry) => entry.id === proxyID(18)));
    await list().getByRole("button", { name: "刷新状态", exact: true }).click();
    await eventually(async () => await list().getByRole("button", { name: "删除选中", exact: true }).isEnabled(), "Refresh stayed busy");
    await list().getByRole("button", { name: "删除选中", exact: true }).click();
    await dialog().getByRole("button", { name: "确认删除", exact: true }).click();
    await list().getByText("已选 0 条（跨页保留）", { exact: true }).waitFor();
    assert.ok(![proxyID(17), proxyID(18)].includes(settings.proxy_pool_active_id));
    console.log("PASS current-proxy changes reject stale confirmation and require an explicit refreshed confirmation");

    await search().fill("");
    await list().getByRole("button", { name: /^全选匹配（/ }).click();
    const remainingCount = settings.proxy_pool.length;
    await list().getByRole("button", { name: "删除选中", exact: true }).click();
    await dialog().getByRole("heading", { name: `确认删除 ${remainingCount} 条代理？`, exact: true }).waitFor();
    assert.match(await dialog().innerText(), /没有已启用成员时改为直连/);
    await dialog().getByRole("button", { name: "确认删除", exact: true }).click();
    await list().getByText("暂无代理池条目。", { exact: true }).waitFor();
    assert.equal(settings.proxy_pool.length, 0); assert.equal(settings.global_proxy, ""); assert.equal(settings.proxy_pool_active_id, "");
    assert.equal(requests.filter((r) => r.endpoint === bulkPath).at(-1).body.ids.length, remainingCount);
    console.log("PASS thousands of selected proxies clear with one confirmed request and no hidden remaining exit");

    assert.ok(await page.evaluate(() => window.fixturePeakProxyRows) <= 50, "A transition temporarily mounted more than one page of proxies");
    assert.equal(requests.filter((r) => r.endpoint.startsWith("/api/tasks/") && r.method !== "GET").length, 0);
    assert.deepEqual(errors, [], "Browser exceptions");
    assert.deepEqual([...unexpected], [], "Unexpected synthetic API calls");
    assert.deepEqual([...external], [], "External request attempted");
    console.log("PASS bounded proxy DOM throughout; no real task, proxy probe or external request ran");
  } catch (error) {
    console.error("Original proxy UI error:", error.message.split("\n", 1)[0], `(diagnostic characters=${error.message.length})`);
    const body = await page.locator("body").innerText({ timeout: 1000 }).catch((diagnosticError) => `[body unavailable: ${diagnosticError.message}]`);
    console.error(JSON.stringify({ route: page.url(), requests: requests.slice(-15).map(({ endpoint, method, query }) => ({ endpoint, method, query })), unexpected: [...unexpected], body: body.slice(0, 5000) }, null, 2));
    throw error;
  } finally { await context.close(); }
}
main().catch((error) => { console.error(error.message.split("\n", 1)[0], `(diagnostic characters=${error.message.length})`); process.exitCode = 1; }).finally(async () => {
  for (const held of heldEgress) held.release();
  if (browser) await browser.close();
  server.closeAllConnections();
  if (server.listening) await new Promise((resolve) => server.close(resolve));
});
