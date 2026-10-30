const assert = require("node:assert/strict");
const test = require("node:test");

require("ts-node").register({
  skipProject: true,
  transpileOnly: true,
  compilerOptions: { target: "ES2022", module: "CommonJS", moduleResolution: "Node" },
});
const data = require("./data.ts");
const { mockHandle } = require("./handler.ts");
const { assertSettingsSnapshot } = require("../../app/(main)/system/settings/_components/settings-response.ts");

test("mock settings return a complete known snapshot including default warn", async () => {
  const settings = await mockHandle("GET", "/settings");
  assert.equal(settings.roe_enforcement, "warn");
  assert.doesNotThrow(() => assertSettingsSnapshot(settings, 50));
});

test("mock RoE settings validate before any mixed-field mutation and preserve omission", async () => {
  const original = structuredClone(data.settings);
  try {
    for (const mode of ["strict", "warn", "off"]) {
      const saved = await mockHandle("PUT", "/settings", JSON.stringify({ roe_enforcement: mode }));
      assert.equal(saved.roe_enforcement, mode);
      assert.doesNotThrow(() => assertSettingsSnapshot(saved, 50));
    }
    const selected = await mockHandle("PUT", "/settings", JSON.stringify({ roe_enforcement: "strict" }));
    const unrelated = await mockHandle("PUT", "/settings", JSON.stringify({ python_interpreter: "/fixture/python" }));
    assert.equal(unrelated.roe_enforcement, selected.roe_enforcement);
    const before = structuredClone(data.settings);
    for (const mode of [null, "", "WARN", " strict ", "unknown", false, 0, [], {}]) {
      await assert.rejects(
        () => mockHandle("PUT", "/settings", JSON.stringify({ roe_enforcement: mode, workers: 9 })),
        /roe_enforcement/,
      );
      assert.deepEqual(data.settings, before, "invalid RoE must not partially update another setting");
    }
  } finally {
    for (const key of Object.keys(data.settings)) delete data.settings[key];
    Object.assign(data.settings, original);
  }
});
