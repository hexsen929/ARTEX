import assert from "node:assert/strict";
import test from "node:test";

import {
  acknowledgeMissingSkillNotices,
  missingSkillLibraryLabel,
  missingSkillNoticeSummary,
} from "./missing-skill-notices.ts";

test("missing notice totals distinguish skill groups from invocation count", () => {
  assert.deepEqual(missingSkillNoticeSummary([{ calls: 3 }, { calls: 1 }]), { skills: 2, calls: 4 });
  assert.deepEqual(missingSkillNoticeSummary([]), { skills: 0, calls: 0 });
});

test("an unknown library presence is not presented as a missing directory", () => {
  assert.equal(missingSkillLibraryLabel(true), "文件存在");
  assert.equal(missingSkillLibraryLabel(false), "文件缺失");
  assert.equal(missingSkillLibraryLabel(undefined), "文件状态未知");
});

test("acknowledgement reloads and preserves a notice created concurrently", async () => {
  const calls = [];
  const latest = [{ skill: "new-unresolved", calls: 1 }];
  const notices = await acknowledgeMissingSkillNotices(
    async () => { calls.push("acknowledge"); },
    async () => { calls.push("reload"); return latest; },
  );
  assert.deepEqual(calls, ["acknowledge", "reload"]);
  assert.equal(notices, latest);
});

test("failed acknowledgement or reload propagates without inventing an empty list", async () => {
  let reloadCalled = false;
  await assert.rejects(acknowledgeMissingSkillNotices(
    async () => { throw new Error("save failed"); },
    async () => { reloadCalled = true; return []; },
  ), /save failed/);
  assert.equal(reloadCalled, false);
  await assert.rejects(acknowledgeMissingSkillNotices(
    async () => undefined,
    async () => { throw new Error("reload failed"); },
  ), /reload failed/);
});
