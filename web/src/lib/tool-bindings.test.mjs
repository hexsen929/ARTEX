import assert from "node:assert/strict";
import test from "node:test";
import { readFileSync } from "node:fs";

import { agentBindingSupport, agentRuntimeCapabilities, bindingCountLabel, toolUsageDisplay } from "./tool-bindings.ts";

test("shell declarations never claim an independent zero invocation count", () => {
  assert.equal(toolUsageDisplay({ kind: "shell", calls: 0 }).label, "环境声明");
  assert.equal(toolUsageDisplay({ usage_kind: "shell_declaration", calls: null }).applicable, false);
  assert.equal(toolUsageDisplay({ kind: "http", calls: 0, usage_available: true }).label, "目录调用 0");
  assert.equal(toolUsageDisplay({ kind: "http", calls: 99, usage_available: false }).label, "统计不可用");
  assert.equal(toolUsageDisplay({ kind: "http", calls: null }).label, "统计不可用");
  assert.equal(toolUsageDisplay({ kind: "http" }).label, "统计不可用");
});

test("fixed role binding semantics preserve inheritance and restricted execution", () => {
  const intranet = { key: "worker.intranet" };
  assert.equal(agentRuntimeCapabilities(intranet).execution_agent_key, "worker");
  assert.equal(agentBindingSupport(intranet, "tool", "external_httpx").allowed, false);
  assert.equal(agentBindingSupport(intranet, "mcp").allowed, false);
  assert.equal(agentBindingSupport(intranet, "skill").allowed, true);
  for (const kind of ["tool", "mcp", "skill"]) assert.equal(agentBindingSupport({ key: "credential_processor" }, kind).allowed, false);
  assert.equal(agentBindingSupport({ key: "goals" }, "tool", "set_goals").allowed, true);
  assert.equal(agentBindingSupport({ key: "goals" }, "tool", "external_httpx").allowed, false);
  assert.equal(agentBindingSupport({ key: "goals" }, "mcp").allowed, false);
  assert.equal(agentBindingSupport({ key: "goals" }, "skill").allowed, true);
  assert.equal(agentRuntimeCapabilities({ key: "goals" }).runtime_config_mode, "llm_only");
  assert.equal(agentRuntimeCapabilities({ key: "credential_processor" }).runtime_config_mode, "llm_only");
  assert.equal(agentRuntimeCapabilities(intranet).runtime_config_mode, "inherited");
  assert.equal(agentBindingSupport({ key: "custom_agent" }, "tool", "custom_tool").allowed, true);
});

test("capability projection and unavailable counts remain explicit", () => {
  const agent = { key: "fixture", capabilities: { execution_agent_key: "worker", tool_binding_mode: "inherited", mcp_binding_mode: "inherited", skill_binding_mode: "direct" } };
  assert.equal(agentBindingSupport(agent, "tool", "fixture").allowed, false);
  assert.equal(bindingCountLabel(undefined), "—");
  assert.equal(bindingCountLabel(0), "0");
  assert.equal(bindingCountLabel(9, false), "—");
});

test("binding editors keep unsupported existing selections read-only", () => {
  const tools = readFileSync(new URL("../app/(main)/system/tools/page.tsx", import.meta.url), "utf8");
  const editor = readFileSync(new URL("../components/agent-editor.tsx", import.meta.url), "utf8");
  assert.match(tools, /agentBindingSupport/);
  assert.match(editor, /agentBindingSupport/);
  assert.doesNotMatch(tools, /setBound\([^\n]*filter\([^\n]*allowed/);
  assert.match(tools, /环境声明/);
  assert.match(editor, /runtime_config_mode === "inherited"/);
  assert.match(editor, /bindingOperation\.current/);
  assert.match(editor, /读取失败不等于未授权/);
  assert.match(editor, /patchAgentVisibility\(agentKey, patch\)/);
  const mcpAction = editor.slice(editor.indexOf("function toggleMcp("), editor.indexOf("function toggleSkill("));
  const skillAction = editor.slice(editor.indexOf("function toggleSkill("), editor.indexOf("async function toggleTool("));
  assert.match(mcpAction, /\{ mcp:/);
  assert.doesNotMatch(mcpAction, /\{ skill:|skillVisible/);
  assert.match(skillAction, /\{ skill:/);
  assert.doesNotMatch(skillAction, /\{ mcp:|mcpVisible/);
  assert.match(editor, /const showTriggers = isCustom && capabilities\.runtime_config_mode === "direct"/);
  assert.equal((editor.match(/\{showTriggers &&/g) || []).length, 2);
  assert.match(editor, /data-testid="agent-dedicated-pipeline"/);
});
