#!/usr/bin/env node
/* Cloud-only, synthetic findings UI regression. No ARTEX process, database,
 * real finding, credential, external request or system clipboard is used. */
"use strict";
const assert = require("node:assert/strict");
const fs = require("node:fs");
const http = require("node:http");
const path = require("node:path");
const { once } = require("node:events");
if (!process.env.CI || !process.env.ARTEX_PLAYWRIGHT_DIR) {
  throw new Error("Run this browser regression in GitHub Actions with its temporary Playwright runtime.");
}
const { chromium } = require(path.resolve(process.env.ARTEX_PLAYWRIGHT_DIR));
const output = path.resolve(__dirname, "..", "out");
const taskName = "合成发现回归任务";
const stamp = "2026-10-29T04:00:00Z";
const screenshotMarkdown = "![UI_FIXTURE_SCREENSHOT](workspace://findings/screenshots/legacy-synthetic.png)";
const report = "## 复现步骤\n\n### 步骤 1：核对合成证据\n\n本步骤仅展示已保存的合成内容，不执行任何请求。\n\n## 最终结果\n\n待人工核对。\n\n" + screenshotMarkdown;
const findings = ["unverified", "confirmed"].map((verification_status, index) => ({
  id: String(501 + index), finding_id: String(501 + index), task_id: "101",
  task_name: taskName, task_description: taskName, ts: stamp, severity: "high",
  status: "pending", verification_status, verification_note: "UI_FIXTURE_ONLY",
  vulnclass: "Synthetic", name: `UI_FIXTURE_${verification_status.toUpperCase()}`,
  summary: "合成发现；不含真实漏洞、目标或密钥。", evidence: "UI_FIXTURE_EVIDENCE",
  report, report_conversation_id: 999, evidence_version: 1, assets: [], traffic_count: 1, inherited: false,
}));
const requests = [], unexpected = new Set(), external = new Set(), sideItems = [];
// These are saved synthetic packets, never an instruction to contact fixture.invalid.
// Keep the original query unique; the second double marker deliberately ends the packet.
const packetRequestBody = "FIXTURE_PACKET_MATCH\nsecond line\nFIXTURE_DOUBLE\nliteral .*[x](a)\nFIXTURE_DOUBLE";
const binding = {
  id: "701", finding_id: "501", snapshot_id: "801", role: "proof", note: "UI_FIXTURE_BOUND_PACKET", position: 0, created_at: stamp,
  snapshot: { id: "801", source_traffic_id: "601", captured_at: 1793246400,
    url: "https://fixture.invalid/demo", method: "POST", status: 200, content_type: "text/plain",
    req_head: "POST /demo HTTP/1.1\nHost: fixture.invalid\nContent-Type: text/plain", resp_head: "HTTP/1.1 200 OK\nContent-Type: text/plain",
    req_hash: "fixture-request-hash", resp_hash: "fixture-response-hash", req_len: Buffer.byteLength(packetRequestBody), resp_len: 19 },
};
const packet = {
  binding,
  request: { content: packetRequestBody, offset: 0, total: Buffer.byteLength(packetRequestBody), next_offset: Buffer.byteLength(packetRequestBody), truncated: false, binary: false },
  response: { content: "UI_FIXTURE_RESPONSE", offset: 0, total: 19, next_offset: 19, truncated: false, binary: false },
};
const reportItems = [{ seq: 1, worker: "reporter", ts: stamp, kind: "text", summary: "UI_FIXTURE_EXISTING_REPORT_HISTORY", detail: "UI_FIXTURE_EXISTING_REPORT_HISTORY\n\n" + screenshotMarkdown }];
let reportResolves = 0;
let reportSends = 0;
function json(response, code, value) {
  response.writeHead(code, { "Content-Type": "application/json", "Cache-Control": "no-store" });
  response.end(JSON.stringify(value));
}
function selected(url) {
  const verification = url.searchParams.get("verification");
  const task = url.searchParams.get("task_id");
  // Date matching is covered by the DB/mock contract tests. This fixture tests
  // that every UI view forwards the same selected date+verification parameters.
  return findings.filter((f) => (!verification || f.verification_status === verification) && (!task || f.task_id === task));
}
function pageValue(url, items, extra = {}) {
  return { items, total: items.length, page: Number(url.searchParams.get("page") || 1), page_size: Number(url.searchParams.get("limit") || 20), ...extra };
}
async function serveAPI(request, response, url) {
  const endpoint = url.pathname, method = request.method || "GET";
  requests.push({ method, endpoint, query: Object.fromEntries(url.searchParams) });
  if (endpoint === "/api/workspace/preview") {
    assert.equal(method, "GET");
    assert.equal(url.searchParams.get("path"), "findings/screenshots/legacy-synthetic.png");
    assert.equal(url.searchParams.get("finding_id"), "501", "Screenshot preview lost its real finding identity");
    const png = Buffer.from("iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR4nGP4z8DwHwAFAAH/iZk9HQAAAABJRU5ErkJggg==", "base64");
    response.writeHead(200, { "Content-Type": "image/png", "Content-Length": png.length, "Cache-Control": "no-store", "X-Report-Screenshot-Timestamp": stamp });
    return response.end(png);
  }
  if (endpoint === "/api/version") return json(response, 200, { ok: true, service: "synthetic", version: "fixture" });
  if (endpoint === "/api/health") return json(response, 200, { ok: true, service: "synthetic", version: "fixture" });
  if (endpoint === "/api/auth/status") return json(response, 200, { initialized: true });
  if (endpoint === "/api/update/check") return json(response, 200, { has_update: false, current: "fixture" });
  if (endpoint === "/api/intercept/pending") return json(response, 200, { pending: [] });
  if (endpoint === "/api/settings/egress") return json(response, 200, { show_egress_ip: false, global_proxy: "" });
  if (endpoint === "/api/settings") return json(response, 200, { show_egress_ip: false, traffic_capture: false });
  if (endpoint === "/api/tasks") return json(response, 200, { tasks: [{ id: "101", name: taskName, description: taskName, status: "done" }], active: "" });
  if (endpoint === "/api/agents") return json(response, 200, { agents: [] });
  if (endpoint === "/api/skills/missing") return json(response, 200, { missing: [] });
  if (endpoint === "/api/exploration/findings/retests/active") return json(response, 200, { retests: [] });
  if (endpoint === "/api/exploration/findings/stats") {
    const matches = selected(url);
    return json(response, 200, {
      total: matches.length, pending: matches.length, critical: 0, high: matches.length, medium: 0, low: 0,
      verified: matches.filter((f) => f.verification_status === "confirmed").length,
      unverified: matches.filter((f) => f.verification_status === "unverified").length,
      distinct: { total: matches.length, reports: matches.length },
      verification_blocked: 0, rejected: 0, legacy: 0, vulnclasses: ["Synthetic"],
      tasks: [{ id: "101", name: taskName, description: taskName, count: 2 }],
    });
  }
  if (endpoint === "/api/exploration/finding-case-suggestions" || endpoint === "/api/exploration/finding-case-review") return json(response, 200, []);
  if (endpoint === "/api/exploration/finding-cases") {
    return json(response, 200, pageValue(url, selected(url).map((finding) => ({ finding, matched_ids: [Number(finding.id)] })), {
      matching_reports: selected(url).length, stats: { total: selected(url).length, critical: 0, high: selected(url).length, medium: 0, low: 0 },
    }));
  }
  if (endpoint === "/api/exploration/findings/groups") {
    const matches = selected(url);
    return json(response, 200, pageValue(url, matches.length ? [{ task_id: "101", task_name: taskName, task_description: taskName, task_status: "done", count: matches.length, critical: 0, high: matches.length, medium: 0, low: 0, last_found_at: stamp }] : [], { finding_total: matches.length }));
  }
  if (endpoint === "/api/exploration/findings/asset-tree") return json(response, 200, { nodes: [], finding_total: selected(url).length, truncated: false, dropped_kinds: [] });
  if (endpoint === "/api/tasks/101/chat/side-questions") {
    assert.equal(url.searchParams.get("agent"), "auto");
    assert.equal(url.searchParams.get("finding_id"), "501");
    const model = { model: "fixture-readonly", name: "Synthetic", format: "openai", profile_id: 1 };
    if (method === "POST") {
      let raw = "";
      for await (const chunk of request) { raw += chunk; assert.ok(raw.length < 8192); }
      const body = JSON.parse(raw);
      const item = { id: "fixture-side-1", ordinal: 1, client_request_id: body.client_request_id, question: body.question, answer: "UI_FIXTURE_READ_ONLY_ANSWER", status: "completed", model, snapshot_at: stamp, created_at: stamp, sequence: 1 };
      sideItems.push(item);
      return json(response, 200, item);
    }
    return json(response, 200, { items: sideItems, current: null, next_cursor: 0, snapshot: { captured_at: stamp, model, available: true, reason: "" } });
  }
  if (endpoint === "/api/findings/501/inline-history") return json(response, 200, { events: [{ id: "1", finding_id: "501", kind: "original", created_at: stamp, payload: { name: "UI_FIXTURE_HISTORY", summary: "Synthetic audit snapshot" } }] });
  if (endpoint === "/api/findings/503/inline-history") return json(response, 200, { events: [] });
  if (endpoint === "/api/exploration/findings/501/retests") return json(response, 200, { retests: [] });
  if (endpoint === "/api/exploration/findings/503/retests") return json(response, 200, { retests: [] });
  if (endpoint === "/api/exploration/findings/501/traffic") return json(response, 200, { finding_id: "501", version: 1, report_version: 1, bindings: [binding] });
  if (endpoint === "/api/exploration/findings/501/traffic/701") return json(response, 200, packet);
  if (endpoint === "/api/exploration/findings/501/report-conversation" && method === "POST") {
    reportResolves += 1;
    // Failure is visible and must not turn into an unbound generic conversation.
    if (reportResolves === 1) return json(response, 503, { error: "UI_FIXTURE_REPORT_RESOLVE_FAILURE" });
    return json(response, 200, { conversation_id: 901, task_id: 101, finding_id: 501 });
  }
  if (endpoint === "/api/conversations/901/messages" && method === "POST") {
    let raw = "";
    for await (const chunk of request) { raw += chunk; assert.ok(raw.length < 8192); }
    const body = JSON.parse(raw);
    assert.equal(body.message, "仅整理现有证据，不增加测试结论");
    reportSends += 1;
    reportItems.push(
      { seq: 2, worker: "reporter", ts: stamp, kind: "user", summary: body.message, detail: body.message },
      { seq: 3, worker: "reporter", ts: stamp, kind: "text", summary: "UI_FIXTURE_REPORT_EDIT_ANSWER", detail: "UI_FIXTURE_REPORT_EDIT_ANSWER" },
    );
    return json(response, 200, { status: "started" });
  }
  if (endpoint === "/api/conversations/901/messages" && method === "GET") {
    const since = Number(url.searchParams.get("since") || 0);
    return json(response, 200, { items: reportItems.filter((item) => item.seq > since), cursor: reportItems.at(-1).seq, running: false, hasMore: false });
  }
  if (/^\/api\/conversations\/901\/messages\/\d+$/.test(endpoint) && method === "GET") {
    const item = reportItems.find((item) => item.seq === Number(endpoint.split("/").at(-1)));
    assert.ok(item, "Unknown synthetic reporter message detail");
    return json(response, 200, { detail: item.detail });
  }
  if (endpoint === "/api/exploration/findings/503/traffic") return json(response, 200, { finding_id: "503", version: 1, report_version: 1, bindings: [] });
  if (endpoint === "/api/exploration/findings/501/generate-report" && method === "POST") return json(response, 200, { queued: true, agent_key: "reporter", finding_id: 501, finding_node_id: 501 });
  if (endpoint === "/api/exploration/findings/501") return json(response, 200, { ...findings[0], inherited: url.searchParams.has("context_task") && url.searchParams.get("context_task") !== "101", source_task_id: "101" });
  if (endpoint === "/api/exploration/findings/503") return json(response, 200, { ...findings[0], id: "503", finding_id: "503", task_id: undefined, source_task_id: undefined, name: "UI_FIXTURE_ORPHAN", report: "" });
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
  } catch (error) { json(response, 500, { error: "Fixture server failed" }); }
});
async function eventually(check, label) {
  const deadline = Date.now() + 15000;
  while (Date.now() < deadline) { if (await check()) return; await new Promise((resolve) => setTimeout(resolve, 50)); }
  assert.fail(label);
}
function last(endpoint) { return requests.filter((r) => r.endpoint === endpoint).at(-1); }
function assertFilters(endpoint, from) {
  const request = last(endpoint);
  assert.ok(request, `Missing ${endpoint}`);
  assert.equal(request.query.verification, "unverified", `Lost verification on ${endpoint}`);
  assert.equal(request.query.task_id, "101", `Lost task on ${endpoint}`);
  assert.equal(request.query.from, from, `Lost date on ${endpoint}`);
}
let browser;
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
  await context.addInitScript(({ token }) => {
    localStorage.setItem("artex_token", token);
    if (!localStorage.getItem("artex_finding_list_preferences")) localStorage.setItem("artex_finding_list_preferences", JSON.stringify({ view: "flat", verification: "unverified", task: "101", severity: "all", status: "all", vulnclass: "all", sort: "severity" }));
    document.cookie = "artex_session=1; path=/; SameSite=Lax";
    document.cookie = "sidebar_state=false; path=/; SameSite=Lax";
  }, { token });
  const page = await context.newPage();
  page.setDefaultTimeout(15000);
  const errors = []; page.on("pageerror", (error) => errors.push(error.message));
  await page.goto(origin + "/function/findings/", { waitUntil: "networkidle" });
  await page.getByText("UI_FIXTURE_UNVERIFIED", { exact: true }).first().waitFor();
  assert.equal(await page.getByText("UI_FIXTURE_CONFIRMED", { exact: true }).count(), 0);
  await page.getByRole("button", { name: "按时间筛选", exact: true }).click();
  await page.getByRole("button", { name: "开始日期", exact: true }).click();
  await page.locator('button[data-day]:not([disabled])').first().click();
  await page.keyboard.press("Escape");
  await eventually(() => Boolean(last("/api/exploration/finding-cases")?.query.from), "Flat view did not submit the selected date");
  const from = last("/api/exploration/finding-cases").query.from;
  assertFilters("/api/exploration/finding-cases", from);
  await eventually(() => last("/api/exploration/findings/stats")?.query.from === from, "Statistics omitted shared date filter");
  assertFilters("/api/exploration/findings/stats", from);
  assert.equal(last("/api/exploration/findings/stats").query.page, undefined);
  assert.equal(last("/api/exploration/findings/stats").query.limit, undefined);
  await page.getByRole("region", { name: "当前筛选验证状态统计" }).getByText("当前筛选，不受分页影响", { exact: true }).waitFor();
  console.log("PASS flat date + verification + task filters survive merge");
  await page.getByRole("tab", { name: "按任务分组", exact: true }).click();
  await eventually(() => last("/api/exploration/findings/groups")?.query.from === from, "Groups omitted shared date filter");
  assertFilters("/api/exploration/findings/groups", from);
  await page.locator('button[aria-expanded]:not([role="combobox"])').filter({ hasText: taskName }).click();
  await page.getByText("UI_FIXTURE_UNVERIFIED", { exact: true }).first().waitFor();
  assertFilters("/api/exploration/finding-cases", from);
  console.log("PASS grouped rows keep shared filters and display unverified reports");
  await page.getByRole("tab", { name: "按资产", exact: true }).click();
  await eventually(() => last("/api/exploration/findings/asset-tree")?.query.from === from, "Asset view omitted date filter");
  assertFilters("/api/exploration/findings/asset-tree", from);
  assertFilters("/api/exploration/finding-cases", from);
  await page.getByRole("tab", { name: "按漏洞", exact: true }).click();
  // The case view uses FindingCaseMemberRow: its visible report link deliberately
  // includes the stable report ID. Flat/group tables render the name separately.
  await page.getByRole("link", { name: "#501 UI_FIXTURE_UNVERIFIED", exact: true }).waitFor();
  assert.equal(await page.getByRole("link", { name: "#502 UI_FIXTURE_CONFIRMED", exact: true }).count(), 0);
  assertFilters("/api/exploration/finding-cases", from);
  assert.equal(last("/api/exploration/finding-cases").query.asset_scope, undefined);
  console.log("PASS asset and case views keep verification/date filters");
  await page.goto(origin + "/function/findings/detail/?id=501", { waitUntil: "networkidle" });
  await page.getByRole("button", { name: "更新验证", exact: true }).waitFor();
  await page.getByText("原始上报历史", { exact: false }).first().waitFor();
  await page.getByText("步骤同时展示，数据包默认隐藏；需要核对原始请求时再按步骤展开。", { exact: true }).waitFor();
  await page.getByText("证据 / PoC", { exact: true }).waitFor();
  await page.getByText("详细报告", { exact: true }).waitFor();
  await eventually(() => requests.some((r) => r.endpoint === "/api/workspace/preview"), "Report screenshot preview was not requested");
  assert.ok(requests.filter((r) => r.endpoint === "/api/workspace/preview").every((r) => r.query.finding_id === "501"), "Report screenshot used a task/viewing context instead of its actual finding");
  assert.equal(await page.getByText("漏洞复测", { exact: true }).count(), 1, "Retest panel was duplicated by conflict resolution");
  await page.getByRole("button", { name: "重新生成步骤与报告", exact: true }).click();
  await eventually(() => requests.some((r) => r.method === "POST" && r.endpoint.endsWith("/generate-report")), "Report generation action was not preserved");
  assert.equal(requests.filter((r) => r.method === "POST" && r.endpoint.includes("/conversations")).length, 0, "Report generation incorrectly started Auto Agent");
  console.log("PASS detail preserves verification/history/structured steps/raw evidence/report/retest");
  await page.getByRole("button", { name: "询问 Auto Agent（只读）", exact: true }).click();
  await page.getByText(/共享任务旁路历史（清空影响共享问答）/).waitFor();
  await page.getByRole("textbox", { name: "旁路问题", exact: true }).fill("解释这条合成证据");
  await page.getByRole("button", { name: "发送旁路问题", exact: true }).click();
  await page.getByText("UI_FIXTURE_READ_ONLY_ANSWER", { exact: true }).waitFor();
  const sideRequest = requests.find((r) => r.method === "POST" && r.endpoint.endsWith("/side-questions"));
  assert.ok(sideRequest, "Finding analysis omitted the read-only side question endpoint");
  assert.equal(sideRequest.endpoint, "/api/tasks/101/chat/side-questions");
  assert.equal(sideRequest.query.agent, "auto");
  assert.equal(sideRequest.query.finding_id, "501");
  assert.equal(requests.filter((r) => r.method === "POST" && r.endpoint.includes("/conversations")).length, 0);
  await page.getByRole("button", { name: "关闭旁路面板", exact: true }).click();
  console.log("PASS finding analysis is task-bound read-only Q&A, not an executable Auto conversation");
  // Next integrates the native History API with useSearchParams. Keep the same
  // document/component alive so the previous delayed report callback still runs.
  const previewsBeforeInherited = requests.filter((r) => r.endpoint === "/api/workspace/preview").length;
  await page.evaluate(() => window.history.pushState(null, "", "/function/findings/detail/?id=501&context_task=202"));
  await page.getByText("来源任务 #101 · 只读", { exact: true }).waitFor();
  assert.equal(await page.getByRole("button", { name: "更新验证", exact: true }).count(), 0);
  assert.equal(await page.getByRole("button", { name: "重新生成步骤与报告", exact: true }).count(), 0);
  // A pending report refresh from the previous context must not overwrite the
  // inherited read-only scope after navigation.
  await new Promise((resolve) => setTimeout(resolve, 2300));
  assert.equal(await page.getByRole("button", { name: "更新验证", exact: true }).count(), 0);
  assert.equal(await page.getByRole("button", { name: "重新生成步骤与报告", exact: true }).count(), 0);
  assert.equal(await page.getByRole("button", { name: "调整报告", exact: true }).count(), 0, "Inherited view exposed the report editing action");
  await eventually(() => requests.filter((r) => r.endpoint === "/api/workspace/preview").length > previewsBeforeInherited, "Inherited report did not reload the associated screenshot");
  assert.ok(requests.filter((r) => r.endpoint === "/api/workspace/preview").every((r) => r.query.finding_id === "501"), "Inherited screenshot used context_task instead of source finding identity");
  console.log("PASS inherited detail remains read-only after delayed report refresh");
  await page.getByRole("button", { name: "询问 Auto Agent（只读）", exact: true }).click();
  await page.getByRole("textbox", { name: "旁路问题", exact: true }).waitFor();
  assert.equal(last("/api/tasks/101/chat/side-questions").query.finding_id, "501");
  assert.equal(requests.filter((r) => r.endpoint.includes("/tasks/202/chat/side-questions")).length, 0, "Inherited viewing context became an analysis grant");
  await page.getByRole("button", { name: "关闭旁路面板", exact: true }).click();
  await page.evaluate(() => window.history.pushState(null, "", "/function/findings/detail/?id=503"));
  await page.getByText("无任务关联，分析入口不可用", { exact: true }).waitFor();
  assert.ok(await page.getByRole("button", { name: "询问 Auto Agent（只读）", exact: true }).isDisabled());
  assert.equal(requests.filter((r) => r.method === "POST" && r.endpoint.includes("/conversations")).length, 0);
  const orphanReportButton = page.getByRole("button", { name: "调整报告", exact: true });
  assert.ok((await orphanReportButton.count()) === 0 || await orphanReportButton.isDisabled(), "Orphan finding exposed executable report editing");
  console.log("PASS inherited analysis uses actual owner; orphan finding has no execution fallback");

  // Reporter editing is deliberately separate from task-bound read-only Auto Q&A.
  // Neither the stored ID nor the finding DTO's old ID may select the conversation.
  await page.evaluate(() => {
    localStorage.setItem("artex.finding.reporter-conversation.501", "999");
    localStorage.setItem("artex.finding.auto-conversation.501", "999");
  });
  await page.goto(origin + "/function/findings/detail/?id=501", { waitUntil: "networkidle" });
  await page.getByRole("button", { name: "调整报告", exact: true }).click();
  let reportDialog = page.getByRole("dialog", { name: "调整漏洞报告", exact: true });
  await reportDialog.getByText("UI_FIXTURE_REPORT_RESOLVE_FAILURE", { exact: false }).waitFor();
  assert.ok(await reportDialog.getByRole("textbox", { name: "报告调整要求", exact: true }).isDisabled());
  assert.equal(reportSends, 0, "Failed report resolution still sent a message");
  assert.equal(requests.filter((r) => r.endpoint.includes("/conversations/999")).length, 0, "Client-trusted report conversation ID was used");
  assert.equal(requests.filter((r) => r.method === "POST" && r.endpoint === "/api/conversations").length, 0, "Resolver failure created an unbound report conversation");
  await page.keyboard.press("Escape");
  await reportDialog.waitFor({ state: "hidden" });
  const previewsBeforeReporter = requests.filter((r) => r.endpoint === "/api/workspace/preview").length;
  await page.getByRole("button", { name: "调整报告", exact: true }).click();
  reportDialog = page.getByRole("dialog", { name: "调整漏洞报告", exact: true });
  await reportDialog.getByText("UI_FIXTURE_EXISTING_REPORT_HISTORY", { exact: true }).waitFor();
  await eventually(() => requests.filter((r) => r.endpoint === "/api/workspace/preview").length > previewsBeforeReporter, "Report transcript did not inherit its finding identity for screenshot preview");
  assert.equal(reportResolves, 2, "Reopening the report editor did not revalidate its real binding");
  await reportDialog.getByRole("textbox", { name: "报告调整要求", exact: true }).fill("仅整理现有证据，不增加测试结论");
  await reportDialog.getByRole("button", { name: "发送报告调整", exact: true }).click();
  await reportDialog.getByText("UI_FIXTURE_REPORT_EDIT_ANSWER", { exact: true }).waitFor();
  assert.equal(reportSends, 1, "Report editing replayed the submitted action");
  await page.keyboard.press("Escape");
  await reportDialog.waitFor({ state: "hidden" });
  await page.getByRole("button", { name: "调整报告", exact: true }).click();
  reportDialog = page.getByRole("dialog", { name: "调整漏洞报告", exact: true });
  await reportDialog.getByText("UI_FIXTURE_REPORT_EDIT_ANSWER", { exact: true }).waitFor();
  assert.equal(reportResolves, 3, "Report editor reused an unchecked cached conversation ID");
  assert.equal(reportSends, 1, "Reopening a report conversation repeated a send");
  assert.equal(requests.filter((r) => r.method === "POST" && r.endpoint === "/api/conversations").length, 0);
  assert.equal(requests.filter((r) => r.endpoint.includes("/conversations/999")).length, 0);
  await page.keyboard.press("Escape");
  await reportDialog.waitFor({ state: "hidden" });
  console.log("PASS report editing resolves a real bound reporter conversation, fails closed, preserves history and never replays sends");

  // The image viewer must reset both scale and pan, not leave a 100% image offscreen.
  await page.getByRole("button", { name: "放大查看报告截图", exact: true }).first().click({ force: true });
  const imageDialog = page.getByRole("dialog").filter({ has: page.getByRole("button", { name: "重置图片大小", exact: true }) });
  await imageDialog.getByRole("button", { name: "放大图片", exact: true }).click();
  await imageDialog.getByRole("button", { name: "放大图片", exact: true }).click();
  const transformedImage = imageDialog.getByRole("img", { name: "UI_FIXTURE_SCREENSHOT", exact: true }).locator("..");
  const panViewport = transformedImage.locator("..");
  const box = await panViewport.boundingBox();
  assert.ok(box && box.width > 100 && box.height > 100, "Screenshot pan viewport was unavailable");
  await page.mouse.move(box.x + box.width / 2, box.y + box.height / 2);
  await page.mouse.down();
  await page.mouse.move(box.x + box.width / 2 + 80, box.y + box.height / 2 + 45, { steps: 3 });
  await page.mouse.up();
  assert.match(await transformedImage.evaluate((element) => element.style.transform), /translate\((?!0px,\s*0px)/, "Dragging a zoomed screenshot did not update its pan");
  await imageDialog.getByRole("button", { name: "重置图片大小", exact: true }).click();
  await imageDialog.getByText("100%", { exact: true }).waitFor();
  assert.match(await transformedImage.evaluate((element) => element.style.transform), /translate\(0px,\s*0px\) scale\(1\)/, "Resetting the screenshot left a nonzero pan offset");
  await page.keyboard.press("Escape");
  await imageDialog.waitFor({ state: "hidden" });
  assert.ok(requests.filter((r) => r.endpoint === "/api/workspace/preview").every((r) => r.query.finding_id === "501"));
  console.log("PASS report, inherited view and transcript screenshots keep finding identity; zoom reset also clears pan");

  // The new packet editor must preserve the old search's visible highlighting.
  await page.getByRole("button", { name: "查看数据包", exact: true }).click();
  const packetEditor = page.getByRole("textbox", { name: "HTTP 报文代码", exact: true }).first();
  await packetEditor.waitFor();
  await page.getByRole("textbox", { name: "搜索报文内容", exact: true }).first().fill("FIXTURE_PACKET_MATCH");
  await page.getByText("1 处命中", { exact: true }).first().waitFor();
  await eventually(async () => await packetEditor.locator(".cm-searchMatch").count() > 0, "Packet search counted matches without highlighting them");
  const packetSearch = page.getByRole("textbox", { name: "搜索报文内容", exact: true }).first();
  await packetSearch.fill("UI_FIXTURE_ABSENT_98765");
  await page.getByText("0 处命中", { exact: true }).first().waitFor();
  await eventually(async () => await packetEditor.locator(".cm-searchMatch").count() === 0, "Absent packet query left stale highlights");
  await packetSearch.fill("FIXTURE_DOUBLE");
  await page.getByText("2 处命中", { exact: true }).first().waitFor();
  await eventually(async () => await packetEditor.locator(".cm-searchMatch").count() === 2, "Repeated packet query or match at document end was counted/highlighted incorrectly");
  await packetSearch.fill(".*[x](a)");
  await page.getByText("1 处命中", { exact: true }).first().waitFor();
  await eventually(async () => await packetEditor.locator(".cm-searchMatch").count() === 1, "Packet query regex metacharacters were not searched literally");
  await packetSearch.fill("");
  await eventually(async () => await packetEditor.locator(".cm-searchMatch").count() === 0, "Clearing packet search left stale highlights");
  console.log("PASS read-only packet search counts and highlights unique/repeated/end/literal queries and clears absent matches");
  const sendsBeforeSeed = requests.filter((r) => r.endpoint === "/api/repeater/send").length;
  await page.getByRole("button", { name: "送入请求重发", exact: true }).first().click();
  const requestEditor = page.getByRole("textbox", { name: "请求", exact: true });
  await requestEditor.waitFor();
  assert.ok((await requestEditor.innerText()).includes("FIXTURE_PACKET_MATCH"), "Saved evidence did not seed the request draft");
  assert.equal(requests.filter((r) => r.endpoint === "/api/repeater/send").length, sendsBeforeSeed, "Prefilling the repeater transmitted a request");
  const originalEditor = await requestEditor.elementHandle();
  await requestEditor.click();
  await requestEditor.press("ControlOrMeta+End");
  await requestEditor.press("Enter");
  await requestEditor.pressSequentially("UI_FIXTURE_UNDO_MARKER");
  assert.ok((await requestEditor.innerText()).includes("UI_FIXTURE_UNDO_MARKER"));
  await page.getByRole("button", { name: "切换为不换行", exact: true }).first().click();
  assert.ok(await originalEditor.evaluate((element) => element.isConnected), "Changing line wrapping destroyed the editor and its history");
  await requestEditor.click();
  await requestEditor.press("ControlOrMeta+z");
  await eventually(async () => !(await requestEditor.innerText()).includes("UI_FIXTURE_UNDO_MARKER"), "Undo history was lost after changing line wrapping");
  assert.ok((await requestEditor.innerText()).includes("FIXTURE_PACKET_MATCH"), "Undo erased the original evidence draft");
  assert.equal(requests.filter((r) => r.endpoint === "/api/repeater/send").length, sendsBeforeSeed);
  console.log("PASS evidence prefill never sends; undo survives packet editor reconfiguration");
  assert.deepEqual(errors, [], "Findings UI raised browser exceptions");
  assert.deepEqual([...unexpected], [], "Unexpected API contract calls");
  assert.deepEqual([...external], [], "Fixture attempted an external connection");
  await context.close();
}
main().catch(async (error) => {
  console.error(error);
  // This server contains only synthetic fixtures. Keep failures diagnosable in
  // CI without running a real service or requesting live task/credential data.
  console.error("Recent synthetic API queries:", JSON.stringify(requests.slice(-12)));
  const page = browser?.contexts()[0]?.pages()[0];
  if (page && !page.isClosed()) {
    try {
      console.error("Fixture route:", page.url());
      console.error("Fixture visible text:", (await page.locator("body").innerText()).slice(0, 6000));
    } catch { /* The original failure remains authoritative if the page closed. */ }
  }
  process.exitCode = 1;
}).finally(async () => {
  if (browser) await browser.close();
  await new Promise((resolve) => server.close(resolve));
});
