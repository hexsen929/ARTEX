import assert from "node:assert/strict";
import test from "node:test";

import { canUnbindTaskSession } from "./session-bindings.ts";

test("session ownership cannot be unbound from its owner task", () => {
  assert.equal(canUnbindTaskSession("12", 12), false);
  assert.equal(canUnbindTaskSession("12", 7), true);
});

test("the all-task view and invalid task filters never expose unbinding", () => {
  for (const taskId of [undefined, "", "all", "0", "-1", "fixture"])
    assert.equal(canUnbindTaskSession(taskId, 7), false);
});

test("an explicitly shared unowned session can relinquish its selected-task grant", () => {
  assert.equal(canUnbindTaskSession("12", undefined), true);
  assert.equal(canUnbindTaskSession("12", 0), true);
});
