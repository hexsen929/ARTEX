import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import test from "node:test";
import { useFilter } from "@base-ui/react/use-filter";
import React from "react";
import { renderToStaticMarkup } from "react-dom/server";
import { llmProfileSearchLabel } from "./llm-profile-search.ts";

const profiles = [
  { id: "13", name: "中转配置", model: "glm-5.3-flash", base_url: "https://relay.example/v1", api_key_hint: "secret-only" },
  { id: "27", name: "Second profile", model: "qwen3.8-flash", base_url: "https://other.example/v1" },
];

// Exercise the same Base UI collator used by the combobox's label filter.
function search(query) {
  let result;
  function Probe() {
    const filter = useFilter({ sensitivity: "base" });
    result = profiles.filter((profile) => filter.contains(llmProfileSearchLabel(profile), query)).map((p) => p.id);
    return null;
  }
  renderToStaticMarkup(React.createElement(Probe));
  return result;
}

test("search finds saved profiles by name, model, endpoint and ID", () => {
  for (const query of ["中转", "GLM-5.3", "relay.example", "#13"]) assert.deepEqual(search(query), ["13"]);
  assert.deepEqual(search("qwen3.8"), ["27"]);
  assert.deepEqual(search("does-not-exist"), []);
});

test("profiles sharing a display name remain distinct ID-valued options", () => {
  const duplicate = { ...profiles[0], id: "99", model: "different-model" };
  assert.notEqual(llmProfileSearchLabel(profiles[0]), llmProfileSearchLabel(duplicate));
  assert.ok(!llmProfileSearchLabel(profiles[0]).includes("secret-only"));
});

test("label generation tolerates profiles without an optional endpoint", () => {
  assert.equal(llmProfileSearchLabel({ id: "5", name: "Fixture", model: "model-x" }), "Fixture model-x #5");
});

const picker = readFileSync(new URL("../components/task-llm-profile-chain.tsx", import.meta.url), "utf8");
const detail = readFileSync(new URL("../app/(main)/function/tasks/detail/page.tsx", import.meta.url), "utf8");

test("combobox searches labels but submits stable IDs", () => {
  assert.match(picker, /itemToStringLabel=\{profileLabel\}/);
  assert.match(picker, /itemToStringValue=\{\(id\) => id\}/);
  assert.ok(!picker.includes("itemToStringValue={profileLabel}"));
});

test("chips and popup use the same anchor inside the parent popover", () => {
  assert.match(picker, /<ComboboxChips ref=\{comboboxAnchor\}/);
  assert.match(picker, /<ComboboxContent anchor=\{comboboxAnchor\} portalContainer=\{portalContainer\}/);
});

test("opening the editor refreshes profiles without discarding the existing list on errors", () => {
  assert.match(detail, /if \(next\) \{\s*syncDraft\(\);\s*void onRefreshProfiles\(\);/);
  assert.match(detail, /onRefreshProfiles=\{refreshProfiles\}/);
  assert.match(detail, /if \(request === profilesRequest.current\) setProfiles\(next\)/);
  assert.ok(!detail.includes(".catch(() => setProfiles([]))"));
  assert.match(detail, /LLM 配置加载失败/);
});
