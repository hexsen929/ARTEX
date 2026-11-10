#!/usr/bin/env node
// Cloud-only UI verification using synthetic records; no model or external target.
"use strict";
const assert = require("node:assert/strict");
const fs = require("node:fs");
const path = require("node:path");
const http = require("node:http");
const { once } = require("node:events");
if (!process.env.CI || !process.env.ARTEX_PLAYWRIGHT_DIR) throw new Error("Use the cloud browser runtime");
const { chromium } = require(path.resolve(process.env.ARTEX_PLAYWRIGHT_DIR));
const output = path.resolve(__dirname, "..", "out");
const requests = [], unexpected = new Set(), external = new Set();
const docs = [];
const templates = [];
let heldSearch, staleEdit = false;
const config = { enabled: false, base_url: "", model: "", dimension: 1024, has_key: false, epoch: 1, vector_available: false };
const task = { id: "101", name: "REFERENCE_TASK", description: "Synthetic task", status: "done" };
function json(res, value, code = 200) { if (res.destroyed) return; res.writeHead(code, { "Content-Type": "application/json" }); res.end(JSON.stringify(value)); }
async function api(req, res, url) {
  let raw = ""; for await (const chunk of req) raw += chunk; const body = raw ? JSON.parse(raw) : {};
  const route = url.pathname; requests.push({ method: req.method, route, body, query: Object.fromEntries(url.searchParams) });
  if (route === "/api/auth/status") return json(res, { initialized: true });
  if (route === "/api/version") return json(res, { version: "fixture" });
  if (route === "/api/update/check") return json(res, { has_update: false, current: "fixture" });
  if (route === "/api/settings" || route === "/api/settings/egress") return json(res, { show_egress_ip: false, global_proxy: "" });
  if (route === "/api/intercept/pending") return json(res, { pending: [] });
  if (route === "/api/agents") return json(res, { agents: [] });
  if (route === "/api/skills/missing") return json(res, { missing: [] });
  if (route === "/api/tasks") return json(res, { tasks: [task], active: "" });
  if (route === "/api/knowledge/config") return json(res, { config, pdf_available: false });
  if (route === "/api/knowledge/search") {
    if (body.query === "hold-old-scope") { heldSearch = () => json(res, { mode: "keyword", notice: "fixture", hits: [{ doc_id: 99, version: 1, chunk: 0, title: "STALE_SCOPE_RESULT", content: "must not appear", task_id: 0, score: 1 }] }); return; }
    return json(res, { mode: "keyword", notice: "Synthetic reference only", hits: docs.filter((d) => d.task_id === body.task_id || d.task_id === 0).map((d) => ({ doc_id: d.id, version: d.version, chunk: 0, title: d.title, content: d.content, task_id: d.task_id, score: 1 })) });
  }
  if (route === "/api/knowledge") {
    if (req.method === "POST") { const doc = { ...body, id: docs.length + 1, version: 1, bytes: body.content.length, chunks: 1, indexed: false }; docs.push(doc); return json(res, doc); }
    return json(res, { items: docs.filter((d) => d.task_id === 0 || d.task_id === Number(url.searchParams.get("task_id"))) });
  }
  if (route === "/api/knowledge/1") {
    if (req.method === "PUT") { if (staleEdit) return json(res, { error: "资料已变化，请刷新后重试" }, 409); Object.assign(docs[0], body, { version: docs[0].version + 1 }); }
    return json(res, docs[0]);
  }
  if (route === "/api/report-templates") {
    if (req.method === "POST") { const template = { ...body, id: 1, version: 1 }; templates.push(template); return json(res, template); }
    return json(res, { items: templates });
  }
  if (route === "/api/exploration/findings") {
    assert.equal(url.searchParams.get("task_id"), "101");
    const finding = { id: "501", finding_id: "501", name: "VERIFIED_FIXTURE", vulnclass: "Synthetic", verification_status: "confirmed", severity: "low", evidence_version: 4 };
    return json(res, { items: [finding], total: 1 });
  }
  if (route === "/api/reports/docx") {
    assert.deepEqual(body.findings, [{ id: 501, evidence_version: 4 }]);
    assert.equal(body.include_unverified, false); assert.equal(body.template_version, 1);
    res.writeHead(200, { "Content-Type": "application/vnd.openxmlformats-officedocument.wordprocessingml.document", "Content-Disposition": "attachment; filename=fixture.docx" }); return res.end("PK_SYNTHETIC_DOWNLOAD");
  }
  unexpected.add(`${req.method} ${route}`); json(res, { error: "Unmocked route" }, 404);
}
const mime = { ".html": "text/html", ".js": "text/javascript", ".css": "text/css", ".json": "application/json", ".svg": "image/svg+xml", ".png": "image/png", ".woff2": "font/woff2", ".ico": "image/x-icon" };
const server = http.createServer(async (req, res) => { try { const url = new URL(req.url, "http://127.0.0.1"); if (url.pathname.startsWith("/api/")) return await api(req, res, url); let file = path.resolve(output, "." + decodeURIComponent(url.pathname)); if (!file.startsWith(output + path.sep)) return res.end(); if (fs.existsSync(file) && fs.statSync(file).isDirectory()) file = path.join(file, "index.html"); if (!fs.existsSync(file)) { res.writeHead(404); return res.end(); } res.writeHead(200, { "Content-Type": mime[path.extname(file)] || "text/plain" }); fs.createReadStream(file).pipe(res); } catch (error) { json(res, { error: error.message }, 500); } });
async function eventually(fn, message) { for (let i = 0; i < 200; i++) { if (await fn()) return; await new Promise((r) => setTimeout(r, 50)); } assert.fail(message); }
let browser, page;
async function main() {
  server.listen(0, "127.0.0.1"); await once(server, "listening"); const origin = `http://127.0.0.1:${server.address().port}`;
  browser = await chromium.launch({ headless: true }); const context = await browser.newContext({ viewport: { width: 1500, height: 1000 }, acceptDownloads: true });
  await context.route("**/*", (route) => { const u = new URL(route.request().url()); if (u.origin === origin || ["data:", "blob:"].includes(u.protocol)) return route.continue(); external.add(u.origin); return route.abort(); });
  const token = [Buffer.from('{"alg":"none"}').toString("base64url"), Buffer.from('{"sub":"fixture","exp":4102444800}').toString("base64url"), "fixture"].join(".");
  await context.addInitScript((token) => { localStorage.setItem("artex_token", token); document.cookie = "artex_session=1; path=/"; document.cookie = "sidebar_state=false; path=/"; }, token);
  page = await context.newPage(); const errors = []; page.on("pageerror", (e) => errors.push(e.message)); page.setDefaultTimeout(15000);
  await page.goto(origin + "/function/knowledge/", { waitUntil: "networkidle" });
  await page.getByRole("heading", { name: "文档知识库", exact: true }).waitFor();
  await page.getByRole("button", { name: "新建资料", exact: true }).click();
  await page.getByLabel("资料标题", { exact: true }).fill("REFERENCE_GUIDE"); await page.getByLabel("资料内容", { exact: true }).fill("Synthetic reusable API testing reference");
  await page.getByRole("button", { name: "保存资料", exact: true }).click(); await eventually(() => docs.length === 1, "reference not saved");
  assert.equal(docs[0].task_id, 0); await page.getByRole("button", { name: "返回检索", exact: true }).click();
  await page.getByLabel("检索主题", { exact: true }).fill("API"); await page.getByRole("button", { name: "检索", exact: true }).click(); await page.getByText("文档 #1 · 版本 1 · 分块 1 · 全局", { exact: true }).waitFor();
  assert.ok(await page.getByRole("button", { name: "建立语义索引", exact: true }).isDisabled());
  staleEdit = true; await page.getByRole("button", { name: "查看 / 编辑", exact: true }).click(); await page.getByLabel("资料标题", { exact: true }).fill("KEEP_UNSAVED_DRAFT"); await page.getByRole("button", { name: "保存资料", exact: true }).click(); await page.getByRole("alert").getByText("资料已变化，请刷新后重试", { exact: true }).waitFor(); assert.equal(await page.getByLabel("资料标题", { exact: true }).inputValue(), "KEEP_UNSAVED_DRAFT");
  await page.getByRole("button", { name: "返回检索", exact: true }).click(); await page.getByLabel("检索主题", { exact: true }).fill("hold-old-scope"); await page.getByRole("button", { name: "检索", exact: true }).click(); await eventually(() => heldSearch, "delayed query missing"); await page.getByLabel("资料范围", { exact: true }).selectOption("101"); heldSearch(); await page.waitForTimeout(250); assert.equal(await page.getByText("STALE_SCOPE_RESULT", { exact: true }).count(), 0);
  console.log("PASS scoped reference UI keeps stale drafts, aborts old searches and exposes index availability");
  await page.goto(origin + "/function/reports/", { waitUntil: "networkidle" }); await page.getByLabel("模板名称", { exact: true }).fill("REPORT_FIXTURE"); await page.getByLabel("出具单位", { exact: true }).fill("Example team"); await page.getByRole("button", { name: "保存模板", exact: true }).click(); await eventually(() => templates.length === 1, "template not saved"); await page.getByLabel("报告来源任务", { exact: true }).selectOption("101"); await page.getByText("VERIFIED_FIXTURE", { exact: true }).waitFor();
  assert.equal(requests.filter((r) => r.route === "/api/exploration/findings").at(-1).query.verification, "confirmed"); await page.getByRole("button", { name: "选择本页全部", exact: true }).click();
  const download = page.waitForEvent("download"); await page.getByRole("button", { name: "生成并下载 Word（1 条）", exact: true }).click(); assert.equal((await download).suggestedFilename(), "fixture.docx");
  assert.deepEqual(errors, []); assert.deepEqual([...unexpected], []); assert.deepEqual([...external], []);
  console.log("PASS Word template and selected verified findings send versioned export without mutation");
  await context.close();
}
main().catch(async (e) => { console.error(e); if (page && !page.isClosed()) console.error((await page.locator("body").innerText()).slice(0, 3000)); process.exitCode = 1; }).finally(async () => { heldSearch?.(); await browser?.close(); server.closeAllConnections?.(); await new Promise((r) => server.close(r)); });
