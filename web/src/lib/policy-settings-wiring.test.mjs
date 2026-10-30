import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import test from "node:test";

import {
  isFindingVerifyMode, isLLMRecordsRetentionDays, MAX_LLM_RECORDS_RETENTION_DAYS,
  parseLLMRecordsRetentionDraft, readFindingVerifyMode, readLLMRecordsRetentionDays,
} from "../app/(main)/system/settings/_components/settings-response.ts";

test("retention snapshot and drafts accept exact bounded integers, including explicit zero", () => {
  assert.equal(MAX_LLM_RECORDS_RETENTION_DAYS, 106751);
  for (const days of [0, 1, 30, 365, 106751]) {
    const snapshot = { llm_records_retention_days: days };
    assert.equal(readLLMRecordsRetentionDays(snapshot), days);
    assert.equal(parseLLMRecordsRetentionDraft(String(days)), days);
    assert.equal(isLLMRecordsRetentionDays(days), true);
    assert.deepEqual(snapshot, { llm_records_retention_days: days });
  }
  for (const days of [undefined, null, "30", -1, 1.5, 106752, Number.NaN, Number.POSITIVE_INFINITY]) {
    assert.equal(isLLMRecordsRetentionDays(days), false);
    assert.throws(() => readLLMRecordsRetentionDays({ llm_records_retention_days: days }), /当前值未确认/);
  }
  for (const input of ["", " ", "-1", "1.5", "1e2", "Infinity", "106752", "30 days"]) {
    assert.equal(parseLLMRecordsRetentionDraft(input), null, `invalid draft ${input} became a number`);
  }
  for (const snapshot of [null, {}, [], { error: "unknown" }]) {
    assert.throws(() => readLLMRecordsRetentionDays(snapshot), /当前值未确认/);
  }
});

test("verification snapshot accepts only auto and off without guessing an unknown state", () => {
  for (const mode of ["auto", "off"]) {
    assert.equal(readFindingVerifyMode({ finding_verify_mode: mode }), mode);
    assert.equal(isFindingVerifyMode(mode), true);
  }
  for (const mode of [undefined, null, "", "AUTO", " off ", "strict", false, 0, [], {}]) {
    assert.equal(isFindingVerifyMode(mode), false);
    assert.throws(() => readFindingVerifyMode({ finding_verify_mode: mode }), /当前值未确认/);
  }
});

test("policy cards are independent, explicit single-field saves with future-effect confirmations", () => {
  const card = readFileSync(new URL("../app/(main)/system/settings/_components/policy-settings-cards.tsx", import.meta.url), "utf8");
  const page = readFileSync(new URL("../app/(main)/system/settings/page.tsx", import.meta.url), "utf8");
  assert.match(page, /isSettingVisible\("agent", "自动验证 复核 finding_verify_mode verifier 触发器"\)/);
  assert.match(page, /isSettingVisible\("chat", "LLM 调用 录制 记录 保留期 保留天数 llm_records_retention_days 清理"\)/);
  assert.match(card, /useState\(""\)/);
  assert.match(card, /useState<FindingVerifyMode \| null>\(null\)/);
  assert.match(card, /usePolicySetting\("llm_records_retention_days", readLLMRecordsRetentionDays/);
  assert.match(card, /usePolicySetting\("finding_verify_mode", readFindingVerifyMode/);
  assert.match(card, /api\.getRuntimeControl\(key\)/);
  assert.match(card, /api\.saveRuntimeControl\(key, expected\)/);
  assert.match(card, /policy\.persist\(selected\)/);
  assert.match(card, /policy\.persist\(draft\)/);
  assert.doesNotMatch(card, /api\.(?:settings|setSettings)\(/);
  assert.match(card, /Object\.is\(value, expected\)/);
  assert.match(card, /selected > 0 && \(saved === 0 \|\| \(saved !== null && selected < saved\)\)/);
  assert.match(card, /确认记录保留策略变更/);
  assert.match(card, /确认自动验证模式变更/);
  assert.match(card, /保存本身不立即删除/);
  assert.match(card, /后续每 24 小时的清理/);
  assert.match(card, /不取消正在运行的验证/);
  assert.match(card, /重新开启不会补跑跳过的历史事件/);
  assert.match(card, /不会自动启用 Agent 或补建触发器/);
  assert.match(card, /保存结果未确认/);
  assert.match(card, /request !== generation\.current/);
  assert.doesNotMatch(card, /(?:delete|clear|start|run)Finding|clearLLMRecords|DeleteLLMRecordsBefore|requestAutomaticFinding/);
});
