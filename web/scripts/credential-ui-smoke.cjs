#!/usr/bin/env node
/*
 * Cloud-only browser regression for the exported credential management UI.
 *
 * Run after build:static on the GitHub Actions runner:
 *   ARTEX_PLAYWRIGHT_DIR=/tmp/runtime/node_modules/playwright \
 *   ARTEX_UI_ARTIFACTS=/tmp/credential-ui-artifacts \
 *   node scripts/credential-ui-smoke.cjs
 *
 * Uses synthetic data and an ephemeral loopback-only static/mock API server.
 * No Go service, database, external target, real credential or system clipboard
 * is used. Playwright is installed in runner temporary storage, not this repo.
 */
"use strict";

const assert = require("node:assert/strict");
const fs = require("node:fs");
const http = require("node:http");
const path = require("node:path");
const { once } = require("node:events");

if (!process.env.CI) {
  throw new Error("This browser regression is cloud-only; run it in GitHub Actions.");
}
if (!process.env.ARTEX_PLAYWRIGHT_DIR) {
  throw new Error("ARTEX_PLAYWRIGHT_DIR must point to the runner's temporary playwright package.");
}
const { chromium } = require(path.resolve(process.env.ARTEX_PLAYWRIGHT_DIR));
const outputDirectory = path.resolve(__dirname, "..", "out");
const artifacts = path.resolve(process.env.ARTEX_UI_ARTIFACTS || path.join(process.env.RUNNER_TEMP || "/tmp", "credential-ui-artifacts"));
if (!fs.existsSync(path.join(outputDirectory, "intranet", "index.html"))) {
  throw new Error("Missing static export: run npm run build:static in the cloud frontend job first.");
}
fs.mkdirSync(artifacts, { recursive: true });

const FIXTURE_SECRET = "UI_FIXTURE_ONLY_NOT_A_REAL_SECRET_2026";
const PRIMARY_SOURCE = "UI fixture source: manual registration from a synthetic example.invalid response; internal evidence UI-EVIDENCE-23.";
const PRIMARY_USERNAME = "fixture-alice";
const TASK_A = "凭据回归任务 A";
const TASK_B = "凭据回归任务 B";
const fixtureTime = "2026-10-29T04:00:00Z";
const records = Array.from({ length: 23 }, (_, index) => {
  const id = index + 1;
  return {
    id,
    task_id: id === 22 ? 202 : id === 1 ? 303 : 101,
    task_name: id === 22 ? TASK_B : id === 1 ? "" : TASK_A,
    task_available: id !== 1,
    cred_type: id === 22 ? "token" : "password",
    username: id === 23 ? PRIMARY_USERNAME : id === 22 ? "fixture-bob" : id === 21 ? "fixture-charlie" : `fixture-user-${id}`,
    target: id === 23 ? "https://target.example.invalid/console" : `host-${id}.example.invalid`,
    target_label: id === 23 ? "target.example.invalid" : `host-${id}.example.invalid`,
    // Query/fragment are synthetic and deliberately unsafe to include in links.
    target_url: id === 23 ? "https://target.example.invalid/console?token=fixture-query#fixture-fragment" : "",
    domain: id === 23 ? "CORP" : "",
    secret_masked: "UI****26",
    source: id === 23 ? PRIMARY_SOURCE : `UI fixture source ${id}`,
    source_url: id === 23 ? "https://source.example.invalid/evidence?token=fixture-query#fixture-fragment" : "",
    source_intent_id: id === 23 ? 723 : 0,
    source_exploration_id: id === 23 ? 501 : 0,
    verified: id === 22,
    verification_status: id === 22 ? "verified" : id === 21 ? "failed" : "unverified",
    verification_at: id === 22 || id === 21 ? fixtureTime : null,
    verification_method: id === 22 || id === 21 ? "Synthetic prior observation" : "",
    verification_evidence: id === 22 || id === 21 ? `UI-EVIDENCE-${id}` : "",
    verification_by: id === 22 || id === 21 ? "ui-fixture-admin" : "",
    verification_history: [],
    created_at: fixtureTime,
  };
});
const tasks = [
  { id: 101, name: TASK_A, description: "Synthetic task A", status: "done" },
  { id: 202, name: TASK_B, description: "Synthetic task B", status: "done" },
];
const requests = [];
const unexpectedAPI = new Set();
const blockedExternal = new Set();
const state = { failCredentials: false, failVerification: false, delayNextReveal: 0 };
const mime = {
  ".html": "text/html; charset=utf-8", ".js": "text/javascript; charset=utf-8", ".css": "text/css; charset=utf-8",
  ".json": "application/json", ".txt": "text/plain; charset=utf-8", ".svg": "image/svg+xml", ".png": "image/png",
  ".jpg": "image/jpeg", ".ico": "image/x-icon", ".woff": "font/woff", ".woff2": "font/woff2",
};

function sendJSON(response, status, value) {
  if (response.destroyed || response.writableEnded) return;
  response.writeHead(status, { "Content-Type": "application/json", "Cache-Control": "no-store", "X-Content-Type-Options": "nosniff" });
  response.end(JSON.stringify(value));
}

async function readJSON(request) {
  let text = "";
  for await (const chunk of request) {
    text += chunk;
    if (text.length > 64 * 1024) throw new Error("Fixture request body exceeded limit");
  }
  return text ? JSON.parse(text) : {};
}

async function serveAPI(request, response, url) {
  const endpoint = url.pathname;
  const method = request.method || "GET";
  if (endpoint !== "/api/health" && !request.headers.authorization?.startsWith("Bearer ")) {
    return sendJSON(response, 401, { error: "Synthetic fixture requires the UI token" });
  }
  const body = method === "GET" ? undefined : await readJSON(request);
  requests.push({ method, path: endpoint, task: url.searchParams.get("task_id"), body });
  if (endpoint === "/api/health") return sendJSON(response, 200, { ok: true, service: "artex-ui-fixture", version: "ui-fixture" });
  if (endpoint === "/api/update/check") return sendJSON(response, 200, { has_update: false, current: "ui-fixture" });
  if (endpoint === "/api/intercept/pending") return sendJSON(response, 200, { pending: [] });
  if (endpoint === "/api/auth/status") return sendJSON(response, 200, { initialized: true });
  if (endpoint === "/api/tasks") return sendJSON(response, 200, { tasks, active: "" });
  if (endpoint === "/api/sessions" || endpoint === "/api/tunnels") return sendJSON(response, 200, { items: [] });
  if (endpoint === "/api/intranet/topology") return sendJSON(response, 200, { nodes: [], edges: [], segments: [] });
  if (endpoint === "/api/intranet/credentials" && method === "GET") {
    if (state.failCredentials) return sendJSON(response, 503, { error: "fixture credential refresh failed" });
    const task = url.searchParams.get("task_id");
    return sendJSON(response, 200, { items: records.filter((record) => !task || record.task_id === Number(task)) });
  }
  const action = endpoint.match(/^\/api\/intranet\/credentials\/(\d+)\/(reveal|verification|metadata)$/);
  if (action) {
    const record = records.find((item) => item.id === Number(action[1]));
    if (!record) return sendJSON(response, 404, { error: "Fixture credential not found" });
    if (action[2] === "reveal" && method === "POST") {
      assert.ok(body.purpose === "view" || body.purpose === "copy", "Reveal purpose must be explicit");
      const delay = state.delayNextReveal;
      state.delayNextReveal = 0;
      if (delay) await new Promise((resolve) => setTimeout(resolve, delay));
      return sendJSON(response, 200, { id: record.id, secret: FIXTURE_SECRET });
    }
    if (action[2] === "verification" && method === "PUT") {
      if (state.failVerification) return sendJSON(response, 503, { error: "fixture verification save failed" });
      assert.ok(["unverified", "verified", "failed"].includes(body.status), "Verification status must be valid");
      if (body.status !== "unverified" && (!body.method?.trim() || !body.evidence?.trim())) {
        return sendJSON(response, 400, { error: "Fixture requires method and evidence" });
      }
      record.verification_status = body.status;
      record.verified = body.status === "verified";
      record.verification_at = body.status === "unverified" ? null : fixtureTime;
      record.verification_method = body.status === "unverified" ? "" : body.method;
      record.verification_evidence = body.status === "unverified" ? "" : body.evidence;
      record.verification_by = body.status === "unverified" ? "" : "ui-fixture-admin";
      record.verification_history.push({ status: body.status, at: fixtureTime, method: record.verification_method, evidence: record.verification_evidence, by: "ui-fixture-admin" });
      return sendJSON(response, 200, record);
    }
    if (action[2] === "metadata" && method === "PATCH") {
      Object.assign(record, { target: body.target, target_label: body.target, domain: body.domain, source_url: body.source_url });
      record.target_url = /^https?:\/\//.test(body.target) ? body.target : "";
      return sendJSON(response, 200, record);
    }
  }
  unexpectedAPI.add(`${method} ${endpoint}`);
  return sendJSON(response, 404, { error: "Unmocked fixture endpoint" });
}

const server = http.createServer(async (request, response) => {
  try {
    const url = new URL(request.url || "/", "http://127.0.0.1");
    if (url.pathname.startsWith("/api/")) return await serveAPI(request, response, url);
    let file = path.resolve(outputDirectory, "." + decodeURIComponent(url.pathname));
    if (file !== outputDirectory && !file.startsWith(outputDirectory + path.sep)) {
      response.writeHead(403); response.end(); return;
    }
    if (fs.existsSync(file) && fs.statSync(file).isDirectory()) file = path.join(file, "index.html");
    if (!fs.existsSync(file) || !fs.statSync(file).isFile()) {
      response.writeHead(404); response.end("Fixture file not found"); return;
    }
    response.writeHead(200, { "Content-Type": mime[path.extname(file)] || "application/octet-stream", "Cache-Control": "no-store" });
    fs.createReadStream(file).pipe(response);
  } catch (error) {
    console.error("Fixture server error:", error.message);
    sendJSON(response, 500, { error: "Fixture request failed" });
  }
});

const completed = [];
let activeBrowser;
async function check(name, run) {
  await run();
  completed.push(name);
  console.log(`PASS ${name}`);
}

async function eventually(checkCondition, message, timeout = 10_000) {
  const deadline = Date.now() + timeout;
  while (Date.now() < deadline) {
    if (await checkCondition()) return;
    await new Promise((resolve) => setTimeout(resolve, 40));
  }
  assert.fail(message);
}

async function main() {
  server.listen(0, "127.0.0.1");
  await once(server, "listening");
  const origin = `http://127.0.0.1:${server.address().port}`;
  const browser = await chromium.launch({ headless: true });
  activeBrowser = browser;
  const context = await browser.newContext({ viewport: { width: 1680, height: 1050 }, locale: "zh-CN", timezoneId: "Asia/Shanghai" });
  const errors = [];
  const page = await context.newPage();
  page.setDefaultTimeout(15_000);
  page.on("pageerror", (error) => errors.push(error.message));
  await context.route("**/*", async (route) => {
    const url = new URL(route.request().url());
    if (url.origin === origin || ["data:", "blob:"].includes(url.protocol)) return route.continue();
    blockedExternal.add(url.origin);
    return route.abort("blockedbyclient");
  });
  const token = [Buffer.from('{"alg":"none"}').toString("base64url"), Buffer.from('{"sub":"ui-fixture-admin","exp":4102444800}').toString("base64url"), "fixture"].join(".");
  await page.addInitScript(({ token }) => {
    localStorage.setItem("artex_token", token);
    document.cookie = "artex_session=1; path=/; SameSite=Lax";
    document.cookie = "sidebar_state=false; path=/; SameSite=Lax";
    window.__fixtureClipboard = [];
    window.__fixtureCopyFails = false;
    Object.defineProperty(navigator, "clipboard", { configurable: true, value: { writeText: async (text) => {
      if (window.__fixtureCopyFails) throw new Error("Synthetic copy failure");
      window.__fixtureClipboard.push(text);
    } } });
    const execCommand = document.execCommand.bind(document);
    document.execCommand = (...arguments_) => arguments_[0] === "copy" && window.__fixtureCopyFails ? false : execCommand(...arguments_);
  }, { token });

  const secretAbsent = async () => !(await page.locator("body").innerText()).includes(FIXTURE_SECRET);
  const revealRequests = () => requests.filter((item) => item.path.endsWith("/reveal"));
  const expectRows = (count) => eventually(async () => (await page.locator("tbody tr").count()) === count, `Expected ${count} credential rows`);
  const expectTaskLink = async (link, taskId) => {
    const href = await link.getAttribute("href");
    assert.ok(href, "Task navigation link is missing");
    const destination = new URL(href, origin);
    assert.equal(destination.origin, origin, "Task navigation must remain within this application");
    // Next static export normalizes internal Links to a trailing slash.
    assert.equal(destination.pathname.replace(/\/+$/, ""), "/function/tasks/detail");
    assert.deepEqual([...destination.searchParams.entries()], [["id", String(taskId)]]);
    assert.equal(destination.hash, "");
  };
  const openPrimary = async () => {
    await page.getByTestId("credential-details-23").click();
    const dialog = page.getByRole("dialog");
    await dialog.getByRole("heading", { name: /^凭据 #23/ }).waitFor();
    return dialog;
  };
  const closeDialog = async () => {
    await page.keyboard.press("Escape");
    await page.getByRole("dialog").waitFor({ state: "hidden" });
  };
  const selectOption = async (trigger, name) => {
    await trigger.click();
    await page.getByRole("option", { name, exact: true }).click();
  };

  try {
    await page.goto(origin + "/intranet/", { waitUntil: "networkidle" });
    await page.getByRole("tab", { name: "凭据", exact: true }).click();
    await page.getByTestId("credential-details-23").waitFor();

    await check("masked list, task navigation and distinct target/authentication domain", async () => {
      assert.ok(await secretAbsent(), "Full fixture secret appeared before explicit reveal");
      assert.equal(revealRequests().length, 0, "List requested full secrets automatically");
      const row = page.getByRole("row").filter({ hasText: PRIMARY_USERNAME });
      await expectTaskLink(row.getByRole("link", { name: `#101 · ${TASK_A}`, exact: true }), 101);
      assert.ok((await row.innerText()).includes("target.example.invalid"));
      assert.ok((await row.innerText()).includes("CORP"));
      const external = row.getByRole("link", { name: "打开目标", exact: true });
      assert.equal(await external.getAttribute("href"), "https://target.example.invalid/console");
      assert.equal(await external.getAttribute("referrerpolicy"), "no-referrer");
      assert.ok((await external.getAttribute("rel")).includes("noreferrer"));
      assert.equal(await page.getByRole("button", { name: /资产与会话拓扑/ }).getAttribute("aria-expanded"), "false");
    });

    await check("search, type/status filters and exact pagination", async () => {
      await expectRows(20);
      await page.getByRole("button", { name: "下一页", exact: true }).click();
      await expectRows(3);
      const orphan = page.getByRole("row").filter({ hasText: "fixture-user-1" });
      assert.ok((await orphan.innerText()).includes("任务当前不可操作"));
      assert.equal(await orphan.getByRole("link").count(), 0);
      await page.getByRole("button", { name: "上一页", exact: true }).click();
      await page.getByRole("textbox", { name: "搜索凭据", exact: true }).fill("CORP");
      await expectRows(1);
      await page.getByRole("button", { name: "清空筛选", exact: true }).first().click();
      await selectOption(page.getByRole("combobox", { name: "凭据类型", exact: true }), "token");
      await expectRows(1);
      await page.getByRole("cell", { name: "fixture-bob", exact: true }).waitFor();
      await page.getByRole("button", { name: "清空筛选", exact: true }).first().click();
      await selectOption(page.getByRole("combobox", { name: "验证状态", exact: true }), "验证失败");
      await expectRows(1);
      await page.getByRole("cell", { name: "fixture-charlie", exact: true }).waitFor();
      await page.getByRole("button", { name: "清空筛选", exact: true }).first().click();
      await page.getByRole("textbox", { name: "搜索凭据", exact: true }).fill("nothing-matches-this-fixture");
      await page.getByText("没有匹配的凭据，请调整或清空筛选。", { exact: true }).waitFor();
      await page.getByRole("button", { name: "清空筛选", exact: true }).first().click();
    });

    await check("source details preserve description and sanitize explicit external links", async () => {
      const dialog = await openPrimary();
      await dialog.getByText(PRIMARY_SOURCE, { exact: true }).waitFor();
      assert.equal(await dialog.getByRole("link", { name: "打开来源（不含查询参数）", exact: true }).getAttribute("href"), "https://source.example.invalid/evidence");
      await expectTaskLink(dialog.getByRole("link", { name: "回到所属任务", exact: true }), 101);
      assert.ok((await dialog.innerText()).includes("意图 #723"));
      assert.ok(await secretAbsent());
      await closeDialog();
    });

    await check("metadata edits persist separately from immutable source and secret", async () => {
      const dialog = await openPrimary();
      await dialog.locator("#credential-target").fill("updated.example.invalid");
      await dialog.locator("#credential-domain").fill("NEWCORP");
      await dialog.locator("#credential-source-url").fill("https://source.example.invalid/new-evidence?token=fixture-query#fragment");
      await dialog.getByRole("button", { name: "保存关联信息", exact: true }).click();
      await page.getByText("凭据关联信息已保存", { exact: true }).waitFor();
      const saved = requests.filter((item) => item.path.endsWith("/metadata")).at(-1);
      assert.deepEqual(saved.body, { target: "updated.example.invalid", domain: "NEWCORP", source_url: "https://source.example.invalid/new-evidence" });
      assert.equal(records[22].source, PRIMARY_SOURCE);
      assert.ok(await secretAbsent());
      await closeDialog();
    });

    await check("manual verification records evidence/history without target traffic", async () => {
      const dialog = await openPrimary();
      await selectOption(dialog.locator("#credential-verification-status"), "验证通过");
      await dialog.locator("#credential-verification-method").fill("Synthetic existing login observation");
      await dialog.locator("#credential-verification-evidence").fill("UI-EVIDENCE-NEW: synthetic response was previously observed");
      state.failVerification = true;
      await dialog.getByRole("button", { name: "保存验证记录", exact: true }).click();
      await page.getByText("fixture verification save failed", { exact: true }).waitFor();
      assert.equal(records[22].verification_status, "unverified", "Failed save changed the stored status");
      await dialog.getByText("未验证", { exact: true }).waitFor();
      state.failVerification = false;
      await dialog.getByRole("button", { name: "保存验证记录", exact: true }).click();
      await page.getByText("验证记录已保存（未发起网络验证）", { exact: true }).waitFor();
      await eventually(() => records[22].verification_status === "verified", "Verification result was not saved");
      assert.equal(records[22].verification_history.length, 1);
      await selectOption(dialog.locator("#credential-verification-status"), "未验证");
      await dialog.getByRole("button", { name: "保存验证记录", exact: true }).click();
      await eventually(() => records[22].verification_history.length === 2, "Reset did not preserve verification history");
      assert.equal(records[22].verification_status, "unverified");
      assert.equal(records[22].verification_method, "");
      await dialog.getByText("查看历史验证记录（2）", { exact: true }).click();
      await dialog.getByText("证据：UI-EVIDENCE-NEW: synthetic response was previously observed", { exact: true }).waitFor();
      assert.ok(requests.filter((item) => item.path.endsWith("/verification")).every((item) => !JSON.stringify(item.body).includes(FIXTURE_SECRET)));
      await closeDialog();
    });

    await check("explicit reveal expires and never writes secret to attributes or storage", async () => {
      const dialog = await openPrimary();
      await dialog.getByRole("button", { name: "查看 30 秒", exact: true }).click();
      await dialog.locator("pre").filter({ hasText: FIXTURE_SECRET }).waitFor();
      const leaks = await page.evaluate((secret) => ({
        attributes: [...document.querySelectorAll("*")].some((element) => [...element.attributes].some((attribute) => attribute.value.includes(secret))),
        local: JSON.stringify(Object.entries(localStorage)).includes(secret),
        session: JSON.stringify(Object.entries(sessionStorage)).includes(secret),
        cookies: document.cookie.includes(secret),
      }), FIXTURE_SECRET);
      assert.deepEqual(leaks, { attributes: false, local: false, session: false, cookies: false });
      // Exercise the actual component deadline without overriding browser clocks.
      await new Promise((resolve) => setTimeout(resolve, 31_000));
      await eventually(secretAbsent, "Revealed secret remained visible after 30 seconds");
      await closeDialog();
    });

    await check("page visibility, close and late reveal responses clear the displayed secret", async () => {
      let dialog = await openPrimary();
      await dialog.getByRole("button", { name: "查看 30 秒", exact: true }).click();
      await dialog.locator("pre").filter({ hasText: FIXTURE_SECRET }).waitFor();
      await page.evaluate(() => {
        Object.defineProperty(document, "visibilityState", { configurable: true, get: () => "hidden" });
        document.dispatchEvent(new Event("visibilitychange"));
      });
      await eventually(secretAbsent, "Hidden-page event did not clear displayed secret");
      await page.evaluate(() => {
        Object.defineProperty(document, "visibilityState", { configurable: true, get: () => "visible" });
        document.dispatchEvent(new Event("visibilitychange"));
      });
      await dialog.getByRole("button", { name: "查看 30 秒", exact: true }).click();
      await dialog.locator("pre").filter({ hasText: FIXTURE_SECRET }).waitFor();
      await closeDialog();
      assert.ok(await secretAbsent());
      dialog = await openPrimary();
      assert.ok(await secretAbsent());
      const previous = revealRequests().length;
      state.delayNextReveal = 350;
      await dialog.getByRole("button", { name: "查看 30 秒", exact: true }).click();
      await eventually(() => revealRequests().length > previous, "Delayed reveal request was not sent");
      await closeDialog();
      await new Promise((resolve) => setTimeout(resolve, 450));
      assert.ok(await secretAbsent(), "Late reveal response restored a closed dialog's secret");
      dialog = await openPrimary();
      assert.ok(await secretAbsent(), "Reopened dialog retained the old secret");
      await closeDialog();
    });

    await check("copy is explicit, masked afterwards and truthful on clipboard failure", async () => {
      const dialog = await openPrimary();
      await dialog.getByRole("button", { name: "复制完整值", exact: true }).click();
      await page.getByText("完整凭据已复制；剪贴板不会自动清除，请自行处理。", { exact: true }).waitFor();
      assert.ok(await secretAbsent());
      assert.ok(await page.evaluate((secret) => window.__fixtureClipboard.at(-1) === secret, FIXTURE_SECRET));
      assert.equal(revealRequests().at(-1).body.purpose, "copy");
      await page.evaluate(() => { window.__fixtureCopyFails = true; });
      await dialog.getByRole("button", { name: "复制完整值", exact: true }).click();
      await page.getByText("复制失败，请查看后手动复制。", { exact: true }).waitFor();
      await page.evaluate(() => { window.__fixtureCopyFails = false; });
      assert.ok(await secretAbsent());
      await closeDialog();
    });

    await check("task/tab changes and refresh failures do not leak or erase known records", async () => {
      await page.getByRole("tab", { name: "会话", exact: true }).click();
      await page.getByRole("tab", { name: "凭据", exact: true }).click();
      assert.equal(await page.getByRole("dialog").count(), 0);
      assert.ok(await secretAbsent());
      await selectOption(page.getByRole("combobox").first(), TASK_B);
      await page.getByTestId("credential-details-22").waitFor();
      await expectRows(1);
      assert.ok((await page.locator("tbody").innerText()).includes("fixture-bob"));
      state.failCredentials = true;
      await page.getByRole("button", { name: "刷新", exact: true }).click();
      await page.getByRole("alert").getByText("凭据：fixture credential refresh failed", { exact: true }).waitFor();
      assert.ok((await page.locator("tbody").innerText()).includes("fixture-bob"), "Refresh failure erased known records");
      state.failCredentials = false;
      await page.getByRole("button", { name: "刷新", exact: true }).click();
      await page.getByRole("alert").waitFor({ state: "hidden" });
      await selectOption(page.getByRole("combobox").first(), "全部任务");
      await page.getByTestId("credential-details-23").waitFor();
      assert.ok(await secretAbsent());
    });

    await check("no unexpected browser exceptions, outbound traffic or API calls", async () => {
      assert.equal(errors.length, 0, `Unexpected browser exceptions: ${errors.join("; ")}`);
      assert.equal(blockedExternal.size, 0, `UI attempted outbound requests: ${[...blockedExternal].join(", ")}`);
      assert.equal(unexpectedAPI.size, 0, `Unmocked API calls: ${[...unexpectedAPI].join(", ")}`);
    });

    await page.screenshot({ path: path.join(artifacts, "credential-list-desktop.png"), fullPage: true });
    const dialog = await openPrimary();
    assert.ok(await secretAbsent());
    await dialog.screenshot({ path: path.join(artifacts, "credential-details-masked.png") });
    await closeDialog();
    await page.setViewportSize({ width: 390, height: 844 });
    await page.screenshot({ path: path.join(artifacts, "credential-list-mobile.png"), fullPage: true });
    fs.writeFileSync(path.join(artifacts, "result.json"), JSON.stringify({ passed: completed, requests: requests.map(({ method, path: endpoint, task }) => ({ method, path: endpoint, task })), outboundRequests: [...blockedExternal] }, null, 2));
    console.log(`Credential UI smoke passed: ${completed.length} scenarios; artifacts contain synthetic fixtures only.`);
  } catch (error) {
    await page.screenshot({ path: path.join(artifacts, "credential-ui-failure.png"), fullPage: true }).catch(() => {});
    fs.writeFileSync(path.join(artifacts, "failure.json"), JSON.stringify({ passed: completed, error: error.message, browserErrors: errors, unexpectedAPI: [...unexpectedAPI], outboundRequests: [...blockedExternal] }, null, 2));
    throw error;
  } finally {
    await browser.close();
    server.closeAllConnections();
    await new Promise((resolve) => server.close(resolve));
  }
}

main().catch(async (error) => {
  await activeBrowser?.close().catch(() => {});
  server.closeAllConnections();
  server.close();
  console.error(error.stack || error.message);
  process.exitCode = 1;
});
