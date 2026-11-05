import assert from "node:assert/strict";
import test from "node:test";
import { assessProxyImportReply } from "./proxy-import-result.ts";

const duplicate = { line: 1, error: "重复代理，已跳过", kind: "duplicate" };
const invalid = { line: 2, error: "地址格式无效", kind: "invalid" };

test("all-duplicate input is a confirmed no-op, preserves the draft and claims no probe", () => {
  const result = assessProxyImportReply({ ok: true, probe_queued: false, imported: 0, skipped_duplicates: 1, invalid: 0, errors: [duplicate] });
  assert.equal(result.clearDraft, false);
  assert.equal(result.probeQueued, false);
  assert.deepEqual(result.invalidErrors, []);
  assert.equal(result.summary, "新增 0 条，重复跳过 1 条，格式错误 0 条");
});

test("partial import preserves invalid input; full valid import may clear its draft", () => {
  const partial = assessProxyImportReply({ ok: true, probe_queued: true, imported: 3, skipped_duplicates: 1, invalid: 1, errors: [duplicate, invalid] });
  assert.equal(partial.clearDraft, false);
  assert.deepEqual(partial.invalidErrors, [invalid]);
  assert.equal(partial.summary, "新增 3 条，重复跳过 1 条，格式错误 1 条");
  const valid = assessProxyImportReply({ ok: true, probe_queued: true, imported: 3, skipped_duplicates: 1, invalid: 0, errors: [duplicate] });
  assert.equal(valid.clearDraft, true);
});

test("invalid-only input needs no queued probe and retains its draft", () => {
  const result = assessProxyImportReply({ ok: true, probe_queued: false, imported: 0, skipped_duplicates: 0, invalid: 1, errors: [invalid] });
  assert.equal(result.clearDraft, false);
  assert.deepEqual(result.invalidErrors, [invalid]);
});

test("malformed acknowledgements cannot become confirmed imports", () => {
  const baseline = { ok: true, probe_queued: true, imported: 1, skipped_duplicates: 0, invalid: 0, errors: [] };
  for (const patch of [{ ok: false }, { imported: -1 }, { imported: 1.5 }, { invalid: undefined }, { skipped_duplicates: 1 }, { probe_queued: false }, { errors: null }, { errors: [{ line: 0, error: "bad" }] }, { errors: [{ line: 1, error: "bad", kind: "unknown" }] }]) {
    assert.throws(() => assessProxyImportReply({ ...baseline, ...patch }));
  }
});

test("legacy acknowledged imports work without inventing counters", () => {
  const result = assessProxyImportReply({ ok: true, probe_queued: true, errors: [] });
  assert.equal(result.summary, null);
  assert.equal(result.clearDraft, true);
  assert.throws(() => assessProxyImportReply({ ok: true, probe_queued: false, errors: [] }));
});
