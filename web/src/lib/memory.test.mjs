import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import test from "node:test";

import {
  assertMemorySettings,
  assertMemoryLearning,
  assertMemoryEntrySharing,
  memoryConflict,
  memoryQueryString,
  memorySettingsInput,
  memorySourceHref,
  memorySourceLabel,
  memorySharingLabel,
  memoryLearningOutcome,
  memoryTime,
  validateMemoryEntryInput,
} from "./memory-types.ts";

const settings = {
  enabled: true, auto_extract: true, global_share: true, profile_id: 0,
  max_entries: 5, max_tokens: 2000, max_jobs_per_hour: 3, retention_days: 90,
  epoch: 7, learn_after: "2026-10-30T04:00:00Z", model_available: true,
};

test("memory settings require an authoritative complete snapshot and bounded budgets", () => {
  assert.doesNotThrow(() => assertMemorySettings(settings));
  assert.doesNotThrow(() => assertMemorySettings({ ...settings, max_tokens: 1024 }));
  assert.doesNotThrow(() => assertMemorySettings({ ...settings, model_available: false }));
  assert.doesNotThrow(() => assertMemorySettings({ ...settings, max_jobs_per_hour: 12 }));
  for (const key of Object.keys(settings)) {
    const missing = { ...settings };
    delete missing[key];
    assert.throws(() => assertMemorySettings(missing), undefined, key);
  }
  for (const [key, value] of [
    ["enabled", "true"], ["profile_id", -1], ["profile_id", 0.5],
    ["max_entries", 0], ["max_entries", 11], ["max_tokens", 1023], ["max_tokens", 2001],
    ["max_jobs_per_hour", 0], ["max_jobs_per_hour", 61], ["retention_days", 0],
    ["retention_days", 3651], ["learn_after", "not-a-date"],
  ]) assert.throws(() => assertMemorySettings({ ...settings, [key]: value }), undefined, `${key}:${value}`);
});

test("memory settings writes preserve epoch and never echo read-only metadata", () => {
  const input = memorySettingsInput({ ...settings, model_error: "fixture unavailable", learning: { processed: 4 }, future_readonly: "not writable" });
  assert.equal(input.epoch, 7);
  assert.equal(input.profile_id, 0);
  assert.equal(input.global_share, true);
  for (const key of ["model_available", "model_error", "learn_after", "learning", "future_readonly"]) assert.equal(Object.hasOwn(input, key), false, key);
  assert.deepEqual(Object.keys(input).sort(), ["enabled", "auto_extract", "global_share", "profile_id", "max_entries", "max_tokens", "max_jobs_per_hour", "retention_days", "epoch"].sort());
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

test("only three experience kinds can be shared and target notes require task ownership plus target", () => {
  const entry = { title: "阶段经验", content: "仅作历史参考", kind: "lesson", target: "", applicability: "", tags: [] };
  const target = { ...entry, kind: "target", target: "TARGET", origin_task_id: 24 };
  assert.doesNotThrow(() => validateMemoryEntryInput(target));
  assert.throws(() => validateMemoryEntryInput({ ...target, origin_task_id: undefined }));
  assert.throws(() => validateMemoryEntryInput({ ...target, origin_task_id: 0 }));
  assert.throws(() => validateMemoryEntryInput({ ...target, target: "" }));
  assert.throws(() => validateMemoryEntryInput({ ...entry, target: "TARGET" }));
  assert.doesNotThrow(() => validateMemoryEntryInput({ ...target, origin_task_id: undefined }, 24));
  assert.throws(() => validateMemoryEntryInput({ ...target, origin_task_id: undefined }, 0));
  for (const kind of ["environment", "procedure", "lesson"]) {
    assert.doesNotThrow(() => assertMemoryEntrySharing({ kind, share_scope: "global_experience" }));
    assert.equal(memorySharingLabel({ kind, share_scope: "global_experience", origin_task_id: 24, origin_conversation_id: 0 }), "通用经验（可跨任务）");
  }
  assert.throws(() => assertMemoryEntrySharing({ kind: "target", share_scope: "global_experience" }));
  assert.doesNotThrow(() => assertMemoryEntrySharing({ kind: "target", share_scope: "source_only" }));
  assert.equal(memorySharingLabel({ kind: "target", share_scope: "source_only", origin_task_id: 24, origin_conversation_id: 0 }), "仅原任务");
  assert.equal(memorySharingLabel({ kind: "target", share_scope: "source_only", origin_task_id: 0, origin_conversation_id: 0 }), "不跨任务（来源未登记）");
});

test("learning state snapshots distinguish stage waiting, budget, unavailable and a successful empty summary", () => {
  const snapshot = { status: "waiting_stage", message: "等待阶段结束", queued: 0, running: 0, failed: 0, processed: 0, starts_last_hour: 0, attempts_last_hour: 0, max_calls_per_hour: 3 };
  for (const status of ["disabled", "model_unavailable", "waiting_stage", "queued", "running", "budget_wait", "retry_wait", "idle"]) assert.doesNotThrow(() => assertMemoryLearning({ ...snapshot, status }));
  assert.doesNotThrow(() => assertMemoryLearning({ ...snapshot, processed: 2, last_result: "no_experience", last_completed_at: "2026-10-30T05:00:00Z" }));
  assert.match(memoryLearningOutcome("no_experience"), /未发现值得保存的通用经验/);
  assert.match(memoryLearningOutcome("no_experience"), /不是学习失败/);
  assert.doesNotMatch(memoryLearningOutcome(undefined), /已保存经验|已经学习|已学会/);
  for (const override of [{ status: "error" }, { processed: -1 }, { queued: undefined }, { max_calls_per_hour: 0 }, { last_result: "learned" }, { last_completed_at: "invalid" }]) assert.throws(() => assertMemoryLearning({ ...snapshot, ...override }));
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
  assert.match(read("../components/memory-settings-card.tsx"), /注入Token预算（UTF-8字节保守计数，含来源元数据）/);
  assert.match(read("../components/memory-settings-card.tsx"), /index_only/);
  assert.match(read("../components/memory-settings-card.tsx"), /跨任务共享通用经验/);
  assert.match(page, /依据原生事实/);
  assert.match(page, /最多取 8 段完整事实摘录/);
  assert.match(page, /清空目标标识（保存后才生效）/);
  assert.doesNotMatch(read("../components/memory-settings-card.tsx"), /所有任务共享|从新增或变更的来源提炼/);
  const learning = read("../components/memory-learning-status.tsx");
  for (const pattern of [/visibilitychange/, /30_000/, /request.signal.aborted/, /controller\?\.abort\(\)/, /clearInterval/, /removeEventListener/]) assert.match(learning, pattern);
  assert.match(learning, /领取阶段/);
  assert.match(learning, /实际 HTTP 尝试/);
});
