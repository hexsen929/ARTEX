import assert from "node:assert/strict";
import test from "node:test";

import { inlineHistoryOrder, inlineReportPayload, inlineReportText } from "./finding-inline-history.ts";

test("inline audit payload accepts JSON objects without assuming report fields", () => {
  assert.deepEqual(inlineReportPayload('{"name":"fixture","summary":"original"}'), { name: "fixture", summary: "original" });
  for (const value of [null, [], "invalid json", 1]) assert.deepEqual(inlineReportPayload(value), {});
});

test("untrusted report markup remains literal text for escaped React rendering", () => {
  const evidence = '<script>alert("fixture")</script> [link](javascript:fixture)';
  assert.equal(inlineReportText(evidence), evidence);
  assert.equal(inlineReportText(null), "");
  assert.equal(inlineReportText({ proof: "fixture" }), '{\n  "proof": "fixture"\n}');
});

test("audit records sort chronologically and keep int64 ID ordering without precision loss", () => {
  const events = [
    { id: "9007199254740993", created_at: "2026-10-26T01:00:00Z" },
    { id: "9007199254740992", created_at: "2026-10-26T01:00:00Z" },
    { id: "99", created_at: "2026-10-26T00:00:00Z" },
  ];
  assert.deepEqual(inlineHistoryOrder(events).map((event) => event.id), ["99", "9007199254740992", "9007199254740993"]);
  assert.equal(events[0].id, "9007199254740993", "sorting must not mutate API state");
});
