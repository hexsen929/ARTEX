const assert = require("node:assert/strict");
const fs = require("node:fs");
const path = require("node:path");
const test = require("node:test");
const vm = require("node:vm");
const ts = require("typescript");

// Exercise the real API methods with fixture HTTP responses, without a running
// backend or Next.js. Only the optional demo transport is replaced.
const compiled = ts.transpileModule(fs.readFileSync(path.join(__dirname, "api.ts"), "utf8"), {
  compilerOptions: { target: ts.ScriptTarget.ES2022, module: ts.ModuleKind.CommonJS },
}).outputText;

function client(payload, calls = []) {
  const exports = {};
  vm.runInNewContext(compiled, {
    exports,
    process: { env: {} },
    URLSearchParams,
    require: (id) => {
      if (id === "@/lib/auth") return { auth: { clearToken: () => assert.fail("unexpected logout") } };
      if (id === "@/lib/auth-refresh") return { refreshAccessToken: async () => assert.fail("unexpected token refresh") };
      if (id === "@/lib/sse-target") return {};
      if (id === "@/lib/topology-mapping") return {};
      if (id === "@/lib/mock/enabled") return { MOCK: false };
      if (id === "@/lib/mock/handler") return { mockHandle: () => assert.fail("unexpected demo request") };
      throw new Error(`Unexpected runtime import: ${id}`);
    },
    fetch: async (url, init) => { calls.push({ url, init }); return { ok: true, status: 200, json: async () => payload }; },
  });
  return exports.api;
}

for (const method of ["proxyPool"]) {
  for (const value of [null, undefined]) {
    test(`${method} normalizes ${value} collections before settings renders them`, async () => {
      const result = await client({ entries: value, switch_log: value, errors: value, active_id: "" })[method]({});
      assert.ok(Array.isArray(result.entries));
      assert.equal(result.entries.length, 0);
      assert.ok(Array.isArray(result.switch_log));
      assert.equal(result.switch_log.length, 0);
      if (method === "importProxyPool") {
        assert.ok(Array.isArray(result.errors));
        assert.equal(result.errors.length, 0);
      }
    });
  }

  test(`${method} preserves proxy entries, switch history and active selection`, async () => {
    const payload = {
      entries: [{ id: "proxy-a", enabled: true }],
      switch_log: [{ at: "2026-11-02T08:00:00Z", to: "proxy-a", reason: "manual" }],
      errors: [{ line: 2, error: "invalid URL" }],
      active_id: "proxy-a",
    };
    const result = await client(payload)[method]({});
    assert.equal(result.entries, payload.entries);
    assert.equal(result.switch_log, payload.switch_log);
    assert.equal(result.active_id, "proxy-a");
    if (method === "importProxyPool") assert.equal(result.errors, payload.errors);
  });
}

// Batch mutations deliberately request a small acknowledgement now. The page
// fetches and validates one persisted settings snapshot after confirmation.
for (const [method, route] of [["importProxyPool", "/settings/proxy-pool/import"], ["testAllProxyPool", "/settings/proxy-pool/test-all"]]) {
  test(`${method} uses a bounded queued acknowledgement without retrieving another pool`, async () => {
    const calls = [];
    const payload = { ok: true, probe_queued: true, errors: [] };
    const result = await client(payload, calls)[method]({ text: "fixture.invalid:8080" });
    assert.equal(calls.length, 1);
    assert.equal(calls[0].url, `/api${route}?summary=1`);
    assert.equal(calls[0].init.method, "POST");
    assert.equal(result, payload);
    assert.equal(result.entries, undefined);
    assert.equal(result.switch_log, undefined);
  });
  test(`${method} does not turn an unconfirmed response into completed work`, async () => {
    const payload = { probe_queued: false, errors: null };
    const result = await client(payload)[method]({});
    assert.equal(result.ok, undefined);
    assert.equal(result.probe_queued, false);
    assert.equal(result.errors, null);
  });
}

test("navbar egress settings uses the lightweight endpoint", async () => {
  const calls = [];
  const payload = { show_egress_ip: false, global_proxy: "" };
  assert.equal(await client(payload, calls).egressSettings(), payload);
  assert.equal(calls[0].url, "/api/settings/egress");
});

test("bulk proxy mutation sends one bounded request and preserves the exact selected IDs and confirmation", async () => {
  const calls = [];
  const payload = { action: "delete", ids: ["proxy-a", "proxy-z"], only_unhealthy: true, expected_active_id: "proxy-current" };
  const response = { ok: true, changed: 1, skipped: 1, active_id: "proxy-current", active_changed: false };
  assert.equal(await client(response, calls).bulkProxyPool(payload), response);
  assert.equal(calls.length, 1);
  assert.equal(calls[0].url, "/api/settings/proxy-pool/bulk");
  assert.equal(calls[0].init.method, "POST");
  assert.deepEqual(JSON.parse(calls[0].init.body), payload);
});
