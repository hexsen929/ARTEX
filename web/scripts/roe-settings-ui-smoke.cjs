#!/usr/bin/env node
/* Cloud-only synthetic RoE settings UI regression. No ARTEX process, database,
 * target tool, real credential, or external network connection is used. */
"use strict";
const assert = require("node:assert/strict");
const fs = require("node:fs");
const http = require("node:http");
const path = require("node:path");
const { once } = require("node:events");
if (!process.env.CI || !process.env.ARTEX_PLAYWRIGHT_DIR) {
  throw new Error("Run in GitHub Actions with its temporary Playwright runtime.");
}
const { chromium } = require(path.resolve(process.env.ARTEX_PLAYWRIGHT_DIR));
const output = path.resolve(__dirname, "..", "out");
if (!fs.existsSync(path.join(output, "system", "settings", "index.html"))) throw new Error("Missing cloud static export");
const settings = {
  roe_enforcement: "strict", finding_inline_merge_enabled: false,
  llm_records_retention_days: 30, finding_verify_mode: "auto",
  credential_agent_enabled: false, credential_traffic_binding: false,
  task_intelligence_enabled: false, task_auto_title_enabled: false, chat_auto_title_enabled: false,
  traffic_capture: false, browser_screenshot_enabled: false, browser_screenshot_timestamp_enabled: false,
  opsec_enabled: false, agent_traffic_binding: false, llm_record: false,
  web_search_enabled: false, web_search_backend: "ddgs", web_search_proxy: "",
  brave_key_set: false, tavily_key_set: false, global_proxy: "", show_egress_ip: false,
  python_interpreter: "", workers: 3, worker_pools: [],
  callback_addr: "", callback_addr_source: "unset", callback_addr_locked: false,
  task_concurrency_enabled: false, task_concurrency_limit: 5,
  llm_pool_enabled: false, llm_pool_bind_fallback: false,
  constraints_inject_planner: true, constraints_inject_worker: true, noa_compaction: false,
  notify_enabled: true, notify_public_base_url: "", notify_digest_interval_min: 30,
};
const state = { failNextRoESave: false, ignoreNextRoESave: false };
const controlKeys = ["roe_enforcement", "llm_records_retention_days", "finding_verify_mode"];
function validControl(key, value) {
  if (key === "roe_enforcement") return ["off", "warn", "strict"].includes(value);
  if (key === "finding_verify_mode") return ["auto", "off"].includes(value);
  return key === "llm_records_retention_days" && Number.isInteger(value) && value >= 0 && value <= 106751;
}
const requests = [], unexpected = new Set(), external = new Set(), errors = [];
function json(response, code, value) {
  response.writeHead(code, { "Content-Type": "application/json", "Cache-Control": "no-store" });
  response.end(JSON.stringify(value));
}
async function api(request, response, url) {
  const endpoint = url.pathname, method = request.method || "GET";
  const chunks = [];
  for await (const chunk of request) chunks.push(chunk);
  const raw = Buffer.concat(chunks).toString();
  const body = raw ? JSON.parse(raw) : {};
  requests.push({ endpoint, method, body });
  const control = endpoint.match(/^\/api\/settings\/runtime-controls\/([^/]+)$/);
  if (control) {
    const key = control[1];
    if (!controlKeys.includes(key)) return json(response, 404, { error: "fixture unknown control" });
    if (method === "GET") return validControl(key, settings[key])
      ? json(response, 200, { [key]: settings[key] })
      : json(response, 500, { error: `fixture invalid ${key}` });
    if (method === "PUT") {
      assert.deepEqual(Object.keys(body), ["value"], "Control request leaked another field");
      if (!validControl(key, body.value)) return json(response, 400, { error: "fixture invalid control value" });
      if (key === "roe_enforcement" && state.failNextRoESave) {
        state.failNextRoESave = false;
        return json(response, 503, { error: "fixture save unavailable" });
      }
      if (key === "roe_enforcement" && state.ignoreNextRoESave) {
        state.ignoreNextRoESave = false;
        return json(response, 200, { [key]: settings[key] });
      }
      settings[key] = body.value;
      return json(response, 200, { [key]: settings[key] });
    }
  }
  if (endpoint === "/api/settings" && method === "GET") {
    return controlKeys.some((key) => !validControl(key, settings[key]))
      ? json(response, 500, { error: "fixture stored controls are invalid" })
      : json(response, 200, settings);
  }
  if (endpoint === "/api/settings" && method === "PUT") {
    assert.deepEqual(Object.keys(body), ["opsec_enabled"], "Independent control used the full settings endpoint");
    if (controlKeys.some((key) => !validControl(key, settings[key]))) return json(response, 500, { error: "fixture stored peer is invalid" });
    Object.assign(settings, body);
    return json(response, 200, settings);
  }
  if (endpoint === "/api/settings/basic-auth") return json(response, 200, { enabled: false, username: "", password_set: false });
  if (endpoint === "/api/update/settings") return json(response, 200, { proxy: "" });
  if (endpoint === "/api/update/check") return json(response, 200, { current: "2.4.7-fixture", mode: "binary", os: "darwin", arch: "arm64", repo: "fixture", has_backup: false, has_update: false, comparable: false });
  if (endpoint === "/api/health") return json(response, 200, { ok: true, service: "fixture", version: "2.4.7-fixture" });
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
    if (url.pathname.startsWith("/api/")) return await api(request, response, url);
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
function modeWrites() { return requests.filter((r) => r.method === "PUT" && r.endpoint === "/api/settings/runtime-controls/roe_enforcement"); }
async function eventually(check, message) {
  const deadline = Date.now() + 15000;
  while (Date.now() < deadline) { if (await check()) return; await new Promise((resolve) => setTimeout(resolve, 40)); }
  assert.fail(message);
}
let browser;
async function main() {
  server.listen(0, "127.0.0.1"); await once(server, "listening");
  const origin = `http://127.0.0.1:${server.address().port}`;
  browser = await chromium.launch({ headless: true });
  const context = await browser.newContext({ viewport: { width: 1500, height: 1100 }, locale: "zh-CN", timezoneId: "Asia/Shanghai" });
  const page = await context.newPage(); page.setDefaultTimeout(15000);
  page.on("pageerror", (e) => errors.push(e.message));
  await context.route("**/*", (route) => {
    const url = new URL(route.request().url());
    if (url.origin === origin || ["data:", "blob:"].includes(url.protocol)) return route.continue();
    external.add(url.origin); return route.abort("blockedbyclient");
  });
  const token = [Buffer.from('{"alg":"none"}').toString("base64url"), Buffer.from('{"sub":"fixture-admin","exp":4102444800}').toString("base64url"), "fixture"].join(".");
  await page.addInitScript(({ token }) => {
    localStorage.setItem("artex_token", token);
    document.cookie = "artex_session=1; path=/; SameSite=Lax";
    document.cookie = "sidebar_state=false; path=/; SameSite=Lax";
  }, { token });
  const gotoSettings = async () => {
    await page.goto(origin + "/system/settings/", { waitUntil: "networkidle" });
    await page.getByRole("textbox", { name: "搜索系统设置", exact: true }).fill("roe_enforcement");
  };
  const card = () => page.locator('[data-slot="card"]').filter({ hasText: "授权范围核验（RoE）" }).first();
  const choose = async (mode) => {
    await card().getByRole("combobox").click();
    await page.getByRole("option", { name: new RegExp(`^${mode} ·`) }).click();
  };
  const assertCurrent = async (mode) => {
    await card().getByText(new RegExp(`^最近确认的模式：${mode} ·`)).waitFor();
    assert.match(await card().getByRole("combobox").innerText(), new RegExp(`^${mode} ·`));
  };
  try {
    await gotoSettings(); await assertCurrent("strict");
    assert.equal(modeWrites().length, 0, "Page load changed enforcement mode");
    console.log("PASS RoE search discovers the saved strict mode without writing defaults");
    await choose("warn");
    assert.equal(modeWrites().length, 0, "Selecting a draft wrote enforcement mode");
    await page.getByRole("textbox", { name: "搜索系统设置", exact: true }).fill("");
    await page.getByRole("button", { name: /^流量与安全/ }).click();
    await page.locator("#opsec-enabled").click();
    await eventually(() => requests.some((r) => r.method === "PUT" && r.body.opsec_enabled === true), "OPSEC fixture save missing");
    await eventually(() => page.locator("#opsec-enabled").isEnabled(), "Other settings response was not fully applied");
    assert.match(await card().getByRole("combobox").innerText(), /^warn ·/, "Another settings response replaced the unsaved RoE draft");
    assert.equal(settings.roe_enforcement, "strict");
    assert.equal(modeWrites().length, 0);
    console.log("PASS another settings save does not overwrite the unsaved RoE draft or change server mode");
    await card().getByRole("button", { name: "保存 RoE 模式", exact: true }).click();
    await assertCurrent("warn");
    assert.deepEqual(modeWrites().at(-1).body, { value: "warn" });
    console.log("PASS explicit save sends only the selected RoE field");
    await choose("off");
    const beforeOff = modeWrites().length;
    await card().getByRole("button", { name: "保存 RoE 模式", exact: true }).click();
    await page.getByRole("alertdialog").getByText("确认关闭 RoE 范围核验？", { exact: true }).waitFor();
    assert.equal(modeWrites().length, beforeOff);
    await page.getByRole("button", { name: "取消", exact: true }).click();
    assert.equal(modeWrites().length, beforeOff);
    await card().getByRole("button", { name: "保存 RoE 模式", exact: true }).click();
    await page.getByRole("button", { name: "确认关闭并保存", exact: true }).click();
    await assertCurrent("off");
    console.log("PASS disabling enforcement needs an explicit confirmation");
    state.failNextRoESave = true;
    await choose("strict");
    await card().getByRole("button", { name: "保存 RoE 模式", exact: true }).click();
    await card().getByText(/保存结果未确认/).waitFor();
    await card().getByText(/^最近确认的模式：off ·/).waitFor();
    assert.match(await card().getByRole("combobox").innerText(), /^strict ·/);
    assert.equal(settings.roe_enforcement, "off");
    await card().getByRole("button", { name: "重新读取 RoE 模式", exact: true }).click();
    await assertCurrent("off");
    console.log("PASS failed writes preserve the last confirmed value and require readback for certainty");
    state.ignoreNextRoESave = true;
    await choose("strict");
    await card().getByRole("button", { name: "保存 RoE 模式", exact: true }).click();
    await card().getByText(/服务返回模式与本次选择不一致/).waitFor();
    await card().getByText(/^最近确认的模式：off ·/).waitFor();
    assert.equal(settings.roe_enforcement, "off");
    await card().getByRole("button", { name: "重新读取 RoE 模式", exact: true }).click();
    await assertCurrent("off");
    console.log("PASS a successful HTTP response that ignores the field is not mislabeled as a successful save");
    await page.getByRole("textbox", { name: "搜索系统设置", exact: true }).fill("llm_records_retention_days");
    const retention = page.locator('[data-slot="card"]').filter({ hasText: "LLM 调用记录保留" }).first();
    await retention.getByText("最近确认的保留期：30 天", { exact: true }).waitFor();
    const retentionWrites = () => requests.filter((r) => r.method === "PUT" && r.endpoint === "/api/settings/runtime-controls/llm_records_retention_days");
    assert.equal(retentionWrites().length, 0);
    await retention.getByRole("spinbutton", { name: "LLM 调用记录保留天数", exact: true }).fill("7");
    await retention.getByRole("button", { name: "保存记录保留期", exact: true }).click();
    await page.getByRole("alertdialog").getByText("确认记录保留策略变更？", { exact: true }).waitFor();
    assert.equal(retentionWrites().length, 0, "Shortening retention wrote before confirmation");
    await page.getByRole("button", { name: "取消", exact: true }).click();
    assert.equal(settings.llm_records_retention_days, 30);
    await retention.getByRole("button", { name: "保存记录保留期", exact: true }).click();
    await page.getByRole("button", { name: "确认仅保存保留期", exact: true }).click();
    await retention.getByText("最近确认的保留期：7 天", { exact: true }).waitFor();
    assert.deepEqual(retentionWrites().at(-1).body, { value: 7 });
    await retention.getByRole("spinbutton", { name: "LLM 调用记录保留天数", exact: true }).fill("0");
    await retention.getByRole("button", { name: "保存记录保留期", exact: true }).click();
    await retention.getByText("最近确认的保留期：0 天（不自动清理）", { exact: true }).waitFor();
    const beforeFinite = retentionWrites().length;
    await retention.getByRole("spinbutton", { name: "LLM 调用记录保留天数", exact: true }).fill("30");
    await retention.getByRole("button", { name: "保存记录保留期", exact: true }).click();
    await page.getByRole("alertdialog").getByText("确认记录保留策略变更？", { exact: true }).waitFor();
    assert.equal(retentionWrites().length, beforeFinite, "Finite cleanup was enabled before confirmation");
    await page.getByRole("button", { name: "确认仅保存保留期", exact: true }).click();
    await retention.getByText("最近确认的保留期：30 天", { exact: true }).waitFor();
    assert.equal(requests.filter((r) => r.method === "DELETE").length, 0, "Settings UI issued immediate record deletion");
    console.log("PASS retention defaults are known, zero disables cleanup, and shorter/finite horizons require confirmation without immediate delete calls");
    await page.getByRole("textbox", { name: "搜索系统设置", exact: true }).fill("finding_verify_mode");
    const verify = page.locator('[data-slot="card"]').filter({ hasText: "发现自动验证" }).first();
    await verify.getByText(/^最近确认的自动验证模式：auto ·/).waitFor();
    const verifyWrites = () => requests.filter((r) => r.method === "PUT" && r.endpoint === "/api/settings/runtime-controls/finding_verify_mode");
    assert.equal(verifyWrites().length, 0);
    await verify.getByRole("combobox").click();
    await page.getByRole("option", { name: /^off ·/ }).click();
    await verify.getByRole("button", { name: "保存自动验证模式", exact: true }).click();
    await page.getByRole("alertdialog").getByText("确认自动验证模式变更？", { exact: true }).waitFor();
    assert.equal(verifyWrites().length, 0);
    await page.getByRole("button", { name: "确认仅保存验证模式", exact: true }).click();
    await verify.getByText(/^最近确认的自动验证模式：off ·/).waitFor();
    assert.deepEqual(verifyWrites().at(-1).body, { value: "off" });
    await verify.getByRole("combobox").click();
    await page.getByRole("option", { name: /^auto ·/ }).click();
    await page.getByRole("textbox", { name: "搜索系统设置", exact: true }).fill("llm_records_retention_days");
    await retention.getByRole("spinbutton", { name: "LLM 调用记录保留天数", exact: true }).fill("60");
    await retention.getByRole("button", { name: "保存记录保留期", exact: true }).click();
    await retention.getByText("最近确认的保留期：60 天", { exact: true }).waitFor();
    await page.getByRole("textbox", { name: "搜索系统设置", exact: true }).fill("finding_verify_mode");
    assert.match(await verify.getByRole("combobox").innerText(), /^auto ·/, "Another policy response erased an unsaved verification draft");
    assert.equal(settings.finding_verify_mode, "off", "Unsaved verification draft changed server state");
    assert.equal(settings.roe_enforcement, "off", "Another policy save changed RoE mode");
    console.log("PASS verification remains conditional, single-field, and independent of retention/RoE writes");
    const beforeCorruption = structuredClone(settings);
    settings.roe_enforcement = "fixture-invalid";
    settings.finding_verify_mode = "fixture-invalid-verifier";
    const beforeUnknown = modeWrites().length;
    await gotoSettings();
    await card().getByText("当前模式未知；未选择任何默认值", { exact: true }).waitFor();
    assert.equal(modeWrites().length, beforeUnknown);
    assert.ok(await card().getByRole("button", { name: "保存 RoE 模式", exact: true }).isDisabled());
    await page.getByRole("textbox", { name: "搜索系统设置", exact: true }).fill("llm_records_retention_days");
    await retention.getByText("最近确认的保留期：60 天", { exact: true }).waitFor();
    await retention.getByRole("spinbutton", { name: "LLM 调用记录保留天数", exact: true }).fill("45");
    await page.getByRole("textbox", { name: "搜索系统设置", exact: true }).fill("roe_enforcement");
    await choose("strict");
    await card().getByRole("button", { name: "保存 RoE 模式", exact: true }).click();
    await page.getByRole("alertdialog").getByText("确认显式修复 RoE 模式？", { exact: true }).waitFor();
    assert.equal(modeWrites().length, beforeUnknown);
    await page.getByRole("button", { name: "确认仅修复 RoE", exact: true }).click();
    await assertCurrent("strict");
    assert.deepEqual(modeWrites().at(-1).body, { value: "strict" });
    assert.equal(settings.finding_verify_mode, "fixture-invalid-verifier", "Repairing RoE altered an invalid peer");
    assert.deepEqual(settings, { ...beforeCorruption, roe_enforcement: "strict", finding_verify_mode: "fixture-invalid-verifier" });
    await page.getByRole("textbox", { name: "搜索系统设置", exact: true }).fill("finding_verify_mode");
    await verify.getByText("当前自动验证模式未知；未选择默认值", { exact: true }).waitFor();
    const beforeVerifyRepair = verifyWrites().length;
    await verify.getByRole("combobox").click();
    await page.getByRole("option", { name: /^off ·/ }).click();
    await verify.getByRole("button", { name: "保存自动验证模式", exact: true }).click();
    await page.getByRole("alertdialog").getByText("确认自动验证模式变更？", { exact: true }).waitFor();
    assert.equal(verifyWrites().length, beforeVerifyRepair);
    await page.getByRole("button", { name: "确认仅保存验证模式", exact: true }).click();
    await verify.getByText(/^最近确认的自动验证模式：off ·/).waitFor();
    assert.deepEqual(verifyWrites().at(-1).body, { value: "off" });
    assert.deepEqual(settings, { ...beforeCorruption, roe_enforcement: "strict" });
    await page.getByRole("textbox", { name: "搜索系统设置", exact: true }).fill("llm_records_retention_days");
    assert.equal(await retention.getByRole("spinbutton", { name: "LLM 调用记录保留天数", exact: true }).inputValue(), "45", "Repairing invalid peers erased an unrelated unsaved retention draft");
    assert.equal(settings.llm_records_retention_days, 60, "Peer repair silently saved another draft");
    console.log("PASS two invalid peers can be repaired independently without guessed defaults or unrelated mutations");
    const beforeNavigation = modeWrites().length;
    await page.goto(origin + "/system/agents/", { waitUntil: "networkidle" });
    await gotoSettings(); await assertCurrent("strict");
    assert.equal(modeWrites().length, beforeNavigation);
    assert.equal(settings.roe_enforcement, "strict");
    assert.equal(settings.finding_verify_mode, "off");
    assert.equal(settings.llm_records_retention_days, 60);
    console.log("PASS navigation and reload preserve the saved mode without automatic downgrade");
    assert.deepEqual(errors, [], "Browser exceptions");
    assert.deepEqual([...unexpected], [], "Unexpected mock API calls");
    assert.deepEqual([...external], [], "External request attempted");
  } catch (e) {
    console.error(JSON.stringify({ route: page.url(), requests: requests.slice(-20), unexpected: [...unexpected], body: (await page.locator("body").innerText()).slice(0, 6500) }, null, 2));
    throw e;
  } finally { await context.close(); }
}
main().catch((e) => { console.error(e); process.exitCode = 1; }).finally(async () => {
  if (browser) await browser.close();
  if (server.listening) await new Promise((resolve) => server.close(resolve));
});
