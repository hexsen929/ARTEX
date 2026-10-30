import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import test from "node:test";

import {
  isRoEEnforcement,
  readRoEEnforcement,
} from "../app/(main)/system/settings/_components/settings-response.ts";

test("RoE snapshots preserve each exact mode and never invent a default", () => {
  for (const mode of ["off", "warn", "strict"]) {
    const response = { roe_enforcement: mode };
    const before = structuredClone(response);
    assert.equal(readRoEEnforcement(response), mode);
    assert.equal(isRoEEnforcement(mode), true);
    assert.deepEqual(response, before);
  }
  for (const value of [undefined, null, {}, [], "warn", { error: "unavailable" }]) {
    assert.throws(() => readRoEEnforcement(value), /当前值未确认/);
  }
  for (const mode of [undefined, null, "", "WARN", " strict ", "unknown", false, 0, [], {}]) {
    assert.equal(isRoEEnforcement(mode), false);
    assert.throws(() => readRoEEnforcement({ roe_enforcement: mode }), /当前值未确认/);
  }
});

test("RoE card is discoverable and distinct from OPSEC or approval toggles", () => {
  const page = readFileSync(new URL("../app/(main)/system/settings/page.tsx", import.meta.url), "utf8");
  const card = readFileSync(new URL("../app/(main)/system/settings/_components/roe-enforcement-card.tsx", import.meta.url), "utf8");
  assert.match(page, /isSettingVisible\("traffic", "RoE roe_enforcement 授权 范围 核验 strict warn off"\)/);
  assert.match(page, /<RoEEnforcementCard/);
  assert.match(page, /搜索设置，例如：RoE、授权范围/);
  assert.match(card, /独立于 OPSEC、提示词约束和自动审批/);
  assert.match(card, /不会增加任务授权范围/);
  assert.match(card, /不会关闭其他拦截、审批或敏感信息保护/);
  for (const mode of ["strict", "warn", "off"]) {
    assert.match(card, new RegExp(`<SelectItem value="${mode}">`));
  }
});

test("RoE editing requires an explicit selected patch, not page-load defaults", () => {
  const card = readFileSync(new URL("../app/(main)/system/settings/_components/roe-enforcement-card.tsx", import.meta.url), "utf8");
  assert.match(card, /useState<RoEEnforcement \| null>\(null\)/);
  assert.doesNotMatch(card, /useState(?:<[^>]+>)?\("warn"\)/);
  assert.match(card, /readRoEEnforcement\(await api\.getRuntimeControl\("roe_enforcement"\)\)/);
  assert.match(card, /api\.saveRuntimeControl\("roe_enforcement", mode\)/);
  assert.doesNotMatch(card, /api\.(?:settings|setSettings)\(/);
  assert.match(card, /if \(!currentKnown \|\| draft === "off"\)/);
  assert.match(card, /确认关闭并保存/);
  assert.match(card, /确认仅修复 RoE/);
  assert.match(card, /保存结果未确认/);
  assert.match(card, /if \(confirmed !== mode\) throw new Error/);
  assert.match(card, /保留上次确认值，请重新读取核对/);
  assert.match(card, /generation\.current !== request/);
  assert.match(card, /mounted\.current/);
});

test("traffic and screenshot labels describe actual supported wiring rather than universal capture", () => {
  const page = readFileSync(new URL("../app/(main)/system/settings/page.tsx", import.meta.url), "utf8");
  assert.doesNotMatch(page, /所有 Agent 的 HTTP 流量经记录代理全量落库/);
  assert.match(page, /未接入的会话驱动流量不会因此自动经过 MITM/);
  assert.match(page, /不改变其他角色已获授权的截图能力/);
  assert.match(page, /不自动修改浏览器启动参数/);
});
