import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import test from "node:test";

import {
  CREDENTIAL_SECRET_TTL_MS,
  credentialExternalURL,
  credentialMatchesQuery,
  credentialPage,
  credentialStatus,
  credentialTaskHref,
} from "./credentials.ts";

const fixture = {
  id: 7,
  task_id: 20,
  task_name: "fixture web task",
  task_available: true,
  cred_type: "token",
  username: "fixture-user",
  target: "https://fixture.invalid",
  target_label: "fixture.invalid",
  domain: "CORP",
  source: "registered fixture evidence",
  source_url: "https://fixture.invalid/evidence",
  secret_masked: "ab****yz",
  verified: false,
  verification_status: "unverified",
  created_at: "2026-10-29T00:00:00Z",
};

test("legacy false stays unverified and explicit failure stays distinct", () => {
  assert.equal(credentialStatus({ verified: false }), "unverified");
  assert.equal(credentialStatus({ verified: true }), "verified");
  assert.equal(credentialStatus({ verified: false, verification_status: "failed" }), "failed");
  assert.equal(credentialStatus({ verified: false, verification_status: "invalid" }), "unverified");
});

test("external credential links discard queries/fragments and reject unsafe or credentialed URLs", () => {
  assert.equal(
    credentialExternalURL("https://fixture.invalid/evidence?token=secret#secret"),
    "https://fixture.invalid/evidence",
  );
  assert.equal(credentialExternalURL("http://[::1]:8787/path?q=1"), "http://[::1]:8787/path");
  const credentialed = new URL("https://fixture.invalid/evidence");
  credentialed.username = "fixture-user";
  assert.equal(credentialExternalURL(credentialed.href), undefined);
  credentialed.password = "fixture-pass";
  assert.equal(credentialExternalURL(credentialed.href), undefined);
  for (const value of [
    "javascript:alert(1)",
    "data:text/html,test",
    "//fixture.invalid",
    "/path",
    "fixture.invalid",
    "https:\n//fixture.invalid",
    "https://fixture.invalid/a b",
    "",
  ]) {
    assert.equal(credentialExternalURL(value), undefined, value);
  }
});

test("only available tasks receive valid internal links", () => {
  assert.equal(credentialTaskHref(fixture), "/function/tasks/detail?id=20");
  assert.equal(credentialTaskHref({ task_id: 20, task_available: false }), undefined);
  for (const task_id of [0, -1, NaN, Infinity, 2.5, Number.MAX_SAFE_INTEGER + 1]) {
    assert.equal(credentialTaskHref({ task_id, task_available: true }), undefined);
  }
});

test("credential search covers provenance/targets/domain without searching secrets", () => {
  for (const query of [
    "fixture-user",
    "任务 #20",
    "CORP",
    "registered evidence",
    "fixture.invalid",
    "web task token",
    "未验证",
  ])
    assert.equal(credentialMatchesQuery(fixture, query), true, query);
  assert.equal(credentialMatchesQuery(fixture, "ab****yz"), false);
  assert.equal(credentialMatchesQuery(fixture, "fixture-user missing"), false);
});

test("pagination clamps empty, stale and malformed page requests", () => {
  assert.deepEqual(credentialPage([], 99), { items: [], page: 1, pages: 1 });
  assert.deepEqual(credentialPage([1, 2, 3], 99, 2), { items: [3], page: 2, pages: 2 });
  assert.deepEqual(credentialPage([1, 2, 3], -2, 2), { items: [1, 2], page: 1, pages: 2 });
  assert.equal(credentialPage([1], Infinity).page, 1);
  assert.equal(credentialPage([1], 1, 0).pages, 1);
  assert.equal(CREDENTIAL_SECRET_TTL_MS, 30_000);
});

test("sensitive API requests opt out of cache; dialogs invalidate late reveal responses", () => {
  const api = readFileSync(new URL("./api.ts", import.meta.url), "utf8");
  const tab = readFileSync(new URL("../app/(main)/intranet/_components/creds-tab.tsx", import.meta.url), "utf8");
  const page = readFileSync(new URL("../app/(main)/intranet/page.tsx", import.meta.url), "utf8");
  assert.match(api, /revealCredential:[\s\S]*?method: "POST"[\s\S]*?cache: "no-store"[\s\S]*?signal,/);
  assert.match(tab, /revealGeneration\.current\+\+/);
  assert.match(tab, /generation === revealGeneration\.current/);
  assert.match(tab, /addEventListener\("visibilitychange"/);
  assert.match(tab, /addEventListener\("pagehide"/);
  assert.match(tab, /setTimeout\(clearSecret, CREDENTIAL_SECRET_TTL_MS\)/);
  assert.doesNotMatch(tab, /localStorage|sessionStorage|console\./);
  assert.doesNotMatch(tab, /title=\{(?:secret|response\.secret)\}|data-[\w-]+=\{(?:secret|response\.secret)\}/);
  assert.match(tab, /rel="noopener noreferrer"\s+referrerPolicy="no-referrer"/);
  assert.match(page, /tab === "creds"[\s\S]*?<CredsTab[\s\S]*?key=\{taskId\}/);
  assert.match(page, /current\.map\(\(item\) => \(?item\.id === updated\.id \? updated : item/);
});
