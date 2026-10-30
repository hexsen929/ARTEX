#!/usr/bin/env node
/* Cloud-only synthetic memory-management regression. No real ARTEX, model,
 * task database, credentials or external target is used. */
"use strict";
const assert = require("node:assert/strict");
const fs = require("node:fs");
const http = require("node:http");
const path = require("node:path");
const { once } = require("node:events");
if (!process.env.CI || !process.env.ARTEX_PLAYWRIGHT_DIR) throw new Error("Run only in GitHub Actions with its temporary browser runtime.");
const { chromium } = require(path.resolve(process.env.ARTEX_PLAYWRIGHT_DIR));
const output = path.resolve(__dirname, "..", "out");
if (!fs.existsSync(path.join(output, "function", "memories", "index.html"))) throw new Error("Missing cloud memory-page export");
const settings = {
  enabled: true, auto_extract: true, global_share: true, profile_id: 0,
  max_entries: 5, max_tokens: 2000, max_jobs_per_hour: 12, retention_days: 90,
  epoch: 1, learn_after: "2026-10-30T04:00:00Z", model_available: true,
};
const fixture = {
  id: 7, title: "合成工具兼容经验", content: "工具版本更新后，应重新核对参数支持情况。",
  kind: "environment", status: "active", confidence: "observed", origin_task_id: 24,
  origin_conversation_id: 0, agent_key: "worker", target: "", applicability: "仅适用于来源所述版本",
  tags: ["版本"], source_kind: "fact", source_id: "31", source_version: 2, version: 1,
  created_at: "2026-10-30T04:00:00Z", updated_at: "2026-10-30T04:00:00Z", expires_at: null,
  last_used_at: null, use_count: 0, automatic: true,
};
let entries = [{ ...fixture }], nextId = 8;
const state = { failList: false, conflictEdit: false, conflictSettings: false, failUsage: false };
const requests = [], unexpected = new Set(), external = new Set(), errors = [];
const json = (response, status, value) => { response.writeHead(status, { "Content-Type": "application/json", "Cache-Control": "no-store" }); response.end(JSON.stringify(value)); };
const pause = (ms) => new Promise((resolve) => setTimeout(resolve, ms));
async function api(request, response, url) {
  const chunks = []; for await (const chunk of request) chunks.push(chunk);
  const raw = Buffer.concat(chunks).toString(), body = raw ? JSON.parse(raw) : {};
  const endpoint = url.pathname, method = request.method || "GET";
  requests.push({ endpoint, method, body, query: url.searchParams.toString() });
  if (endpoint === "/api/memories/settings") {
    if (method === "GET") return json(response, 200, settings);
    if (method === "PUT") {
      for (const key of ["model_available", "model_error", "learn_after"]) assert.equal(Object.hasOwn(body, key), false, `Readonly settings field ${key} was echoed`);
      if (state.conflictSettings) { state.conflictSettings = false; settings.epoch += 1; settings.global_share = false; return json(response, 409, { error: "记忆或来源已变化，请刷新后重试" }); }
      assert.equal(body.epoch, settings.epoch);
      Object.assign(settings, body, { epoch: settings.epoch + 1 });
      return json(response, 200, settings);
    }
  }
  if (endpoint === "/api/memories/usage") return state.failUsage
    ? json(response, 503, { error: "fixture usage unavailable" })
    : json(response, 200, { items: [{ id: 1, entry_id: 7, entry_version: 1, task_id: 24, conversation_id: 0, session_id: "intent:31", agent_key: "worker", result: "injected", created_at: "2026-10-30T05:00:00Z" }] });
  if (endpoint === "/api/memories/extract") return json(response, 202, { queued: 2 });
  if (endpoint === "/api/memories") {
    if (method === "GET") {
      if (state.failList) return json(response, 503, { error: "fixture list unavailable" });
      const q = url.searchParams.get("q"), status = url.searchParams.get("status"), kind = url.searchParams.get("kind"), task = Number(url.searchParams.get("task_id") || 0);
      let result = entries.filter((item) => (!q || `${item.title} ${item.content} ${item.tags.join(" ")}`.includes(q)) && (!status || item.status === status) && (!kind || item.kind === kind) && (!task || item.origin_task_id === task));
      const total = result.length, offset = Number(url.searchParams.get("offset") || 0), limit = Number(url.searchParams.get("limit") || 20);
      result = result.slice(offset, offset + limit); return json(response, 200, { items: result, total });
    }
    if (method === "POST") {
      assert.equal(Object.hasOwn(body, "status"), false, "Manual creation attempted an approval state");
      await pause(100);
      const entry = { ...fixture, ...body, id: nextId++, status: "active", confidence: "unverified", automatic: false, origin_task_id: 0, source_kind: "manual", source_id: "", version: 1 };
      entries.unshift(entry); return json(response, 201, entry);
    }
  }
  const match = endpoint.match(/^\/api\/memories\/(\d+)$/);
  if (match) {
    const id = Number(match[1]), entry = entries.find((item) => item.id === id);
    if (!entry) return json(response, 404, { error: "记忆或来源不存在" });
    if (method === "GET") return json(response, 200, entry);
    if (method === "PATCH") {
      if (state.conflictEdit) { state.conflictEdit = false; entry.content = "另一操作已更新的最新内容"; entry.version += 1; return json(response, 409, { error: "记忆或来源已变化，请刷新后重试" }); }
      assert.equal(body.version, entry.version); Object.assign(entry, body, { version: entry.version + 1 }); return json(response, 200, entry);
    }
    if (method === "DELETE") {
      assert.equal(body.version, entry.version); await pause(100); entries = entries.filter((item) => item.id !== id); return json(response, 200, { deleted: true });
    }
  }
  if (endpoint === "/api/auth/status") return json(response, 200, { initialized: true });
  if (endpoint === "/api/settings" && method === "GET") return json(response, 200, { show_egress_ip: false, global_proxy: "" });
  if (endpoint === "/api/update/check") return json(response, 200, { current: "memory-fixture", mode: "binary", os: "darwin", arch: "arm64", repo: "fixture", has_backup: false, has_update: false, comparable: false });
  if (endpoint === "/api/health") return json(response, 200, { ok: true, service: "fixture", version: "memory-fixture" });
  if (endpoint === "/api/tasks") return json(response, 200, { tasks: [], active: "" });
  if (endpoint === "/api/agents") return json(response, 200, { agents: [] });
  if (endpoint === "/api/llm/profiles") return json(response, 200, { profiles: [] });
  if (endpoint === "/api/intercept/pending") return json(response, 200, { pending: [] });
  if (endpoint === "/api/skills/missing") return json(response, 200, { missing: [] });
  unexpected.add(`${method} ${endpoint}`); return json(response, 404, { error: "Unmocked synthetic endpoint" });
}
const mime = { ".html": "text/html", ".js": "text/javascript", ".css": "text/css", ".json": "application/json", ".svg": "image/svg+xml", ".png": "image/png", ".woff2": "font/woff2", ".ico": "image/x-icon", ".txt": "text/plain" };
const server = http.createServer(async (request, response) => {
  try {
    const url = new URL(request.url || "/", "http://127.0.0.1");
    if (url.pathname.startsWith("/api/")) return await api(request, response, url);
    let file = path.resolve(output, "." + decodeURIComponent(url.pathname));
    if (file !== output && !file.startsWith(output + path.sep)) { response.writeHead(403); return response.end(); }
    if (fs.existsSync(file) && fs.statSync(file).isDirectory()) file = path.join(file, "index.html");
    if (!fs.existsSync(file) || !fs.statSync(file).isFile()) { response.writeHead(404); return response.end("Fixture file missing"); }
    response.writeHead(200, { "Content-Type": mime[path.extname(file)] || "application/octet-stream", "Cache-Control": "no-store" }); fs.createReadStream(file).pipe(response);
  } catch (error) { unexpected.add(`fixture error: ${error.message}`); json(response, 500, { error: "Synthetic fixture failed" }); }
});
async function eventually(check, message) { const deadline = Date.now() + 15000; while (Date.now() < deadline) { if (await check()) return; await pause(40); } assert.fail(message); }
let browser;
async function main() {
  server.listen(0, "127.0.0.1"); await once(server, "listening");
  const origin = `http://127.0.0.1:${server.address().port}`;
  browser = await chromium.launch({ headless: true });
  const context = await browser.newContext({ viewport: { width: 1500, height: 1100 }, locale: "zh-CN", timezoneId: "Asia/Shanghai" });
  const page = await context.newPage(); page.setDefaultTimeout(15000); page.on("pageerror", (error) => errors.push(error.message));
  await context.route("**/*", (route) => { const url = new URL(route.request().url()); if (url.origin === origin || ["data:", "blob:"].includes(url.protocol)) return route.continue(); external.add(url.origin); return route.abort("blockedbyclient"); });
  const token = [Buffer.from('{"alg":"none"}').toString("base64url"), Buffer.from('{"sub":"fixture-admin","exp":4102444800}').toString("base64url"), "fixture"].join(".");
  await page.addInitScript(({ token }) => { localStorage.setItem("artex_token", token); document.cookie = "artex_session=1; path=/; SameSite=Lax"; document.cookie = "sidebar_state=false; path=/; SameSite=Lax"; }, { token });
  const writes = (method, endpoint) => requests.filter((item) => item.method === method && item.endpoint === endpoint);
  try {
    await page.goto(origin + "/function/memories/", { waitUntil: "networkidle" });
    await page.getByRole("heading", { name: "记忆中心", exact: true }).waitFor();
    await page.getByRole("button", { name: fixture.title, exact: true }).waitFor();
    const sourceHref = await page.getByRole("link", { name: "任务 #24", exact: true }).getAttribute("href");
    assert.equal(typeof sourceHref, "string");
    const sourceURL = new URL(sourceHref, origin);
    // Next static export canonicalizes internal links with trailingSlash. The
    // destination and task identity matter, not the optional terminal slash.
    assert.equal(sourceURL.origin, origin);
    assert.equal(sourceURL.pathname.replace(/\/+$/, ""), "/function/tasks/detail");
    assert.equal(sourceURL.searchParams.get("id"), "24");
    assert.deepEqual([...sourceURL.searchParams.keys()], ["id"]);
    assert.equal(requests.filter((item) => ["POST", "PATCH", "DELETE", "PUT"].includes(item.method)).length, 0);
    console.log("PASS memory list loads with source ownership and no approval/default writes");

    await page.getByRole("button", { name: "记住这个", exact: true }).click();
    await page.getByLabel("标题", { exact: true }).fill("人工合成排错经验");
    await page.getByLabel("记忆正文", { exact: true }).fill("仅在匹配版本和适用条件时复用历史排错结果。");
    await page.getByRole("button", { name: "保存记忆", exact: true }).click();
    await page.getByRole("button", { name: "人工合成排错经验", exact: true }).waitFor();
    assert.equal(writes("POST", "/api/memories").length, 1); console.log("PASS manual memory activates directly without approval");

    state.conflictEdit = true;
    await page.getByRole("button", { name: "编辑记忆 7", exact: true }).click();
    await page.getByLabel("记忆正文", { exact: true }).fill("即将提交的旧版本草稿");
    await page.getByRole("button", { name: "保存记忆", exact: true }).click();
    await page.getByText("条目已被其他操作更新，本次未覆盖。请重新载入最新条目后再编辑。", { exact: true }).waitFor();
    assert.equal(await page.getByRole("button", { name: "保存记忆", exact: true }).isDisabled(), true);
    await page.getByRole("button", { name: "重新载入最新条目", exact: true }).click();
    await eventually(async () => await page.getByLabel("记忆正文", { exact: true }).inputValue() === "另一操作已更新的最新内容", "Latest conflicted entry was not reloaded");
    await page.getByLabel("记忆正文", { exact: true }).fill("重新核对最新版本后的经验");
    await page.getByRole("button", { name: "保存记忆", exact: true }).click();
    await page.getByText("重新核对最新版本后的经验", { exact: true }).waitFor();
    assert.equal(writes("PATCH", "/api/memories/7").at(-1).body.version, 2); console.log("PASS edit conflict blocks overwrite and requires explicit latest-version reload");

    await page.getByRole("tab", { name: "记忆设置", exact: true }).click();
    await page.getByLabel("所有任务共享", { exact: true }).waitFor();
    state.conflictSettings = true;
    await page.getByLabel("所有任务共享", { exact: true }).click();
    await page.getByRole("button", { name: "保存记忆设置", exact: true }).click();
    await page.getByText("记忆设置已发生变化，本次未覆盖。请重新读取最新设置后再修改。", { exact: true }).waitFor();
    assert.equal(await page.getByRole("button", { name: "保存记忆设置", exact: true }).isDisabled(), true);
    await page.getByRole("button", { name: "重新读取最新设置", exact: true }).click();
    await eventually(async () => await page.getByLabel("所有任务共享", { exact: true }).getAttribute("aria-checked") === "false", "Conflicted settings did not reload");
    await page.getByLabel("所有任务共享", { exact: true }).click();
    await page.getByRole("button", { name: "保存记忆设置", exact: true }).click();
    await eventually(() => settings.global_share === true && settings.epoch === 3, "Settings save did not use the latest epoch");
    console.log("PASS settings epoch conflict locks stale save and read-only metadata is excluded");

    await page.getByRole("tab", { name: "记忆条目", exact: true }).click();
    await page.getByRole("button", { name: "删除记忆 8", exact: true }).click();
    await page.getByRole("alertdialog").getByText("删除这条记忆？", { exact: true }).waitFor();
    assert.equal(writes("DELETE", "/api/memories/8").length, 0);
    await page.getByRole("button", { name: "取消", exact: true }).click();
    assert.equal(writes("DELETE", "/api/memories/8").length, 0);
    await page.getByRole("button", { name: "删除记忆 8", exact: true }).click();
    await page.getByRole("button", { name: "确认删除记忆", exact: true }).click();
    await eventually(() => !entries.some((item) => item.id === 8), "Memory deletion was not confirmed");
    assert.equal(writes("DELETE", "/api/memories/8").length, 1); console.log("PASS deletion is explicitly confirmed once and describes historical retention");

    state.failList = true;
    await page.getByRole("button", { name: "刷新记忆列表", exact: true }).click();
    await page.getByText(/记忆列表读取失败：fixture list unavailable/).waitFor();
    await page.getByText("数量未确认", { exact: true }).waitFor();
    assert.equal(await page.getByText("暂无符合条件的记忆。", { exact: true }).count(), 0);
    state.failList = false; entries[0].status = "source_invalid";
    await page.getByRole("button", { name: "重试读取", exact: true }).click();
    await page.getByText("来源已失效", { exact: true }).waitFor();
    assert.equal(await page.getByRole("link", { name: "任务 #24", exact: true }).count(), 0);
    console.log("PASS failed reads are not empty data and invalid sources lose their links");

    state.failUsage = true;
    await page.getByRole("tab", { name: "使用记录", exact: true }).click();
    await page.getByText(/使用记录读取失败：fixture usage unavailable/).waitFor();
    state.failUsage = false;
    await page.getByRole("button", { name: "刷新使用记录", exact: true }).click();
    await page.getByText("已注入", { exact: true }).waitFor();
    console.log("PASS usage metadata has independent error and retry states");

    settings.model_available = false; settings.model_error = "合成模型未配置";
    await page.reload({ waitUntil: "networkidle" });
    await page.getByText(/自动提炼不可用：合成模型未配置/).waitFor();
    assert.equal(await page.getByRole("button", { name: "主动回顾历史", exact: true }).isDisabled(), true);
    assert.deepEqual([...unexpected], []); assert.deepEqual([...external], []); assert.deepEqual(errors, []);
    console.log("PASS missing models are unavailable, with zero external requests and page errors");
    const artifacts = process.env.ARTEX_UI_ARTIFACTS;
    if (artifacts) { fs.mkdirSync(artifacts, { recursive: true }); await page.screenshot({ path: path.join(artifacts, "memory-center.png"), fullPage: true }); }
  } finally { await context.close(); }
}
main().catch((error) => { console.error(error); process.exitCode = 1; }).finally(async () => { if (browser) await browser.close(); await new Promise((resolve) => server.close(resolve)); });
