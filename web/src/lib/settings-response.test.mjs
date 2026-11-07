import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import test from "node:test";

import {
  assertSettingsSnapshot,
  egressSettingsSnapshot,
} from "../app/(main)/system/settings/_components/settings-response.ts";

const fixture = () => ({
  roe_enforcement: "warn",
  llm_records_retention_days: 30,
  finding_verify_mode: "auto",
  credential_agent_enabled: false,
  credential_traffic_binding: false,
  task_intelligence_enabled: false,
  task_auto_title_enabled: false,
  chat_auto_title_enabled: false,
  traffic_capture: true,
  browser_screenshot_enabled: false,
  browser_screenshot_timestamp_enabled: false,
  agent_traffic_binding: false,
  llm_record: false,
  deepseek_search_key_set: false,
  proxy_pool: [], proxy_pool_active_id: "", proxy_pool_switch_log: [],
  proxy_rotate_timed_enabled: false, proxy_rotate_interval_seconds: 30,
  proxy_rotate_on_failure: false, proxy_rotate_strategy: "sequential",
  proxy_health_fail_threshold: 3, proxy_health_cooldown_seconds: 60,
  proxy_health_check_interval_seconds: 30, proxy_reconnect_max_attempts: 5,
  proxy_reconnect_interval_seconds: 30,
  web_search_enabled: false,
  web_search_backend: "ddgs",
  web_search_proxy: "",
  brave_key_set: false,
  tavily_key_set: false,
  workers: 3,
  worker_pools: [],
  global_proxy: "",
  show_egress_ip: false,
  opsec_enabled: false,
  finding_inline_merge_enabled: false,
  python_interpreter: "",
  callback_addr: "",
  callback_addr_source: "unset",
  callback_addr_locked: false,
  task_concurrency_enabled: false,
  task_concurrency_limit: 5,
  llm_pool_enabled: false,
  llm_pool_bind_fallback: false,
  constraints_inject_planner: true,
  constraints_inject_worker: true,
  noa_compaction: false,
  notify_enabled: true,
  notify_public_base_url: "",
  notify_digest_interval_min: 30,
});

test("settings snapshot accepts explicit false and an empty pool without changing the response", () => {
  const settings = fixture();
  const before = structuredClone(settings);
  assert.doesNotThrow(() => assertSettingsSnapshot(settings, 50));
  assert.deepEqual(settings, before);
  assert.equal(settings.credential_agent_enabled, false);
  assert.equal(settings.opsec_enabled, false);
});

test("every full Settings boolean and string must be known before apply", () => {
  for (const [key, value] of Object.entries(fixture())) {
    if (typeof value !== "boolean" && typeof value !== "string") continue;
    const missing = fixture();
    delete missing[key];
    assert.throws(() => assertSettingsSnapshot(missing, 50), undefined, `missing ${key} was accepted`);
    const malformed = { ...fixture(), [key]: typeof value === "boolean" ? "false" : null };
    assert.throws(() => assertSettingsSnapshot(malformed, 50), undefined, `invalid ${key} was accepted`);
    const original = structuredClone(malformed);
    try { assertSettingsSnapshot(malformed, 50); } catch {}
    assert.deepEqual(malformed, original, `${key} was coerced to a default`);
  }
});

test("callback source and lock state stay authoritative and optional error text stays typed", () => {
  for (const source of ["unset", "settings", "config", "env"]) {
    const settings = { ...fixture(), callback_addr_source: source, callback_addr_locked: source === "env" };
    assert.doesNotThrow(() => assertSettingsSnapshot(settings, 50));
    assert.doesNotThrow(() => assertSettingsSnapshot({ ...settings, callback_addr_error: "fixture invalid address" }, 50));
  }
  assert.throws(() => assertSettingsSnapshot({ ...fixture(), callback_addr_source: "unknown" }, 50), /回连来源/);
  assert.throws(() => assertSettingsSnapshot({ ...fixture(), callback_addr_source: "env", callback_addr_locked: false }, 50), /回连来源/);
  assert.throws(() => assertSettingsSnapshot({ ...fixture(), callback_addr_error: null }, 50), /回连来源/);
  for (const key of ["task_concurrency_limit", "notify_digest_interval_min"]) {
    for (const value of [undefined, null, "5", 0, -1, 1.5]) {
      assert.throws(() => assertSettingsSnapshot({ ...fixture(), [key]: value }, 50), /并发或通知周期/);
    }
  }
});

test("unknown or partial settings responses cannot become default worker pool writes", () => {
  const missingPools = fixture();
  delete missingPools.worker_pools;
  for (const value of [null, undefined, [], "unknown", {}, { error: "unavailable" }, missingPools]) {
    assert.throws(() => assertSettingsSnapshot(value, 50), /设置|Worker/);
  }
  const invalidBoolean = { ...fixture(), credential_agent_enabled: "false" };
  assert.throws(() => assertSettingsSnapshot(invalidBoolean, 50), /完整设置/);
});

test("worker pool snapshot validates identity, counts, duplicates and total bounds without mutation", () => {
  const valid = { ...fixture(), workers: 5, worker_pools: [{ profile_id: "11", workers: 2 }, { profile_id: "12", workers: 3 }] };
  const before = structuredClone(valid);
  assert.doesNotThrow(() => assertSettingsSnapshot(valid, 50));
  assert.deepEqual(valid, before);
  for (const workers of [undefined, null, "3", 0, -1, 1.5, 51, Number.NaN]) {
    assert.throws(() => assertSettingsSnapshot({ ...fixture(), workers }, 50), /并发数/);
  }
  for (const worker_pools of [
    null,
    {},
    [null],
    [{ profile_id: "", workers: 1 }],
    [{ profile_id: 11, workers: 1 }],
    [{ profile_id: "11", workers: 0 }],
    [{ profile_id: "11", workers: "3" }],
    [{ profile_id: "11", workers: 1 }, { profile_id: "11", workers: 2 }],
    [{ profile_id: "11", workers: 30 }, { profile_id: "12", workers: 30 }],
  ]) {
    assert.throws(() => assertSettingsSnapshot({ ...fixture(), worker_pools }, 50), /Worker/);
  }
});

test("egress probing needs an explicit boolean opt-in and a known saved proxy", () => {
  assert.deepEqual(egressSettingsSnapshot(fixture()), { enabled: false, globalProxy: "" });
  assert.deepEqual(egressSettingsSnapshot({ show_egress_ip: true, global_proxy: "http://fixture.invalid:8080" }), {
    enabled: true,
    globalProxy: "http://fixture.invalid:8080",
  });
  for (const value of [null, {}, { show_egress_ip: "true", global_proxy: "" }, { show_egress_ip: true }]) {
    assert.throws(() => egressSettingsSnapshot(value), /出口显示设置/);
  }
});

test("merged settings retain callback and inline merge cards behind the new category layout", () => {
  const page = readFileSync(new URL("../app/(main)/system/settings/page.tsx", import.meta.url), "utf8");
  assert.match(page, /isSettingVisible\("network", "回连 callback 地址 主机"\) \? <CallbackAddressCard/);
  assert.match(page, /同入口候选聚合/);
  assert.match(page, /onCheckedChange=\{\(enabled\) => void toggleInlineFindingMerge\(enabled\)\}/);
  assert.match(page, /const apply = React\.useCallback\(\(s: Settings\) => \{\s*try \{\s*assertSettingsSnapshot\(s, MAX_TASK_WORKER_COUNT\)/);
  assert.doesNotMatch(page, /setWorkerPools\(s\.worker_pools \?\? \[\]\)/);
  assert.match(page, /页面载入和手动刷新会访问公共 IP 回显服务/);
});

test("egress provider suppresses stale results and consumes rejected UI promises", () => {
  const provider = readFileSync(new URL("../app/(main)/_components/egress-ip-banner.tsx", import.meta.url), "utf8");
  assert.match(provider, /if \(snapshot\.enabled\) void probeForProxy\(snapshot\.globalProxy\)\.catch/);
  assert.match(provider, /mounted\.current && version === probeVersion\.current/);
  assert.match(provider, /request !== probeVersion\.current/);
  assert.match(provider, /if \(preferencePending\.current\) throw new Error/);
  assert.match(provider, /if \(!snapshot\.enabled\) \{\s*clearProbe\(\)/);
  assert.match(provider, /onClick=\{\(\) => void probeEgress\(\)\.catch/);
  assert.match(provider, /void dismiss\(\)\.catch/);
  assert.doesNotMatch(provider, /api\.settings\(\)|\.settings\(\)/);
  assert.match(provider, /api\.egressSettings\(\)/);
  assert.match(provider, /busy \|\| probesInFlight\.current > 0/);
  assert.match(provider, /document\.visibilityState === "hidden"/);
  assert.match(provider, /if \(!active \|\| !mounted\.current/);
  assert.match(provider, /finally \{\s*busy = false/);
});


test("proxy snapshots reject unknown routes and malformed policies without inventing defaults", () => {
  for (const key of ["proxy_pool", "proxy_pool_switch_log"]) {
    for (const value of [undefined, null, {}]) {
      assert.throws(() => assertSettingsSnapshot({ ...fixture(), [key]: value }, 50), /代理池/);
    }
  }
  for (const key of ["proxy_rotate_interval_seconds", "proxy_health_fail_threshold", "proxy_health_cooldown_seconds", "proxy_health_check_interval_seconds", "proxy_reconnect_max_attempts", "proxy_reconnect_interval_seconds"]) {
    for (const value of [undefined, null, "30", 0, -1, 1.5, Number.NaN]) {
      assert.throws(() => assertSettingsSnapshot({ ...fixture(), [key]: value }, 50), /代理池/);
    }
  }
  for (const strategy of ["", "RANDOM", "unknown"]) {
    assert.throws(() => assertSettingsSnapshot({ ...fixture(), proxy_rotate_strategy: strategy }, 50), /轮换策略/);
  }
  assert.throws(() => assertSettingsSnapshot({ ...fixture(), proxy_pool_active_id: "missing" }, 50), /选择未知/);
  const entry = { id: "fixture-proxy", url: "http://127.0.0.1:8080", enabled: true };
  const settings = { ...fixture(), proxy_pool: [entry], proxy_pool_active_id: entry.id };
  const before = structuredClone(settings);
  assert.doesNotThrow(() => assertSettingsSnapshot(settings, 50));
  assert.deepEqual(settings, before);
  assert.throws(() => assertSettingsSnapshot({ ...settings, proxy_pool: [entry, entry] }, 50), /条目无效/);
});

test("proxy tests do not silently request task resume and mutations reload the persisted route", () => {
  const page = readFileSync(new URL("../app/(main)/system/settings/page.tsx", import.meta.url), "utf8");
  assert.doesNotMatch(page, /resume:\s*true/);
  assert.match(page, /apply\(await api\.settings\(\)\)/);
  assert.match(page, /fieldset disabled=\{!loaded \|\| proxyPoolBusy\}/);
  assert.match(page, /api\.activateProxyPool\(id, selection\)/);
});
