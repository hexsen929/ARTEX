const assert = require("node:assert/strict");
const fs = require("node:fs");
const path = require("node:path");
const test = require("node:test");
const vm = require("node:vm");
const ts = require("typescript");

const compiled = ts.transpileModule(fs.readFileSync(path.join(__dirname, "api.ts"), "utf8"), {
  compilerOptions: { target: ts.ScriptTarget.ES2022, module: ts.ModuleKind.CommonJS },
}).outputText;

function client(fetch, refreshAccessToken = async () => assert.fail("unexpected token refresh")) {
  const exports = {};
  vm.runInNewContext(compiled, {
    exports,
    process: { env: {} },
    URLSearchParams,
    AbortController,
    DOMException,
    window: {},
    localStorage: { getItem: () => "test-admin-token" },
    require: (id) => {
      if (id === "@/lib/auth") return { auth: { clearToken: () => assert.fail("unexpected logout") } };
      if (id === "@/lib/auth-refresh") return { refreshAccessToken };
      if (id === "@/lib/sse-target") return {};
      if (id === "@/lib/topology-mapping") return {};
      if (id === "@/lib/mock/enabled") return { MOCK: false };
      if (id === "@/lib/mock/handler") return { mockHandle: () => assert.fail("unexpected demo request") };
      throw new Error(`Unexpected runtime import: ${id}`);
    },
    fetch,
  });
  return exports.api;
}

test("version queries carry the admin token and request fresh results after restart", async () => {
  const versions = ["v1.0.0", "v1.0.1"];
  const api = client(async (url, init) => {
    assert.equal(url, "/api/version");
    assert.equal(init.headers.Authorization, "Bearer test-admin-token");
    assert.equal(init.cache, "no-store");
    return { ok: true, status: 200, json: async () => ({ version: versions.shift() }) };
  });

  assert.equal((await api.version()).version, "v1.0.0");
  assert.equal((await api.version()).version, "v1.0.1");
});

test("version queries can resume after a temporary restart connection failure", async () => {
  let attempts = 0;
  const api = client(async () => {
    if (++attempts === 1) throw new TypeError("connection unavailable during restart");
    return { ok: true, status: 200, json: async () => ({ version: "v1.0.1" }) };
  });

  await assert.rejects(api.version(), /connection unavailable/);
  assert.equal((await api.version()).version, "v1.0.1");
});

test("version verification forwards the caller abort signal without losing authentication", async () => {
  const controller = new AbortController();
  const api = client(async (url, init) => {
    assert.equal(url, "/api/version");
    assert.equal(init.headers.Authorization, "Bearer test-admin-token");
    assert.equal(init.signal, controller.signal);
    controller.abort();
    assert.equal(init.signal.aborted, true);
    throw new DOMException("fixture cancelled", "AbortError");
  });
  await assert.rejects(api.version(controller.signal), { name: "AbortError" });
});


test("aborting a version poll releases a hung shared refresh without replay or logout", async () => {
  const controller = new AbortController();
  let fetched = 0, refreshing;
  const refreshStarted = new Promise((resolve) => { refreshing = resolve; });
  const api = client(async () => {
    fetched++;
    return { ok: false, status: 401 };
  }, () => {
    refreshing();
    return new Promise(() => {});
  });
  const result = api.version(controller.signal);
  await refreshStarted;
  controller.abort();
  await assert.rejects(result, { name: "AbortError" });
  assert.equal(fetched, 1, "Cancelled poll replayed after 401");
});

test("one cancelled refresh waiter cannot cancel another or replay when shared refresh later succeeds", async () => {
  const cancelled = new AbortController(), active = new AbortController();
  let resolveRefresh, bothStarted;
  const sharedRefresh = new Promise((resolve) => { resolveRefresh = resolve; });
  const bothWaiting = new Promise((resolve) => { bothStarted = resolve; });
  const attempts = new Map();
  let waiters = 0;
  const api = client(async (_url, init) => {
    const count = (attempts.get(init.signal) ?? 0) + 1;
    attempts.set(init.signal, count);
    if (count === 1) return { ok: false, status: 401 };
    assert.equal(init.signal, active.signal, "Cancelled waiter replayed its GET");
    return { ok: true, status: 200, json: async () => ({ version: "v2.4.10" }) };
  }, () => {
    if (++waiters === 2) bothStarted();
    return sharedRefresh;
  });
  const stopped = api.version(cancelled.signal), continued = api.version(active.signal);
  await bothWaiting;
  cancelled.abort();
  await assert.rejects(stopped, { name: "AbortError" });
  resolveRefresh(true);
  assert.equal((await continued).version, "v2.4.10");
  await Promise.resolve();
  assert.equal(attempts.get(cancelled.signal), 1);
  assert.equal(attempts.get(active.signal), 2);
});

test("a pre-cancelled version query does not send HTTP or start shared refresh", async () => {
  const controller = new AbortController();
  controller.abort();
  const api = client(async () => assert.fail("Cancelled request sent HTTP"));
  await assert.rejects(api.version(controller.signal), { name: "AbortError" });
});
