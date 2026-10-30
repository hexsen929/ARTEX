import assert from "node:assert/strict";
import fs from "node:fs";
import test from "node:test";
import { assertNotificationMeta, notificationDigestMinutes, readNotificationSettings } from "./notification-ui-state.ts";

const meta = () => ({
  enabled: false, public_base_url: "https://fixture.example.invalid", digest_interval_min: "",
  defaults: { digest_interval_min: 30 },
  kinds: [{ kind: "webhook", default_rate_per_min: 0, secret_keys: ["url"] }],
  stats: { channels: 1, channels_on: 1, pending: 0, failed: 0, sent_today: 0, backlog_age_ms: 0 },
});

test("notification metadata must be known before edits; missing fields are not false/empty defaults", () => {
  assert.doesNotThrow(() => assertNotificationMeta(meta()));
  for (const key of ["enabled", "public_base_url", "digest_interval_min", "defaults", "kinds", "stats"]) {
    const partial = meta(); delete partial[key];
    assert.throws(() => assertNotificationMeta(partial));
  }
  for (const invalid of [null, [], {}, { ...meta(), enabled: "false" }, { ...meta(), defaults: { digest_interval_min: 0 } }, { ...meta(), kinds: [{ kind: "webhook" }] }]) {
    assert.throws(() => assertNotificationMeta(invalid));
  }
});

test("notification digest validation never silently omits an invalid submitted interval", () => {
  assert.equal(notificationDigestMinutes("", 30), 30);
  for (const raw of ["1", "30", "1440", " 45 "]) assert.equal(notificationDigestMinutes(raw, 30), Number(raw));
  for (const raw of ["0", "-1", "1.5", "1441", "NaN", "bad", "Infinity"]) assert.throws(() => notificationDigestMinutes(raw, 30));
});

test("saved notification response is checked before acknowledging or replacing a draft", () => {
  assert.deepEqual(readNotificationSettings({ notify_enabled: false, notify_public_base_url: "", notify_digest_interval_min: 45 }), { enabled: false, publicBaseURL: "", digestMinutes: 45 });
  for (const partial of [{}, { notify_enabled: false }, { notify_enabled: true, notify_public_base_url: "saved" }, { notify_enabled: false, notify_public_base_url: "", notify_digest_interval_min: 0 }]) assert.throws(() => readNotificationSettings(partial));
});

const notify = fs.readFileSync(new URL("../app/(main)/system/notify/page.tsx", import.meta.url), "utf8");
const tools = fs.readFileSync(new URL("../app/(main)/system/tools/page.tsx", import.meta.url), "utf8");
test("notification refresh has request and dirty-draft ownership and unknown-state save guards", () => {
  assert.match(notify, /request !== requests\.current\.meta/);
  assert.match(notify, /!globalDraft\.current\.dirty && draftVersion === globalDraft\.current\.version/);
  assert.match(notify, /if \(!metaKnown \|\| metaLoading \|\| !meta \|\| globalOperation\.current\) return/);
  assert.match(notify, /readNotificationSettings\(await api\.setSettings/);
});
test("tool mutation callbacks do not close newer editors and test output belongs to its submitted spec", () => {
  assert.match(tools, /onSaved\(current && latestDraft\.current === submitted\)/);
  assert.match(tools, /if \(closeEditor\) setCustomEdit\(null\)/);
  assert.match(tools, /submitted === latestTest\.current/);
  assert.match(tools, /generation === lifetime\.current\.generation/);
  assert.match(tools, /if \(testBusy\.current\) return/);
  assert.match(tools, /会实际执行一次/);
});
