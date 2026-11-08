const assert = require("node:assert/strict");
const test = require("node:test");
require("ts-node").register({ skipProject: true, transpileOnly: true,
  compilerOptions: { target: "ES2022", module: "CommonJS", moduleResolution: "Node" } });
const data = require("./data.ts");
const { mockHandle } = require("./handler.ts");

test("mock saved egress service preserves omitted/null values and explicit clear", async () => {
  const original = structuredClone(data.settings);
  try {
    assert.equal((await mockHandle("GET", "/settings")).egress_probe_url, "");
    const value = "http://echo.fixture.invalid/ip";
    assert.equal((await mockHandle("PUT", "/settings", JSON.stringify({ egress_probe_url: ` ${value} ` }))).egress_probe_url, value);
    assert.equal((await mockHandle("PUT", "/settings", JSON.stringify({ opsec_enabled: false }))).egress_probe_url, value);
    assert.equal((await mockHandle("PUT", "/settings", JSON.stringify({ egress_probe_url: null }))).egress_probe_url, value);
    const before = structuredClone(data.settings);
    await assert.rejects(mockHandle("PUT", "/settings", JSON.stringify({ egress_probe_url: 12, opsec_enabled: true })));
    assert.deepEqual(data.settings, before);
    const diagnosis = await mockHandle("POST", "/settings/proxy-pool/fixture/diagnose");
    assert.equal(diagnosis.probe_mode, "configured");
    assert.equal(diagnosis.probe_url, value);
    assert.equal(diagnosis.http_skipped, true);
    assert.equal((await mockHandle("PUT", "/settings", JSON.stringify({ egress_probe_url: "" }))).egress_probe_url, "");
  } finally { Object.assign(data.settings, original); }
});

test("mock task creation and metadata use an explicit opt-in sharing policy", async () => {
  const first = await mockHandle("POST", "/tasks", JSON.stringify({ description: "fixture", goal: "fixture" }));
  assert.equal(first.share_global_assets, false);
  const enabled = await mockHandle("PATCH", `/tasks/${first.id}`, JSON.stringify({ share_global_assets: true }));
  assert.equal(enabled.id, first.id);
  assert.equal(enabled.share_global_assets, true);
  assert.equal((await mockHandle("PATCH", `/tasks/${first.id}`, JSON.stringify({ name: "renamed" }))).share_global_assets, true);
  await assert.rejects(mockHandle("PATCH", `/tasks/${first.id}`, JSON.stringify({ share_global_assets: "false" })));
  assert.equal((await mockHandle("GET", `/tasks/${first.id}`)).share_global_assets, true);
  assert.equal((await mockHandle("PATCH", `/tasks/${first.id}`, JSON.stringify({ share_global_assets: false }))).share_global_assets, false);
  const second = await mockHandle("POST", "/tasks", JSON.stringify({ description: "fixture", goal: "fixture", share_global_assets: true }));
  assert.equal(second.share_global_assets, true);
});
