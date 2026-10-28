#!/usr/bin/env node
/*
 * Cloud-only browser regression for the exported credential and Agent UIs.
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
if (!fs.existsSync(path.join(outputDirectory, "system", "agents", "detail", "index.html"))) {
  throw new Error("Missing static Agent detail export.");
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
const AGENT_GUIDANCE_VERSION = "ui-fixture-v1";
const AGENT_GUIDANCE = "\n\n【角色工作规范 · worker · ui-fixture-v1】\n优先既有工具，记录真实证据和失败判据；保留任务范围、审批和停止条件。";
const agentFixtures = {
  worker: {
    agent: { id: "11", key: "worker", name: "执行", role: "worker", builtin: true, enabled: true, max_turns: 7, run_seconds: 600, web_search: false, interactive_shell: false },
    prompt: "FIXTURE_CUSTOM_WORKER_BODY\n工作目录：{{.DataDir}}",
    variables: [{ name: "DataDir", description: "虚构工作目录", example: "/fixture/data", source: "runtime" }],
    versions: [{ version: 7, ts: fixtureTime, note: "fixture original", template_text: "FIXTURE_CUSTOM_WORKER_BODY\n工作目录：{{.DataDir}}" }],
    visibility: { mcp: [71], skill: ["fixture-skill"] },
    llm_profiles: [],
    work_guidance_version: AGENT_GUIDANCE_VERSION,
    work_guidance: AGENT_GUIDANCE,
  },
  fixture_custom: {
    agent: { id: "12", key: "fixture_custom", name: "自定义回归角色", role: "assistant", builtin: false, enabled: true, max_turns: 3, run_seconds: 600, web_search: false, interactive_shell: false },
    prompt: "FIXTURE_CUSTOM_ROLE_BODY",
    variables: [],
    versions: [{ version: 2, ts: fixtureTime, note: "fixture custom original", template_text: "FIXTURE_CUSTOM_ROLE_BODY" }],
    visibility: { mcp: [], skill: [] },
    llm_profiles: [],
    work_guidance_version: "",
    work_guidance: "",
  },
};
const runtimeAgentFixtures = [
  { ...agentFixtures.worker.agent, mcp_count: 4, skill_count: 3, tool_count: 8, binding_counts_available: true,
    capabilities: { execution_agent_key: "worker", tool_binding_mode: "direct", mcp_binding_mode: "direct", skill_binding_mode: "direct", runtime_config_mode: "direct" } },
  { id: "21", key: "worker.intranet", name: "执行·内网", role: "worker", builtin: true, enabled: true, mcp_count: 4, skill_count: 2, tool_count: 0, binding_counts_available: true,
    capabilities: { execution_agent_key: "worker", tool_binding_mode: "inherited", mcp_binding_mode: "inherited", skill_binding_mode: "direct", runtime_config_mode: "inherited", runtime_config_agent_key: "worker", note: "执行工具与 MCP 沿用 worker；Skill 单独授权。" } },
  { id: "22", key: "goals", name: "目标拆解", role: "goals", builtin: true, enabled: true, mcp_count: 0, skill_count: 2, tool_count: 3, binding_counts_available: true,
    capabilities: { execution_agent_key: "goals", tool_binding_mode: "limited", mcp_binding_mode: "unsupported", skill_binding_mode: "readonly", runtime_config_mode: "llm_only", runtime_config_agent_key: "goals", allowed_tool_keys: ["set_goals", "set_constraints", "add_task_scope"], note: "目标拆解仅使用有限基础工具与只读 Skill 指导，不开放 Bash、交互 Shell 或 MCP。" } },
  { id: "23", key: "credential_processor", name: "凭据整理", role: "assistant", builtin: false, enabled: true, mcp_count: 0, skill_count: 0, tool_count: 0, binding_counts_available: true,
    capabilities: { execution_agent_key: "credential_processor", tool_binding_mode: "unsupported", mcp_binding_mode: "unsupported", skill_binding_mode: "unsupported", runtime_config_mode: "llm_only", note: "凭据整理仅做无工具的结构化模型调用，不执行命令或加载执行依赖。" },
    automation: { kind: "credential_processing", available: true, enabled: false, configured: false, state: "disabled", reason: "fixture 凭据整理总开关未开启" } },
  { id: "24", key: "verifier", name: "漏洞验证", role: "assistant", builtin: false, enabled: true, mcp_count: 4, skill_count: 2, tool_count: 2, binding_counts_available: true,
    automation: { kind: "finding_verification", available: false, enabled: false, configured: true, state: "unknown", reason: "fixture trigger state unavailable" } },
  { ...agentFixtures.fixture_custom.agent, mcp_count: 0, skill_count: 0, tool_count: 0, binding_counts_available: false, binding_counts_error: "fixture binding statistics unavailable" },
];
const toolFixtures = [
  { key: "external_fixture_cli", system: false, kind: "shell", description: "Synthetic environment declaration; this fixture never executes it.", schema: {}, agents: ["worker", "worker.intranet"], enabled: true, deferred: false, exec: {}, calls: null, usage_kind: "shell_declaration", usage_available: false },
  { key: "fixture_callable_zero", system: true, kind: "builtin", description: "Synthetic callable with a known zero invocation count.", schema: { type: "object", properties: {} }, agents: ["worker"], enabled: true, deferred: false, calls: 0, usage_kind: "callable", usage_available: true },
  { key: "fixture_callable_unknown", system: true, kind: "builtin", description: "Synthetic callable with unavailable statistics, not a zero count.", schema: { type: "object", properties: {} }, agents: ["worker"], enabled: true, deferred: false, calls: null, usage_kind: "callable", usage_available: false, usage_error: "fixture tool statistics unavailable" },
];
const skillFixtures = [
  { name: "fixture-skill", declared_name: "fixture-skill", description: "Synthetic explicit-load fixture.", files: ["SKILL.md", "refs/fixture.md"], calls: 3, tasks: 1, usage_agents: ["worker"], last_used: fixtureTime, usage_available: true, loadable: true, mcps: [] },
  { name: "chains-web", declared_name: "chains-web", description: "Synthetic built-in rules and explicit-load accounting are distinct.", files: ["SKILL.md"], calls: 0, tasks: 0, usage_agents: [], usage_available: true, loadable: true, mcps: [] },
  { name: "fixture-broken", description: "Synthetic malformed metadata fixture.", files: ["SKILL.md"], calls: 0, tasks: 0, usage_agents: [], usage_available: true, loadable: false, load_error: "fixture invalid Skill metadata", mcps: [] },
];
const requests = [];
const unexpectedAPI = new Set();
const blockedExternal = new Set();
const state = {
  failCredentials: false, failVerification: false, delayNextReveal: 0, failAgentPreview: false,
  failToolsList: false, failSkillSummary: false,
  failSkillUsage: new Set(), failSkillVisibility: new Set(),
  delayNextSkillUsage: "", delayNextSkillFile: "",
};
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
  if (endpoint === "/api/agents" && method === "GET") return sendJSON(response, 200, { agents: runtimeAgentFixtures });
  if (endpoint === "/api/mcp" && method === "GET") return sendJSON(response, 200, { servers: [] });
  if (endpoint === "/api/skills" && method === "GET") return sendJSON(response, 200, {
    skills: skillFixtures.map((item) => state.failSkillSummary ? { ...item, calls: null, usage_available: false, usage_error: "fixture Skill statistics unavailable" } : item),
    usage_available: !state.failSkillSummary, usage_error: state.failSkillSummary ? "fixture Skill statistics unavailable" : "", registry_available: true,
  });
  if (endpoint === "/api/skills/missing" && method === "GET") return sendJSON(response, 200, { missing: [] });
  const skillAction = endpoint.match(/^\/api\/skills\/(fixture-skill|chains-web|fixture-broken)(?:\/(usage|files)(?:\/(.+))?)?$/);
  if (skillAction && method === "GET") {
    const name = skillAction[1];
    if (skillAction[2] === "usage") {
      const calls = name === "fixture-skill" ? [{ ts: fixtureTime, agent_key: "fixture-explicit-history", task_id: 777, session_id: "", args_len: 0 }] : [];
      if (state.delayNextSkillUsage === name) {
        state.delayNextSkillUsage = "";
        await new Promise((resolve) => setTimeout(resolve, 700));
      }
      if (state.failSkillUsage.has(name)) return sendJSON(response, 503, { error: "fixture usage unavailable" });
      return sendJSON(response, 200, { calls, usage_available: true, scope: "live" });
    }
    if (skillAction[2] === "files" && skillAction[3]) {
      const content = `# FIXTURE FILE ${name}\nSynthetic file content only.`;
      if (state.delayNextSkillFile === name) {
        state.delayNextSkillFile = "";
        await new Promise((resolve) => setTimeout(resolve, 700));
      }
      return sendJSON(response, 200, { content, file: skillAction[3] });
    }
    if (skillAction[2] === "files") return sendJSON(response, 200, { files: skillFixtures.find((item) => item.name === name).files });
  }
  const skillVisibility = endpoint.match(/^\/api\/visibility\/skill\/(fixture-skill|chains-web|fixture-broken)$/);
  if (skillVisibility && method === "GET") {
    if (state.failSkillVisibility.has(skillVisibility[1])) return sendJSON(response, 503, { error: "fixture visibility unavailable" });
    return sendJSON(response, 200, { agents: skillVisibility[1] === "fixture-broken" ? [] : ["11"] });
  }
  if (endpoint === "/api/tools" && method === "GET") {
    if (state.failToolsList) return sendJSON(response, 503, { error: "fixture tools refresh unavailable" });
    return sendJSON(response, 200, { tools: toolFixtures });
  }
  if (endpoint === "/api/settings" && method === "GET") return sendJSON(response, 200, { traffic_capture: false, web_search_enabled: false });
  const agentAction = endpoint.match(/^\/api\/agents\/(worker|fixture_custom)(?:\/(prompt(?:\/preview)?|triggers))?$/);
  if (agentAction) {
    const fixture = agentFixtures[agentAction[1]];
    if (!agentAction[2] && method === "GET") return sendJSON(response, 200, fixture);
    if (agentAction[2] === "triggers" && method === "GET") return sendJSON(response, 200, { triggers: [] });
    if (agentAction[2] === "prompt/preview" && method === "POST") {
      const guidance = { work_guidance_version: fixture.work_guidance_version, work_guidance: fixture.work_guidance };
      if (state.failAgentPreview) return sendJSON(response, 200, { rendered: "", error: "fixture template preview failed", ...guidance });
      return sendJSON(response, 200, { rendered: body.template.replaceAll("{{.DataDir}}", "/fixture/data"), ...guidance });
    }
    if (agentAction[2] === "prompt" && method === "PUT") {
      assert.deepEqual(Object.keys(body).sort(), ["note", "template"], "Prompt saving must submit the editable body only");
      assert.equal(typeof body.template, "string");
      assert.ok(!body.template.includes(AGENT_GUIDANCE), "Runtime guidance leaked into the saved prompt body");
      const version = fixture.versions[0].version + 1;
      fixture.prompt = body.template;
      fixture.versions.unshift({ version, ts: fixtureTime, note: "fixture saved", template_text: body.template });
      return sendJSON(response, 200, { version });
    }
  }
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
      await dialog.locator('[data-slot="badge"]').filter({ hasText: /^未验证$/ }).waitFor();
      state.failVerification = false;
      await dialog.getByRole("button", { name: "保存验证记录", exact: true }).click();
      await page.getByText("验证记录已保存（未发起网络验证）", { exact: true }).waitFor();
      await eventually(() => records[22].verification_status === "verified", "Verification result was not saved");
      await dialog.locator('[data-slot="badge"]').filter({ hasText: /^验证通过$/ }).waitFor();
      assert.equal(records[22].verification_history.length, 1);
      await selectOption(dialog.locator("#credential-verification-status"), "未验证");
      await dialog.getByRole("button", { name: "保存验证记录", exact: true }).click();
      await eventually(() => records[22].verification_history.length === 2, "Reset did not preserve verification history");
      assert.equal(records[22].verification_status, "unverified");
      assert.equal(records[22].verification_method, "");
      await dialog.locator('[data-slot="badge"]').filter({ hasText: /^未验证$/ }).waitFor();
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
      const loadAlert = page.getByRole("alert").filter({ hasText: "部分数据加载失败" });
      await loadAlert.getByText("凭据：fixture credential refresh failed", { exact: true }).waitFor();
      assert.ok((await page.locator("tbody").innerText()).includes("fixture-bob"), "Refresh failure erased known records");
      state.failCredentials = false;
      await page.getByRole("button", { name: "刷新", exact: true }).click();
      await loadAlert.waitFor({ state: "hidden" });
      await selectOption(page.getByRole("combobox").first(), "全部任务");
      await page.getByTestId("credential-details-23").waitFor();
      assert.ok(await secretAbsent());
    });

    await check("no unexpected browser exceptions, outbound traffic or API calls", async () => {
      assert.equal(errors.length, 0, `Unexpected browser exceptions: ${errors.join("; ")}`);
      assert.equal(blockedExternal.size, 0, `UI attempted outbound requests: ${[...blockedExternal].join(", ")}`);
      assert.equal(unexpectedAPI.size, 0, `Unmocked API calls: ${[...unexpectedAPI].join(", ")}`);
    });

    // Allow the intentionally exercised failure toasts to expire before visual QA.
    await new Promise((resolve) => setTimeout(resolve, 5000));
    await page.screenshot({ path: path.join(artifacts, "credential-list-desktop.png"), fullPage: true });
    const dialog = await openPrimary();
    assert.ok(await secretAbsent());
    await dialog.screenshot({ path: path.join(artifacts, "credential-details-masked.png") });
    await closeDialog();
    await page.setViewportSize({ width: 390, height: 844 });
    await check("compact mobile header remains horizontal without page overflow", async () => {
      const heading = await page.getByRole("heading", { name: "内网作战", exact: true }).boundingBox();
      assert.ok(heading && heading.height <= 40 && heading.width >= 70, "Mobile title was squeezed into vertical text");
      assert.ok(await page.evaluate(() => document.documentElement.scrollWidth <= window.innerWidth + 1), "Mobile page overflowed horizontally outside the table scroller");
      assert.ok(await page.getByRole("button", { name: "刷新", exact: true }).isVisible());
    });
    await page.screenshot({ path: path.join(artifacts, "credential-list-mobile.png"), fullPage: true });

    await page.setViewportSize({ width: 1680, height: 1050 });
    await check("Agent runtime guidance is read-only and separate from editable and previewed bodies", async () => {
      await page.goto(origin + "/system/agents/detail/?key=worker", { waitUntil: "networkidle" });
      const bodyEditor = page.getByRole("textbox", { name: "提示词正文", exact: true });
      await eventually(async () => (await bodyEditor.inputValue()) === agentFixtures.worker.prompt, "Agent custom prompt did not load");
      const panel = page.getByTestId("agent-work-guidance");
      assert.equal(await panel.count(), 1);
      assert.ok((await panel.locator("summary").innerText()).includes(AGENT_GUIDANCE_VERSION));
      await panel.locator("summary").click();
      await panel.locator("pre").waitFor();
      assert.equal((await panel.locator("pre").innerText()).trim(), AGENT_GUIDANCE.trim());
      assert.equal(await panel.getByRole("textbox").count(), 0, "Runtime guidance must not be editable");
      assert.ok(!(await bodyEditor.inputValue()).includes(AGENT_GUIDANCE_VERSION));
      assert.ok((await panel.innerText()).includes("保留自定义提示词正文与历史版本"));
      await page.screenshot({ path: path.join(artifacts, "agent-work-guidance.png"), fullPage: true });

      await page.getByRole("button", { name: "预览渲染", exact: true }).click();
      let preview = page.getByRole("dialog");
      await preview.getByText("FIXTURE_CUSTOM_WORKER_BODY", { exact: false }).waitFor();
      const previewPanel = preview.getByTestId("agent-work-guidance");
      await previewPanel.locator("summary").click();
      assert.equal((await previewPanel.locator("pre").innerText()).trim(), AGENT_GUIDANCE.trim());
      assert.ok((await preview.innerText()).includes("正文预览"));
      assert.equal(requests.filter((item) => item.path.endsWith("/prompt/preview")).at(-1).body.template, agentFixtures.worker.prompt);
      await closeDialog();

      state.failAgentPreview = true;
      await page.getByRole("button", { name: "预览渲染", exact: true }).click();
      preview = page.getByRole("dialog");
      await preview.getByText("渲染错误：fixture template preview failed", { exact: true }).waitFor();
      const errorPanel = preview.getByTestId("agent-work-guidance");
      await errorPanel.locator("summary").click();
      assert.equal((await errorPanel.locator("pre").innerText()).trim(), AGENT_GUIDANCE.trim());
      await closeDialog();
      state.failAgentPreview = false;
      assert.equal(await bodyEditor.inputValue(), agentFixtures.worker.prompt, "Preview changed the editable prompt");
    });

    await check("saving an Agent prompt preserves runtime guidance, history and permissions independently", async () => {
      const beforeAgent = structuredClone(agentFixtures.worker.agent);
      const beforeVisibility = structuredClone(agentFixtures.worker.visibility);
      const priorVersion = structuredClone(agentFixtures.worker.versions[0]);
      const editedBody = "FIXTURE_EDITED_WORKER_BODY\n工作目录：{{.DataDir}}";
      const bodyEditor = page.getByRole("textbox", { name: "提示词正文", exact: true });
      await bodyEditor.fill(editedBody);
      await page.getByRole("button", { name: "保存为新版本", exact: true }).click();
      await eventually(() => agentFixtures.worker.prompt === editedBody, "Agent prompt save did not persist");
      await page.getByText(`v${agentFixtures.worker.versions[0].version}`, { exact: true }).waitFor();
      await eventually(async () => (await bodyEditor.inputValue()) === editedBody, "Saved Agent prompt did not reload");
      const saved = requests.filter((item) => item.method === "PUT" && item.path === "/api/agents/worker/prompt").at(-1);
      assert.deepEqual(saved.body, { template: editedBody, note: "" });
      assert.equal(agentFixtures.worker.versions.length, 2);
      assert.deepEqual(agentFixtures.worker.versions[1], priorVersion, "Prompt save rewrote prior history");
      assert.deepEqual(agentFixtures.worker.agent, beforeAgent);
      assert.deepEqual(agentFixtures.worker.visibility, beforeVisibility);
      assert.equal(agentFixtures.worker.work_guidance, AGENT_GUIDANCE);
      assert.ok(!agentFixtures.worker.versions[0].template_text.includes(AGENT_GUIDANCE_VERSION));
      assert.equal(requests.filter((item) => item.method !== "GET" && /^\/api\/(agents\/worker\/(config|visibility)|tools|settings)/.test(item.path)).length, 0, "Prompt editing changed permissions or model settings");
    });

    await check("unknown custom Agent roles keep their body without a runtime guidance panel", async () => {
      await page.goto(origin + "/system/agents/detail/?key=fixture_custom", { waitUntil: "networkidle" });
      const bodyEditor = page.getByRole("textbox", { name: "提示词正文", exact: true });
      await eventually(async () => (await bodyEditor.inputValue()) === agentFixtures.fixture_custom.prompt, "Custom Agent body did not load");
      assert.equal(await page.getByTestId("agent-work-guidance").count(), 0);
      assert.ok(!(await page.locator("body").innerText()).includes(AGENT_GUIDANCE_VERSION));
      await page.getByRole("button", { name: "预览渲染", exact: true }).click();
      const preview = page.getByRole("dialog");
      await preview.getByText("FIXTURE_CUSTOM_ROLE_BODY", { exact: true }).waitFor();
      assert.equal(await preview.getByTestId("agent-work-guidance").count(), 0);
      await closeDialog();
      assert.equal(agentFixtures.fixture_custom.versions.length, 1);
    });

    await check("Agent pages preserve zero browser exceptions, outbound traffic and unexpected API calls", async () => {
      assert.equal(errors.length, 0, `Unexpected browser exceptions: ${errors.join("; ")}`);
      assert.equal(blockedExternal.size, 0, `UI attempted outbound requests: ${[...blockedExternal].join(", ")}`);
      assert.equal(unexpectedAPI.size, 0, `Unmocked API calls: ${[...unexpectedAPI].join(", ")}`);
      assert.ok(await secretAbsent());
    });

    await check("tool cards distinguish environment declarations, known zero and unknown statistics", async () => {
      await page.goto(origin + "/system/tools/", { waitUntil: "networkidle" });
      await page.getByTestId("tool-card-fixture_callable_zero").waitFor();
      assert.equal(await page.getByTestId("tool-usage-fixture_callable_zero").innerText(), "目录调用 0");
      assert.equal(await page.getByTestId("tool-usage-fixture_callable_unknown").innerText(), "统计不可用");
      await page.getByRole("tab", { name: "自定义工具", exact: true }).click();
      const declaration = page.getByTestId("tool-card-external_fixture_cli");
      await declaration.waitFor();
      assert.equal(await declaration.getByTestId("tool-usage-external_fixture_cli").innerText(), "环境声明");
      assert.ok((await declaration.innerText()).includes("独立调用次数不适用"));
      assert.ok(!(await declaration.innerText()).includes("调用 0"));
      await declaration.click();
      const editor = page.getByRole("dialog");
      const inherited = editor.getByRole("checkbox", { name: "worker.intranet 工具绑定", exact: true });
      assert.ok(await inherited.isDisabled(), "Variant-only bindings must not be silently editable as independent grants");
      assert.ok(await inherited.isChecked(), "Existing inactive variant binding must remain displayed instead of being silently removed");
      assert.ok(await editor.getByRole("checkbox", { name: "credential_processor 工具绑定", exact: true }).isDisabled());
      assert.ok(await editor.getByRole("checkbox", { name: "goals 工具绑定", exact: true }).isDisabled());
      await closeDialog();
      await page.screenshot({ path: path.join(artifacts, "tools-environment-declaration.png"), fullPage: true });
    });

    await check("tool list read failures preserve loaded records without mutating bindings", async () => {
      await page.getByRole("tab", { name: "系统工具", exact: true }).click();
      state.failToolsList = true;
      await page.getByRole("button", { name: "刷新", exact: true }).click();
      await page.getByTestId("tools-load-error").getByText("工具：fixture tools refresh unavailable", { exact: true }).waitFor();
      await page.getByTestId("tool-card-fixture_callable_zero").waitFor();
      assert.equal(await page.getByTestId("tool-usage-fixture_callable_zero").innerText(), "目录调用 0");
      state.failToolsList = false;
      await page.getByRole("button", { name: "刷新", exact: true }).click();
      await page.getByTestId("tools-load-error").waitFor({ state: "hidden" });
      assert.equal(requests.filter((item) => item.method !== "GET" && /^\/api\/tools(?:\/|$)/.test(item.path)).length, 0, "Viewing tool readiness altered tool bindings");
    });

    await check("Agent cards explain inherited tools, restricted goals and no-tool credential processing", async () => {
      await page.goto(origin + "/system/agents/", { waitUntil: "networkidle" });
      const inherited = page.getByTestId("agent-card-worker.intranet");
      await inherited.waitFor();
      assert.ok((await inherited.innerText()).includes("工具沿用 worker"));
      assert.ok((await inherited.innerText()).includes("MCP 沿用 worker"));
      const goals = page.getByTestId("agent-card-goals");
      assert.ok((await goals.innerText()).includes("Skill 指导 2"));
      assert.ok((await goals.innerText()).includes("基础工具绑定 3"));
      assert.ok((await goals.innerText()).includes("不开放 Bash"));
      const processor = page.getByTestId("agent-card-credential_processor");
      assert.ok((await processor.innerText()).includes("无执行工具"));
      assert.ok((await processor.innerText()).includes("不使用 Skill"));
      assert.ok((await processor.innerText()).includes("结构化模型调用"));
      assert.ok((await page.getByTestId("agent-automation-credential_processor").innerText()).includes("凭据整理总开关未开启"));
      assert.ok((await page.getByTestId("agent-automation-verifier").innerText()).includes("自动流程状态未知"));
      const unknownBindings = page.getByTestId("agent-card-fixture_custom");
      assert.ok((await unknownBindings.innerText()).includes("绑定统计不可用"));
      assert.ok(!(await unknownBindings.innerText()).includes("工具绑定 0"));
    });

    await check("Skill overview describes explicit loads and exposes invalid directories", async () => {
      await page.goto(origin + "/system/skills/", { waitUntil: "networkidle" });
      const overview = page.getByTestId("skills-overview");
      await overview.getByRole("heading", { name: "技能库总览", exact: true }).waitFor();
      assert.ok((await overview.innerText()).includes("显式加载记录"));
      assert.ok((await overview.innerText()).includes("不等于未使用自动规则"));
      assert.ok((await overview.innerText()).includes("以下目录存在加载问题"));
      assert.ok(!(await overview.innerText()).includes("从未被任何"));
      await page.getByTestId("skill-select-fixture-broken").click();
      const broken = page.getByTestId("skill-detail-fixture-broken");
      await broken.getByRole("heading", { name: "fixture-broken", exact: true }).waitFor();
      assert.ok((await broken.innerText()).includes("fixture invalid Skill metadata"));
      assert.ok((await broken.innerText()).includes("此目录不计为可加载 Skill"));
    });

    await check("unavailable Skill summary statistics remain unknown instead of becoming zero", async () => {
      state.failSkillSummary = true;
      await page.goto(origin + "/system/skills/", { waitUntil: "networkidle" });
      const overview = page.getByTestId("skills-overview");
      await page.getByTestId("skills-module-errors").getByText("fixture Skill statistics unavailable", { exact: true }).waitFor();
      assert.ok((await overview.innerText()).includes("统计不可用"));
      assert.ok((await overview.innerText()).includes("统计未就绪，不标记为未加载"));
      assert.ok((await page.getByTestId("skill-select-fixture-skill").innerText()).includes("未知"));
      state.failSkillSummary = false;
      await page.getByRole("button", { name: "刷新 Skill 模块", exact: true }).click();
      await page.getByTestId("skills-module-errors").waitFor({ state: "hidden" });
      await eventually(async () => !(await page.getByTestId("skill-select-fixture-skill").innerText()).includes("未知"), "Restored Skill statistics did not leave unknown state");
    });

    await check("Skill detail errors are visible and block authorization writes until a successful retry", async () => {
      state.failSkillUsage.add("fixture-skill");
      state.failSkillVisibility.add("fixture-skill");
      await page.goto(origin + "/system/skills/", { waitUntil: "networkidle" });
      await page.getByTestId("skill-select-fixture-skill").click();
      const detail = page.getByTestId("skill-detail-fixture-skill");
      await detail.getByTestId("skill-usage-error").getByText("读取近期加载明细失败：fixture usage unavailable", { exact: true }).waitFor();
      await detail.getByTestId("skill-visibility-error").getByText("读取授权失败：fixture visibility unavailable", { exact: true }).waitFor();
      const workerGrant = detail.getByRole("checkbox", { name: "执行 Skill 可见性", exact: true });
      assert.ok(await workerGrant.isDisabled(), "Unknown authorization state was presented as editable/unassigned");
      assert.ok(!(await detail.innerText()).includes("近期热数据中暂无显式加载记录"), "Usage failure was misrepresented as empty history");
      assert.equal(requests.filter((item) => item.method !== "GET" && item.path.startsWith("/api/visibility/")).length, 0, "An authorization read failure produced a write");
      state.failSkillUsage.delete("fixture-skill");
      state.failSkillVisibility.delete("fixture-skill");
      await detail.getByRole("button", { name: "重试读取授权", exact: true }).click();
      await detail.getByTestId("skill-visibility-error").waitFor({ state: "hidden" });
      await eventually(async () => !(await workerGrant.isDisabled()), "Successful authorization retry did not restore editing readiness");
      assert.ok(await workerGrant.isChecked());
      await detail.getByRole("button", { name: "重试加载明细", exact: true }).click();
      await detail.getByTestId("skill-usage-error").waitFor({ state: "hidden" });
      await detail.getByText("fixture-explicit-history", { exact: true }).waitFor();
      assert.ok(await detail.getByRole("checkbox", { name: "凭据整理 Skill 可见性", exact: true }).isDisabled());
      await page.screenshot({ path: path.join(artifacts, "skill-detail-readiness.png"), fullPage: true });
    });

    await check("late Skill usage responses never replace the newly selected directory", async () => {
      await page.getByTestId("skill-select-chains-web").click();
      const zero = page.getByTestId("skill-detail-chains-web");
      await zero.getByText("近期热数据中暂无显式加载记录；历史记录可能已归档，汇总总数仍包含归档。", { exact: true }).waitFor();
      const before = requests.filter((item) => item.path === "/api/skills/fixture-skill/usage").length;
      state.delayNextSkillUsage = "fixture-skill";
      await page.getByTestId("skill-select-fixture-skill").click();
      await eventually(() => requests.filter((item) => item.path === "/api/skills/fixture-skill/usage").length > before, "Delayed fixture Skill usage request did not start");
      await page.getByTestId("skill-select-chains-web").click();
      await zero.getByRole("heading", { name: "chains-web", exact: true }).waitFor();
      await new Promise((resolve) => setTimeout(resolve, 850));
      assert.equal(await page.getByTestId("skill-detail-fixture-skill").count(), 0);
      assert.ok(!(await zero.innerText()).includes("fixture-explicit-history"), "Late records leaked from the prior Skill selection");
      assert.equal(requests.filter((item) => item.method !== "GET" && item.path.startsWith("/api/visibility/")).length, 0);
    });

    await check("module audits preserve existing permissions and never invoke tools or external services", async () => {
      assert.equal(errors.length, 0, `Unexpected browser exceptions: ${errors.join("; ")}`);
      assert.equal(blockedExternal.size, 0, `UI attempted outbound requests: ${[...blockedExternal].join(", ")}`);
      assert.equal(unexpectedAPI.size, 0, `Unmocked API calls: ${[...unexpectedAPI].join(", ")}`);
      assert.equal(requests.filter((item) => item.method !== "GET" && /^\/api\/(tools|visibility|agents\/[^/]+\/config)(?:\/|$)/.test(item.path)).length, 0);
      assert.ok(await secretAbsent());
    });
    fs.writeFileSync(path.join(artifacts, "result.json"), JSON.stringify({ passed: completed, requests: requests.map(({ method, path: endpoint, task }) => ({ method, path: endpoint, task })), outboundRequests: [...blockedExternal] }, null, 2));
    console.log(`Management UI smoke passed: ${completed.length} scenarios; artifacts contain synthetic fixtures only.`);
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
