import assert from "node:assert/strict";
import test from "node:test";
import fs from "node:fs";
import { isUpdateTransportFailure, normalizedUpdateVersion, observeUpdateVersion } from "./update-lifecycle.ts";

test("updates confirm only the requested target, including optional tag prefix", () => {
  const operation = { kind: "update", from: "2.4.6", target: "v2.4.7" };
  assert.equal(observeUpdateVersion(operation, "2.4.6"), "waiting");
  assert.equal(observeUpdateVersion(operation, "2.4.7"), "confirmed");
  assert.equal(observeUpdateVersion(operation, "v2.4.7"), "confirmed");
  assert.equal(observeUpdateVersion(operation, "2.4.8"), "unexpected");
  assert.equal(observeUpdateVersion(operation, "2.4.5"), "unexpected");
  for (const value of [undefined, null, 7, {}, "", " "]) {
    assert.equal(observeUpdateVersion(operation, value), "waiting");
  }
});

test("rollback observation never asserts a verified update target", () => {
  const operation = { kind: "rollback", from: "2.4.7" };
  assert.equal(observeUpdateVersion(operation, "2.4.7"), "waiting");
  assert.equal(observeUpdateVersion(operation, "2.4.6"), "rollback_observed");
  assert.equal(observeUpdateVersion(operation, "2.4.8"), "rollback_observed");
  assert.equal(normalizedUpdateVersion(" v2.4.7 "), "2.4.7");
  assert.equal(normalizedUpdateVersion("dev"), "dev");
});

test("transport disconnect is uncertain, not permission to replay an update", () => {
  assert.equal(isUpdateTransportFailure(new TypeError("Failed to fetch")), true);
  const network = new Error("connection interrupted");
  network.name = "NetworkError";
  assert.equal(isUpdateTransportFailure(network), true);
  assert.equal(isUpdateTransportFailure(new Error("server rejected update")), false);
  assert.equal(isUpdateTransportFailure(null), false);
});

test("update card cancels stale checks and polls without replaying update requests", () => {
  const card = fs.readFileSync(new URL("../app/(main)/system/settings/_components/update-card.tsx", import.meta.url), "utf8");
  assert.match(card, /checkVersion\.current/);
  assert.match(card, /version !== streamVersion\.current/);
  assert.match(card, /restartAbort\.current\?\.abort/);
  assert.match(card, /observeUpdateVersion\(operation, j\.version\)/);
  assert.match(card, /isUpdateTransportFailure\(e\)/);
  assert.match(card, /attempt\.waitForRestart\(\)/);
  assert.match(card, /kind: "update", from, target/);
  assert.match(card, /kind: "rollback", from/);
  assert.doesNotMatch(card, /j\.version\s*!==\s*fromVersion/);
});
