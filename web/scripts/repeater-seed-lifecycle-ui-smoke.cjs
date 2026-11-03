#!/usr/bin/env node
/* Cloud-only synthetic UI regression. No ARTEX process, real task, packet,
 * credential, clipboard, model call or target request is used. */
"use strict";
const assert = require("node:assert/strict");
const fs = require("node:fs");
const http = require("node:http");
const path = require("node:path");
const { once } = require("node:events");
if (!process.env.CI || !process.env.ARTEX_PLAYWRIGHT_DIR) throw new Error("Run in GitHub Actions with its temporary Playwright runtime.");
const { chromium } = require(path.resolve(process.env.ARTEX_PLAYWRIGHT_DIR));
const output = path.resolve(__dirname, "..", "out");
const stamp = "2026-11-03T12:00:00Z";
const storageKey = "artex_repeater_workspace_v1";
const baseline = { version: 1, groups: [{ id: "default", name: "Fixture" }], activeTabId: "kept-draft", tabs: [{ id: "kept-draft", groupId: "default", title: "KEEP_EXISTING_DRAFT", method: "POST", url: "https://fixture.invalid/kept", headersText: "Content-Type: text/plain", body: "KEEP_EXISTING_DRAFT_BODY", useProxy: true, insecure: false, response: null, history: [], updatedAt: stamp }] };
const requests = [], unexpected = new Set(), external = new Set();
const delayed = new Set();
const held = new Map();
const flows = [601, 602, 603].map((id) => ({ id: String(id), ts: stamp, method: "POST", host: `fixture-${id}.invalid`, url: `https://fixture-${id}.invalid/packet`, status: 200, content_type: "text/plain", resp_len: 16, req_len: 32 }));
function packet(id) { const flow = flows.find((item) => item.id === String(id)); return { req: `POST /packet HTTP/1.1\r\nHost: ${flow.host}\r\nContent-Type: text/plain\r\n\r\nFIXTURE_BODY_${id}`, resp: "HTTP/1.1 200 OK\r\n\r\nFIXTURE_RESPONSE" }; }
function finding(id) { return { id, finding_id: id, task_id: "101", source_task_id: "101", name: `FINDING_SCOPE_${id}`, vulnclass: "Synthetic", severity: "low", status: "pending", verification_status: "unverified", verification_note: "Fixture only", summary: "Synthetic saved record", evidence: "Synthetic saved evidence", report: "", evidence_version: 1, assets: [], traffic_count: id === "501" ? 1 : 0, inherited: false, ts: stamp }; }
const binding = { id: "701", finding_id: "501", snapshot_id: "801", role: "supporting", note: "SYNTHETIC_BOUND_PACKET", position: 0, created_at: stamp, snapshot: { id: "801", source_traffic_id: "601", captured_at: 1793707200, url: flows[0].url, method: "POST", status: 200, content_type: "text/plain", req_head: "POST /packet HTTP/1.1\r\nHost: fixture-601.invalid\r\nContent-Type: text/plain", resp_head: "HTTP/1.1 200 OK", req_hash: "synthetic", resp_hash: "synthetic", req_len: 16, resp_len: 16 } };
const evidence = { binding, request: { content: "FIXTURE_EVIDENCE_BODY", binary: false, truncated: false, offset: 0, total: 21, next_offset: 21 }, response: { content: "FIXTURE_RESPONSE", binary: false, truncated: false, offset: 0, total: 16, next_offset: 16 } };
function json(response, value, code = 200) { if (response.destroyed || response.writableEnded) return; response.writeHead(code, { "Content-Type": "application/json", "Cache-Control": "no-store" }); response.end(JSON.stringify(value)); }
async function delayedJSON(key, response, value) { if (delayed.has(key)) await new Promise((resolve) => { held.set(key, resolve); }); json(response, value); }
function release(key) { delayed.delete(key); const resolve = held.get(key); held.delete(key); resolve?.(); }
function task(id) { return { id, name: `TASK_SCOPE_${id}`, description: `Synthetic task ${id}`, goal: "Read saved fixture metadata only", status: "done", paused: false, exploration_id: id, worker_count: 1, workers: 1, created_at: stamp, goals_total: 0, goals_met: 0, intents_total: 0, completed_at: stamp, tokens: { input: 0, output: 0, cache_read: 0, cache_write: 0 } }; }
async function serveAPI(request, response, url) {
  const endpoint = url.pathname, method = request.method || "GET";
  requests.push({ endpoint, method, query: Object.fromEntries(url.searchParams) });
  if (method !== "GET") { unexpected.add(`${method} ${endpoint}`); return json(response, { error: "Mutation forbidden in this fixture" }, 405); }
  if (endpoint === "/api/auth/status") return json(response, { initialized: true });
  if (endpoint === "/api/version") return json(response, { version: "fixture" });
  if (endpoint === "/api/update/check") return json(response, { has_update: false, current: "fixture" });
  if (endpoint === "/api/settings") return json(response, { show_egress_ip: false, traffic_capture: false });
  if (endpoint === "/api/agents") return json(response, { agents: [] });
  if (endpoint === "/api/skills/missing") return json(response, { missing: [] });
  if (endpoint === "/api/intercept/pending") return json(response, { pending: [] });
  if (endpoint === "/api/tasks") return json(response, { tasks: [task("101"), task("202")], active: "" });
  if (/^\/api\/tasks\/(101|202)$/.test(endpoint)) return json(response, task(endpoint.split("/").at(-1)));
  if (/^\/api\/intercept\/task\/(101|202)$/.test(endpoint)) return json(response, { items: [] });
  if (endpoint === "/api/llm/profiles") return json(response, { profiles: [] });
  if (endpoint === "/api/stats") return json(response, { active_task: null, tasks: 2, traffic: 0, llm_configured: false, engine_mode: "idle" });
  if (endpoint === "/api/report") { response.writeHead(200, { "Content-Type": "text/markdown" }); return response.end("# SYNTHETIC_REPORT_VIEW"); }
  if (endpoint === "/api/traffic") { const current = url.searchParams.get("task_id") === "202" ? [flows[2]] : flows; return json(response, { enabled: false, count: current.length, total: current.length, page: 0, size: 50, exchanges: current, proxy: "" }); }
  if (endpoint === "/api/traffic/hosts") return json(response, { hosts: flows.map((flow) => ({ host: flow.host, count: 1 })) });
  if (endpoint === "/api/traffic/exchange") { const id = url.searchParams.get("id"); assert.ok(flows.some((item) => item.id === id)); return delayedJSON(`traffic:${id}`, response, packet(id)); }
  const record = endpoint.match(/^\/api\/exploration\/findings\/(501|503)$/);
  if (record) return json(response, finding(record[1]));
  if (/^\/api\/exploration\/findings\/(501|503)\/retests$/.test(endpoint)) return json(response, { retests: [] });
  if (/^\/api\/findings\/(501|503)\/inline-history$/.test(endpoint)) return json(response, { events: [] });
  if (endpoint === "/api/exploration/findings/501/traffic") return json(response, { finding_id: "501", version: 1, report_version: 1, bindings: [binding] });
  if (endpoint === "/api/exploration/findings/503/traffic") return json(response, { finding_id: "503", version: 1, report_version: 1, bindings: [] });
  if (endpoint === "/api/exploration/findings/501/traffic/701") return delayedJSON("finding:701", response, evidence);
  unexpected.add(`${method} ${endpoint}`); return json(response, { error: "Unmocked synthetic endpoint" }, 404);
}
const mime = { ".html": "text/html", ".js": "text/javascript", ".css": "text/css", ".json": "application/json", ".svg": "image/svg+xml", ".png": "image/png", ".woff2": "font/woff2", ".ico": "image/x-icon", ".txt": "text/plain" };
const server = http.createServer(async (request, response) => { try { const url = new URL(request.url || "/", "http://127.0.0.1"); if (url.pathname.startsWith("/api/")) return await serveAPI(request, response, url); let file = path.resolve(output, "." + decodeURIComponent(url.pathname)); if (file !== output && !file.startsWith(output + path.sep)) { response.writeHead(403); response.end(); return; } if (fs.existsSync(file) && fs.statSync(file).isDirectory()) file = path.join(file, "index.html"); if (!fs.existsSync(file) || !fs.statSync(file).isFile()) { response.writeHead(404); response.end("Missing fixture asset"); return; } response.writeHead(200, { "Content-Type": mime[path.extname(file)] || "application/octet-stream", "Cache-Control": "no-store" }); fs.createReadStream(file).pipe(response); } catch { json(response, { error: "Fixture server failed" }, 500); } });
async function eventually(check, message) { const deadline = Date.now() + 20000; while (Date.now() < deadline) { if (await check()) return; await new Promise((resolve) => setTimeout(resolve, 25)); } assert.fail(message); }
async function workspace(page) { return page.evaluate((key) => JSON.parse(localStorage.getItem(key) || "null"), storageKey); }
async function settle(page) { await page.waitForTimeout(700); }
let browser, page;
async function main() {
  server.listen(0, "127.0.0.1"); await once(server, "listening"); const origin = `http://127.0.0.1:${server.address().port}`;
  browser = await chromium.launch({ headless: true });
  const context = await browser.newContext({ viewport: { width: 1680, height: 1100 }, locale: "zh-CN", timezoneId: "Asia/Shanghai" });
  await context.route("**/*", (route) => { const url = new URL(route.request().url()); if (url.origin === origin || ["data:", "blob:"].includes(url.protocol)) return route.continue(); external.add(url.origin); return route.abort("blockedbyclient"); });
  const token = [Buffer.from('{"alg":"none"}').toString("base64url"), Buffer.from('{"sub":"fixture-admin","exp":4102444800}').toString("base64url"), "fixture"].join(".");
  await context.addInitScript(({ token, storageKey, baseline }) => {
    localStorage.setItem("artex_token", token); localStorage.setItem(storageKey, JSON.stringify(baseline)); document.cookie = "artex_session=1; path=/; SameSite=Lax"; document.cookie = "sidebar_state=false; path=/; SameSite=Lax";
    window.__fixtureDocument = Math.random(); window.__seedSignalAborts = [];
    // Simulate a transport that completes despite cancellation: generation
    // guards, not cancellation alone, must prevent stale global side effects.
    const original = window.fetch.bind(window);
    window.fetch = (input, init) => { const url = new URL(typeof input === "string" ? input : input.url, location.href); const marker = url.pathname.endsWith("/traffic/701") ? "finding:701" : url.pathname === "/api/traffic/exchange" ? `traffic:${url.searchParams.get("id")}` : ""; if (marker && window.__ignoreSeedAbort) { const options = { ...init }; const signal = options.signal; window.__seedSignalAborts.push({ marker, provided: !!signal, aborted: !!signal?.aborted }); const event = window.__seedSignalAborts.at(-1); signal?.addEventListener("abort", () => { event.aborted = true; }, { once: true }); delete options.signal; return original(input, options); } return original(input, init); };
  }, { token, storageKey, baseline });
  page = await context.newPage(); page.setDefaultTimeout(20000); const errors = []; page.on("pageerror", (error) => errors.push(error.message));

  delayed.add("finding:701"); await page.goto(origin + "/function/findings/detail?id=501", { waitUntil: "networkidle" });
  await page.getByRole("heading", { name: "FINDING_SCOPE_501", exact: true }).waitFor();
  await page.evaluate(() => { window.__ignoreSeedAbort = true; }); const firstDocument = await page.evaluate(() => window.__fixtureDocument);
  await page.getByRole("button", { name: "送入请求重发", exact: true }).click(); await eventually(() => held.has("finding:701"), "Finding evidence request did not reach delay barrier");
  await page.evaluate(() => history.pushState(null, "", "/function/findings/detail?id=503")); await page.getByRole("heading", { name: "FINDING_SCOPE_503", exact: true }).waitFor();
  assert.equal(await page.evaluate(() => window.__fixtureDocument), firstDocument, "Scope test replaced the document instead of exercising late callbacks");
  release("finding:701"); await settle(page); assert.ok(page.url().includes("id=503")); assert.deepEqual(await workspace(page), baseline);
  assert.ok(await page.evaluate(() => window.__seedSignalAborts.some((event) => event.marker === "finding:701" && event.provided && event.aborted)), "Finding scope cleanup did not cancel the read signal");
  await page.evaluate(() => history.pushState(null, "", "/function/findings/detail?id=501")); await page.getByRole("heading", { name: "FINDING_SCOPE_501", exact: true }).waitFor();
  await page.getByRole("button", { name: "送入请求重发", exact: true }).click(); await page.waitForURL((url) => url.pathname.replace(/\/$/, "") === "/function/repeater", { waitUntil: "domcontentloaded" }); await eventually(async () => (await workspace(page)).tabs.some((tab) => tab.body === "FIXTURE_EVIDENCE_BODY"), "Current finding failed to import");
  console.log("PASS finding change ignores delayed evidence without losing the current import or sending a request");

  delayed.add("traffic:601"); await page.goto(origin + "/function/tasks/detail?id=101&tab=traffic", { waitUntil: "networkidle" }); await page.getByRole("checkbox", { name: "选择本页流量", exact: true }).waitFor();
  await page.evaluate(() => { window.__ignoreSeedAbort = true; }); const taskDocument = await page.evaluate(() => window.__fixtureDocument);
  await page.getByRole("checkbox", { name: "选择本页流量", exact: true }).check(); await page.getByRole("button", { name: "送入请求重发", exact: true }).click(); await eventually(() => held.has("traffic:601"), "Task import did not reach delay barrier");
  await page.evaluate(() => history.pushState(null, "", "/function/tasks/detail?id=202&tab=traffic")); await page.getByText("已选 0 条流量", { exact: true }).waitFor(); await page.getByText(flows[2].url, { exact: true }).waitFor();
  assert.equal(await page.evaluate(() => window.__fixtureDocument), taskDocument); release("traffic:601"); await settle(page); assert.ok(page.url().includes("id=202")); assert.deepEqual(await workspace(page), baseline);
  assert.ok(await page.evaluate(() => window.__seedSignalAborts.some((event) => event.marker === "traffic:601" && event.provided && event.aborted)), "Task scope cleanup did not cancel the read signal");
  assert.equal(requests.filter((item) => item.endpoint === "/api/traffic/exchange" && item.query.id === "602").length, 0, "Stale batch continued to the next packet");
  await page.getByRole("checkbox", { name: "选择本页流量", exact: true }).check(); await page.getByRole("button", { name: "送入请求重发", exact: true }).click(); await page.waitForURL((url) => url.pathname.replace(/\/$/, "") === "/function/repeater", { waitUntil: "domcontentloaded" }); await eventually(async () => (await workspace(page)).tabs.some((tab) => tab.body === "FIXTURE_BODY_603"), "New task remained locked after the prior import was cancelled");
  console.log("PASS task change cancels the old batch, clears prior selection and keeps the new task import usable");

  delayed.add("traffic:602"); await page.goto(origin + "/function/tasks/detail?id=101&tab=traffic", { waitUntil: "networkidle" }); await page.getByRole("checkbox", { name: "选择本页流量", exact: true }).waitFor(); await page.evaluate(() => { window.__ignoreSeedAbort = true; });
  const reads603 = requests.filter((item) => item.endpoint === "/api/traffic/exchange" && item.query.id === "603").length;
  await page.getByRole("checkbox", { name: "选择本页流量", exact: true }).check(); await page.getByRole("button", { name: "送入请求重发", exact: true }).click(); await eventually(() => held.has("traffic:602"), "Second batch packet did not reach delay barrier");
  await eventually(async () => (await workspace(page)).tabs.some((tab) => tab.body === "FIXTURE_BODY_601"), "First valid batch packet was not retained"); const partial = await workspace(page);
  await page.getByRole("tab", { name: "报告", exact: true }).click(); await page.getByText("SYNTHETIC_REPORT_VIEW", { exact: true }).waitFor(); release("traffic:602"); await settle(page);
  assert.deepEqual(await workspace(page), partial, "Unmounted traffic import changed global drafts"); assert.ok(!page.url().includes("/function/repeater"));
  assert.equal(requests.filter((item) => item.endpoint === "/api/traffic/exchange" && item.query.id === "603").length, reads603, "Unmounted batch fetched another packet");
  assert.ok((await workspace(page)).tabs.some((tab) => tab.id === "kept-draft" && tab.body === "KEEP_EXISTING_DRAFT_BODY"));
  assert.equal(requests.filter((item) => item.endpoint === "/api/repeater/send").length, 0, "Evidence import sent a request"); assert.deepEqual(errors, []); assert.deepEqual([...unexpected], []); assert.deepEqual([...external], []);
  console.log("PASS leaving the traffic tab keeps earlier valid drafts but blocks late drafts, redirects and further reads"); await context.close();
}
main().catch(async (error) => { console.error(error); console.error("Recent synthetic requests", JSON.stringify(requests.slice(-12))); if (page && !page.isClosed()) { try { console.error("Synthetic route", page.url()); console.error("Synthetic visible text", (await page.locator("body").innerText()).slice(0, 5000)); } catch {} } process.exitCode = 1; }).finally(async () => { for (const key of held.keys()) release(key); if (browser) await browser.close(); server.closeAllConnections?.(); await new Promise((resolve) => server.close(resolve)); });
