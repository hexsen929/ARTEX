import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import test from "node:test";
import { readTaskAssetSharingUpdate } from "./task-asset-sharing.ts";

test("task sharing changes require the expected task and exact boolean result", () => {
  for (const expected of [false, true]) {
    const task = { id: "fixture-task", share_global_assets: expected, unrelated: "retained" };
    const before = structuredClone(task);
    assert.equal(readTaskAssetSharingUpdate(task, task.id, expected), expected);
    assert.deepEqual(task, before);
    for (const value of [null, {}, [], { id: task.id }, { id: "other", share_global_assets: expected }, { id: task.id, share_global_assets: String(expected) }, { id: task.id, share_global_assets: !expected }]) {
      assert.throws(() => readTaskAssetSharingUpdate(value, task.id, expected), /未确认/);
    }
  }
});

test("task UI keeps sharing opt-in and does not confuse an unknown policy with disabled", () => {
  const page = readFileSync(new URL("../app/(main)/function/tasks/page.tsx", import.meta.url), "utf8");
  assert.match(page, /const \[shareGlobalAssets, setShareGlobalAssets\] = React\.useState\(false\)/);
  assert.match(page, /readTaskAssetSharingUpdate\(updated, task\.id, !enabled\)/);
  assert.match(page, /const known = typeof task\.share_global_assets === "boolean"/);
  assert.match(page, /disabled=\{!known \|\| saving\}/);
  assert.match(page, /共享全局资产：状态未知，请刷新/);
});
