import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import test from "node:test";

import {
  filterProxyPool,
  PROXY_POOL_PAGE_SIZE,
  proxyPoolPage,
  proxySearchAddress,
  summarizeProxyImportErrors,
} from "./proxy-pool-pagination.ts";

const fixture = (i) => ({
  id: `proxy-${i}`, url: `http://proxy-${i}.invalid:8080`,
  name: `Fixture ${i}`, remark: i % 2 ? "odd region" : "even region",
  enabled: i % 2 === 0, health: "healthy", egress_ip: `192.0.2.${i % 255}`,
});

test("large proxy pools never return more than one fixed page or reorder entries", () => {
  const entries = Array.from({ length: 50_001 }, (_, i) => fixture(i));
  const first = proxyPoolPage(entries, 1);
  assert.equal(PROXY_POOL_PAGE_SIZE, 50);
  assert.equal(first.items.length, 50);
  assert.equal(first.items[0], entries[0]);
  assert.equal(first.pages, 1001);
  const last = proxyPoolPage(entries, 1001);
  assert.deepEqual(last.items, [entries[50_000]]);
  assert.equal(entries.length, 50_001);
});

test("pagination clamps empty/malformed/stale pages after deleting a last-page row", () => {
  assert.deepEqual(proxyPoolPage([], 999), { items: [], page: 1, pages: 1 });
  const entries = Array.from({ length: 51 }, (_, i) => fixture(i));
  assert.equal(proxyPoolPage(entries, 999).page, 2);
  assert.equal(proxyPoolPage(entries.slice(0, 50), 2).page, 1);
  for (const request of [0, -3, NaN, Infinity, -Infinity]) {
    assert.equal(proxyPoolPage(entries, request).page, 1);
  }
});

test("search covers names, masked addresses, remarks, egress and state without userinfo", () => {
  const credentialed = "socks5://" + ["PRIVATE_USER", "PRIVATE_PASS"].join(":") + "@" + "host.invalid:1080";
  const entries = [fixture(0), fixture(1), { ...fixture(2), url: credentialed }];
  assert.equal(filterProxyPool(entries, ""), entries);
  assert.deepEqual(filterProxyPool(entries, "fixture 1 odd"), [entries[1]]);
  assert.deepEqual(filterProxyPool(entries, "host.invalid"), [entries[2]]);
  assert.deepEqual(filterProxyPool(entries, "PRIVATE_USER"), []);
  assert.deepEqual(filterProxyPool(entries, "PRIVATE_PASS"), []);
  assert.equal(proxySearchAddress("http://" + "user:pass" + "@" + "host.invalid:8080"), "http://***@host.invalid:8080");
  assert.deepEqual(filterProxyPool(entries, "192.0.2.0 已启用"), [entries[0]]);
  assert.equal(entries[2].url, credentialed);
});

test("bulk import errors have a bounded summary and never include input strings", () => {
  const errors = Array.from({ length: 10_000 }, (_, i) => ({ line: i + 1, input: "PRIVATE_PROXY_INPUT", error: `invalid format ${"x".repeat(300)}` }));
  const summary = summarizeProxyImportErrors(errors);
  assert.match(summary, /共 10000 行/);
  assert.match(summary, /仅显示前 5 条/);
  assert.equal((summary.match(/第\d+行/g) || []).length, 5);
  assert.ok(summary.length < 900);
  assert.ok(!summary.includes("PRIVATE_PROXY_INPUT"));
});

test("settings do not mount a hidden whole pool or keep an import textarea in the parent render path", () => {
  const page = readFileSync(new URL("../app/(main)/system/settings/page.tsx", import.meta.url), "utf8");
  const list = readFileSync(new URL("../app/(main)/system/settings/_components/proxy-pool-list.tsx", import.meta.url), "utf8");
  assert.ok(!page.includes("proxyPool.map("));
  assert.ok(!page.includes("proxyPoolText"));
  assert.match(page, /isSettingVisible\("traffic", "全局代理 代理池[^\n]+\? <Card/);
  assert.match(page, /const proxyPoolDraft = React\.useRef\(""\)/);
  assert.match(page, /summarizeProxyImportErrors\(result\.errors\)/);
  assert.match(page, /proxyPoolOperationPending\.current = true/);
  assert.match(page, /if \(proxyPoolDraft\.current === text\) proxyPoolDraft\.current = ""/);
  assert.match(page, /toast\.success\("全部代理探测已排队/);
  assert.match(page, /result\.ok !== true \|\| result\.probe_queued !== true \|\| !Array\.isArray\(result\.errors\)/);
  assert.doesNotMatch(page, /已完成全部代理探测/);
  assert.match(list, /items\.map\(\(entry\)/);
  assert.match(list, /useDeferredValue\(query\)/);
  assert.match(list, /data-testid="proxy-pool-row" data-proxy-id=\{entry\.id\}/);
  assert.match(list, /onClick=\{onRefresh\} disabled=\{busy\}>刷新状态/);
  assert.match(list, /style=\{\{ fieldSizing: "fixed" \}\}/);
});
