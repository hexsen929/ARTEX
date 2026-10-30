const assert = require("node:assert/strict");
const test = require("node:test");

require("ts-node").register({
  skipProject: true, transpileOnly: true,
  compilerOptions: { target: "ES2022", module: "CommonJS", moduleResolution: "Node" },
});
const data = require("./data.ts");
const { mockHandle } = require("./handler.ts");
const { assertSettingsSnapshot } = require("../../app/(main)/system/settings/_components/settings-response.ts");

test("mock exposes retention/verification defaults and rejects invalid mixed writes before mutation", async () => {
  const original = structuredClone(data.settings);
  const records = structuredClone(data.llmRecords);
  try {
    const initial = await mockHandle("GET", "/settings");
    assert.equal(initial.llm_records_retention_days, 30);
    assert.equal(initial.finding_verify_mode, "auto");
    assert.doesNotThrow(() => assertSettingsSnapshot(initial, 50));
    for (const days of [0, 7, 30, 106751]) {
      const saved = await mockHandle("PUT", "/settings", JSON.stringify({ llm_records_retention_days: days }));
      assert.equal(saved.llm_records_retention_days, days);
      assert.equal(saved.finding_verify_mode, "auto");
      assert.deepEqual(data.llmRecords, records, "retention settings must not delete records synchronously");
    }
    for (const mode of ["off", "auto"]) {
      const saved = await mockHandle("PUT", "/settings", JSON.stringify({ finding_verify_mode: mode }));
      assert.equal(saved.finding_verify_mode, mode);
      assert.equal(saved.llm_records_retention_days, 106751);
    }
    const before = structuredClone(data.settings);
    const invalid = [
      ...[null, "30", -1, 1.5, 106752, {}, []].map((value) => ({ llm_records_retention_days: value })),
      ...[null, "", "AUTO", "strict", false, {}].map((value) => ({ finding_verify_mode: value })),
    ];
    for (const patch of invalid) {
      await assert.rejects(() => mockHandle("PUT", "/settings", JSON.stringify({ ...patch, workers: 9 })), /llm_records_retention_days|finding_verify_mode/);
      assert.deepEqual(data.settings, before);
    }
    await mockHandle("PUT", "/settings", JSON.stringify({ python_interpreter: "/fixture/python" }));
    assert.equal(data.settings.llm_records_retention_days, before.llm_records_retention_days);
    assert.equal(data.settings.finding_verify_mode, before.finding_verify_mode);
  } finally {
    for (const key of Object.keys(data.settings)) delete data.settings[key];
    Object.assign(data.settings, original);
  }
});

test("dedicated control endpoints repair two invalid peers independently without exposing or changing the other fields", async () => {
  const original = structuredClone(data.settings);
  try {
    data.settings.roe_enforcement = "fixture-invalid-roe";
    data.settings.finding_verify_mode = "fixture-invalid-verifier";
    const unrelated = structuredClone(data.settings);
    await assert.rejects(() => mockHandle("GET", "/settings"), /roe_enforcement/);
    assert.deepEqual(await mockHandle("GET", "/settings/runtime-controls/llm_records_retention_days"), { llm_records_retention_days: 30 });
    await assert.rejects(() => mockHandle("GET", "/settings/runtime-controls/roe_enforcement"), /roe_enforcement/);
    assert.deepEqual(await mockHandle("PUT", "/settings/runtime-controls/roe_enforcement", JSON.stringify({ value: "strict" })), { roe_enforcement: "strict" });
    assert.equal(data.settings.finding_verify_mode, "fixture-invalid-verifier");
    assert.deepEqual(data.settings, { ...unrelated, roe_enforcement: "strict" });
    await assert.rejects(() => mockHandle("GET", "/settings"), /finding_verify_mode/);
    assert.deepEqual(await mockHandle("PUT", "/settings/runtime-controls/finding_verify_mode", JSON.stringify({ value: "off" })), { finding_verify_mode: "off" });
    assert.deepEqual(data.settings, { ...unrelated, roe_enforcement: "strict", finding_verify_mode: "off" });
    assert.doesNotThrow(() => assertSettingsSnapshot(data.settings, 50));
    const before = structuredClone(data.settings);
    await assert.rejects(() => mockHandle("PUT", "/settings/runtime-controls/roe_enforcement", JSON.stringify({ value: "warn", finding_verify_mode: "auto" })), /one valid value/);
    await assert.rejects(() => mockHandle("PUT", "/settings/runtime-controls/llm_records_retention_days", JSON.stringify({ value: -1 })), /one valid value/);
    await assert.rejects(() => mockHandle("PUT", "/settings/runtime-controls/not-a-control", JSON.stringify({ value: 30 })), /unknown runtime control/);
    assert.deepEqual(data.settings, before);
  } finally {
    for (const key of Object.keys(data.settings)) delete data.settings[key];
    Object.assign(data.settings, original);
  }
});
