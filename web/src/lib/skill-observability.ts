import type { SkillItem } from "./types";

export const SKILL_USAGE_SCOPE_NOTE =
  "仅统计显式 Skill() 注册匹配的加载尝试，不代表执行完成或健康状态；不含内置规则自动注入和直接 Read。汇总包含归档，近期明细仅热数据。";

/** A missing/unavailable counter is unknown, never an observed zero. */
export function skillExplicitLoads(skill: Pick<SkillItem, "calls" | "usage_available">, available?: boolean): number | null {
  if (available === false || skill.usage_available === false) return null;
  return typeof skill.calls === "number" && Number.isSafeInteger(skill.calls) && skill.calls >= 0 ? skill.calls : null;
}

export function skillLoadability(skill: Pick<SkillItem, "loadable">): "loadable" | "invalid" | "unknown" {
  return skill.loadable === true ? "loadable" : skill.loadable === false ? "invalid" : "unknown";
}

export function summarizeSkillUsage(skills: SkillItem[], available?: boolean) {
  const counted = skills.flatMap((skill) => {
    const calls = skillExplicitLoads(skill, available);
    return calls === null ? [] : [{ skill, calls }];
  });
  const allKnown = available !== false && counted.length === skills.length;
  const ranked = counted.filter((item) => item.calls > 0).sort((a, b) => b.calls - a.calls);
  return {
    totalCalls: allKnown ? counted.reduce((sum, item) => sum + item.calls, 0) : null,
    usedCount: ranked.length,
    ranked,
    topCalls: ranked[0]?.calls ?? 0,
    // Invalid entries have a loading issue, not a trustworthy "unused" status.
    unrecorded: counted.filter(({ skill, calls }) => skill.loadable !== false && calls === 0).map(({ skill }) => skill),
    unknownCount: skills.length - counted.length,
    invalid: skills.filter((skill) => skillLoadability(skill) === "invalid"),
    loadableCount: skills.filter((skill) => skillLoadability(skill) === "loadable").length,
    loadabilityUnknown: skills.filter((skill) => skillLoadability(skill) === "unknown").length,
    recent: counted.filter(({ skill }) => skill.last_used).map(({ skill }) => skill)
      .sort((a, b) => a.last_used! < b.last_used! ? 1 : -1).slice(0, 6),
  };
}
