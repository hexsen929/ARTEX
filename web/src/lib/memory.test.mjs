import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import test from "node:test";

import {
  assertMemorySettings,
  memoryConflict,
  memoryQueryString,
  memorySettingsInput,
  memorySourceHref,
  memorySourceLabel,
  memoryTime,
  validateMemoryEntryInput,
} from "./memory-types.ts";

const settings = {
  enabled: true, auto_extract: true, global_share: true, profile_id: 0,
  max_entries: 5, max_tokens: 1200, max_jobs_per_hour: 12, retention_days: 90,
  epoch: 7, learn_after: "2026-10-30T04:00:00Z", model_available: true,
};

test("memory settings require an authoritative complete snapshot and bounded budgets", () => {
  assert.doesNotThrow(() => assertMemorySettings(settings));
  assert.doesNotThrow(() => assertMemorySettings({ ...settings, model_available: false }));
  for (const key of Object.keys(settings)) {
    const missing = { ...settings };
    delete missing[key];
    assert.throws(() => assertMemorySettings(missing), undefined, key);
  }
  for (const [key, value] of [
    ["enabled", "true"], ["profile_id", -1], ["profile_id", 0.5],
    ["max_entries", 0], ["max_entries", 11], ["max_tokens", 127], ["max_tokens", 2001],
    ["max_jobs_per_hour", 0], ["max_jobs_per_hour", 61], ["retention_days", 0],
    ["retention_days", 3651], ["learn_after", "not-a-date"],
  ]) assert.throws(() => assertMemorySettings({ ...settings, [key]: value }), undefined, `${key}:${value}`);
});

test("memory settings writes preserve epoch and never echo read-only metadata", () => {
  const input = memorySettingsInput({ ...settings, model_error: "fixture unavailable" });
  assert.equal(input.epoch, 7);
  assert.equal(input.profile_id, 0);
  assert.equal(input.global_share, true);
  for (const key of ["model_available", "model_error", "learn_after"]) assert.equal(Object.hasOwn(input, key), false, key);
});

test("source links are internal, preserve ownership and disappear for invalid sources", () => {
  assert.equal(memorySourceHref({ origin_task_id: 24, origin_conversation_id: 8, status: "active" }), "/function/tasks/detail?id=24");
  assert.equal(memorySourceHref({ origin_task_id: 0, origin_conversation_id: 8, status: "disabled" }), "/chat?c=8");
  assert.equal(memorySourceHref({ origin_task_id: 24, origin_conversation_id: 0, status: "source_invalid" }), undefined);
  assert.equal(memorySourceLabel({ origin_task_id: 24, origin_conversation_id: 0, status: "source_invalid" }), "来源已失效");
  assert.equal(memorySourceLabel({ origin_task_id: 0, origin_conversation_id: 0, status: "active" }), "人工记忆");
  for (const value of [-1, NaN, Infinity, 1.5, "javascript:alert(1)"]) {
    assert.equal(memorySourceHref({ origin_task_id: value, origin_conversation_id: 0, status: "active" }), undefined);
  }
});

test("query encoding keeps task and conversation identity separate and rejects malformed numeric IDs", () => {
  const query = new URLSearchParams(memoryQueryString({ q: "环境 未支持 v1.20", task_id: 24, offset: 0, limit: 20 }).slice(1));
  assert.equal(query.get("q"), "环境 未支持 v1.20");
  assert.equal(query.get("task_id"), "24");
  assert.equal(query.get("offset"), "0");
  assert.equal(memoryQueryString({ status: "", kind: "" }), "");
  assert.match(memoryQueryString({ conversation_id: 9, session_id: "conv-9" }), /conversation_id=9/);
  for (const value of [0, -1, Infinity, NaN, 1.5]) assert.throws(() => memoryQueryString({ task_id: value }));
});

test("known concurrent-write errors trigger explicit refresh instead of stale overwrite", () => {
  for (const message of ["HTTP 409", "memory version conflict", "记忆或来源已变化，请刷新后重试", "epoch mismatch"]) assert.equal(memoryConflict(new Error(message)), true, message);
  assert.equal(memoryConflict(new Error("模型尚未配置")), false);
  assert.equal(memoryTime("invalid"), "时间无效");
  assert.equal(memoryTime(null), "—");
});

test("manual entry bounds count UTF-8 bytes rather than assuming one character is one byte", () => {
  const entry = { title: "环境经验", content: "先核对工具版本", kind: "lesson", target: "", applicability: "", tags: [] };
  assert.doesNotThrow(() => validateMemoryEntryInput(entry));
  assert.throws(() => validateMemoryEntryInput({ ...entry, title: "中".repeat(81) }));
  assert.throws(() => validateMemoryEntryInput({ ...entry, content: "中".repeat(1601) }));
  assert.throws(() => validateMemoryEntryInput({ ...entry, tags: Array.from({ length: 17 }, (_, i) => String(i)) }));
  assert.throws(() => validateMemoryEntryInput({ ...entry, tags: ["x".repeat(49)] }));
});

test("memory center and settings are fully wired without an approval queue or shared API mutations", () => {
  const read = (path) => readFileSync(new URL(path, import.meta.url), "utf8");
  const page = read("../app/(main)/function/memories/page.tsx");
  assert.match(read("../navigation/sidebar/sidebar-items.ts"), /url: "\/function\/memories"/);
  assert.match(read("../app/(main)/system/settings/page.tsx"), /isSettingVisible\("agent", "记忆 memory[^\n]+<MemorySettingsCard/);
  assert.match(page, /无需审批/);
  assert.match(page, /原始任务证据、历史聊天、检查点、旧导出或已发送给模型的请求/);
  assert.match(page, /重新载入最新条目/);
  assert.match(page, /source_invalid|memorySourceHref/);
  assert.doesNotMatch(page, /批准记忆|审核通过|待审批/);
  assert.match(read("../app/(main)/chat/page.tsx"), /MemoryUsageButton conversationId=\{conv.id\}/);
  assert.match(read("../app/(main)/function/tasks/detail/_tabs/sessions-tab.tsx"), /MemoryUsageButton[^\n]+sessionId=\{activeKey\}/);
});
