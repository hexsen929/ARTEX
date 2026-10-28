import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import test from "node:test";

import { SKILL_USAGE_SCOPE_NOTE, skillExplicitLoads, skillLoadability, summarizeSkillUsage } from "./skill-observability.ts";

const fixture = (name, calls, extra = {}) => ({
  name, calls, files: ["SKILL.md"], tasks: 0, usage_agents: [], ...extra,
});

test("unavailable and malformed Skill counters remain unknown instead of zero", () => {
  assert.equal(skillExplicitLoads(fixture("unknown", null)), null);
  assert.equal(skillExplicitLoads(fixture("unknown", 0, { usage_available: false })), null);
  assert.equal(skillExplicitLoads(fixture("unknown", 12), false), null);
  for (const calls of [undefined, NaN, Infinity, -1, 1.5, Number.MAX_SAFE_INTEGER + 1]) {
    assert.equal(skillExplicitLoads(fixture("invalid", calls)), null);
  }
  assert.equal(skillExplicitLoads(fixture("old-response", 0)), 0);
  assert.equal(skillExplicitLoads(fixture("known", 12, { usage_available: true })), 12);
});

test("loadability is independent of directory presence and explicit-use records", () => {
  assert.equal(skillLoadability({ loadable: true }), "loadable");
  assert.equal(skillLoadability({ loadable: false }), "invalid");
  assert.equal(skillLoadability({}), "unknown");
  const result = summarizeSkillUsage([
    fixture("loaded", 3, { loadable: true }),
    fixture("unrecorded", 0, { loadable: true }),
    fixture("broken", 0, { loadable: false }),
    fixture("old-server", 0),
  ], true);
  assert.equal(result.totalCalls, 3);
  assert.equal(result.loadableCount, 2);
  assert.equal(result.loadabilityUnknown, 1);
  assert.deepEqual(result.invalid.map((skill) => skill.name), ["broken"]);
  assert.deepEqual(result.unrecorded.map((skill) => skill.name), ["unrecorded", "old-server"]);
});

test("incomplete totals are not presented as complete zero-use classifications", () => {
  const items = [fixture("known", 4), fixture("unknown", null, { usage_available: false })];
  const summary = summarizeSkillUsage(items, true);
  assert.equal(summary.totalCalls, null);
  assert.equal(summary.unknownCount, 1);
  assert.equal(summary.usedCount, 1);
  const unavailable = summarizeSkillUsage(items, false);
  assert.equal(unavailable.totalCalls, null);
  assert.deepEqual(unavailable.unrecorded, []);
  assert.deepEqual(unavailable.ranked, []);
});

test("ranking preserves archive-inclusive counts but does not rank unknown data", () => {
  const items = [
    fixture("older", 2, { last_used: "2026-10-28T00:00:00Z" }),
    fixture("recent", 5, { last_used: "2026-10-29T00:00:00Z" }),
    fixture("unavailable", null, { last_used: "2026-10-30T00:00:00Z" }),
  ];
  const summary = summarizeSkillUsage(items);
  assert.deepEqual(summary.ranked.map(({ skill }) => skill.name), ["recent", "older"]);
  assert.deepEqual(summary.recent.map((skill) => skill.name), ["recent", "older"]);
  assert.match(SKILL_USAGE_SCOPE_NOTE, /不代表执行完成或健康状态/);
  assert.match(SKILL_USAGE_SCOPE_NOTE, /不含内置规则自动注入和直接 Read/);
  assert.match(SKILL_USAGE_SCOPE_NOTE, /汇总包含归档，近期明细仅热数据/);
});

test("Skill UI separates loading/error states and invalidates stale detail responses", () => {
  const page = readFileSync(new URL("../app/(main)/system/skills/page.tsx", import.meta.url), "utf8");
  assert.match(page, /api\.skillsStatus\(\)/);
  assert.match(page, /api\.skillUsageStatus\(name, 20\)/);
  assert.match(page, /generation !== catalogGeneration\.current/);
  assert.match(page, /generation !== usageGeneration\.current/);
  assert.match(page, /generation !== fileGeneration\.current/);
  assert.match(page, /visibilityGeneration\.current\[name\] !== generation/);
  assert.match(page, /visibilityStatus\[selectedSkill\.name\] !== "ready"/);
  assert.match(page, /agentBindingSupport\(actor, "skill"\)/);
  assert.match(page, /data-testid="skill-usage-error"/);
  assert.match(page, /data-testid="skill-visibility-error"/);
  assert.match(page, /data-testid="skills-module-errors"/);
  assert.doesNotMatch(page, /从未被任何 agent 加载|所有 Skill 都被调用过|还没有调用记录/);
  assert.doesNotMatch(page, /\.catch\(\(\) => setUsageCalls\(\[\]\)\)/);
});
