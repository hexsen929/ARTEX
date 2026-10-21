import { syncResultStatus } from "../sync-result.ts";
import assert from "node:assert/strict";
import test from "node:test";

const result = (extra) => ({ synced: {}, companies: [], warnings: [], errors: [], ...extra });

test("legacy failed and truncated responses never claim complete success", () => {
  assert.equal(syncResultStatus(result({ errors: ["page 3 failed"] })), "failed");
  assert.equal(syncResultStatus(result({ synced: { subdomain: 40 }, errors: ["page 3 failed"] })), "partial");
  assert.equal(syncResultStatus(result({ synced: { app: 5000 }, warnings: ["remaining pages"] })), "partial");
});

test("empty successful source and existing-asset writes remain successful", () => {
  assert.equal(syncResultStatus(result({})), "success");
  assert.equal(syncResultStatus(result({ synced: { app: 1 } })), "success");
});

test("saved interruptions and partial failures survive refresh", () => {
  assert.equal(syncResultStatus(result({ status: "interrupted", synced: { app: 1 } })), "interrupted");
  assert.equal(syncResultStatus(result({ status: "running" })), "running");
  assert.equal(syncResultStatus(result({ status: "partial" })), "partial");
  assert.equal(syncResultStatus(result({ status: "success", stats: { skipped: 1 } })), "partial");
});
