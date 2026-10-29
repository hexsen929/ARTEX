import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import test from "node:test";
import vm from "node:vm";
import ts from "typescript";

import { SEVERITY_FILTER_OPTIONS, statusMeta } from "./status.ts";

const source = readFileSync(new URL("./api.ts", import.meta.url), "utf8");
const tree = ts.createSourceFile("api.ts", source, ts.ScriptTarget.Latest, true, ts.ScriptKind.TS);
const declarations = tree.statements.flatMap((statement) =>
  ts.isVariableStatement(statement) ? [...statement.declarationList.declarations] : [],
);
const apiNode = declarations.find((declaration) => declaration.name.getText(tree) === "api")?.initializer;
assert.ok(apiNode && ts.isObjectLiteralExpression(apiNode), "API object must remain inspectable");

function compile(text, name, context) {
  const result = ts.transpileModule(text, {
    compilerOptions: { target: ts.ScriptTarget.ES2022, module: ts.ModuleKind.None },
  });
  return vm.runInNewContext(`${result.outputText}\n${name};`, { URLSearchParams, ...context });
}

const filterDeclaration = tree.statements.find(
  (statement) => ts.isFunctionDeclaration(statement) && statement.name?.text === "findingFilterParams",
);
assert.ok(filterDeclaration, "All finding views need the shared filter serializer");
const findingFilterParams = compile(filterDeclaration.getText(tree), "findingFilterParams", {});

function method(name, context) {
  const property = apiNode.properties.find((node) => node.name?.getText(tree) === name);
  assert.ok(property && ts.isPropertyAssignment(property), `API method ${name} is missing`);
  return compile(`const invoke = ${property.initializer.getText(tree)};`, "invoke", context);
}

test("all finding query paths compose verification with upstream date and time filters", async () => {
  const query = {
    page: 3,
    pageSize: 7,
    severity: "high",
    status: "pending",
    verification: "unverified",
    task: "fixture-task",
    query: " fixture evidence ",
    sort: "time",
    timeDirection: "asc",
    from: "2026-10-28T16:00:00Z",
    to: "2026-10-29T15:59:00Z",
    assetScope: "a:42",
  };
  const expected = {
    severity: "high", status: "pending", verification: "unverified", task_id: "fixture-task",
    q: "fixture evidence", sort: "time", time_direction: "asc",
    from: query.from, to: query.to, asset_scope: "a:42",
  };
  for (const name of ["findingsPage", "findingGroups", "findingCases", "findingAssetTree"]) {
    let requested;
    const invoke = method(name, {
      findingFilterParams,
      get: async (url) => { requested = new URL(url, "https://fixture.invalid"); return {}; },
    });
    await invoke(query);
    for (const [key, value] of Object.entries(expected)) {
      assert.equal(requested.searchParams.get(key), value, `${name} lost ${key}`);
    }
    if (name !== "findingAssetTree") {
      assert.equal(requested.searchParams.get("page"), "3");
      assert.equal(requested.searchParams.get("limit"), "7");
    }
  }
  const exportNode = apiNode.properties.find((node) => node.name?.getText(tree) === "exportFindings");
  assert.match(exportNode.getText(tree), /findingFilterParams\(opts\.filters\)/);
  assert.equal(findingFilterParams({ verification: "all", severity: "all", status: "all", task: "all" }).toString(), "");
});

test("report regeneration coexists with abortable inline history and credential routes", async () => {
  let posted;
  const generate = method("generateFindingReport", { post: (path, body) => { posted = { path, body }; return { queued: true }; } });
  generate("fixture/id");
  assert.equal(posted.path, "/exploration/findings/fixture%2Fid/generate-report");
  assert.equal(JSON.stringify(posted.body), "{}");
  let requested;
  const signal = new AbortController().signal;
  const history = method("findingInlineHistory", {
    http: async (path, options) => { requested = { path, options }; return { events: null }; },
    arr: (value) => value ?? [],
  });
  assert.deepEqual(await history("fixture/id", signal), []);
  assert.equal(requested.path, "/findings/fixture%2Fid/inline-history");
  assert.equal(requested.options.signal, signal);
  for (const name of ["revealCredential", "updateCredentialMetadata", "updateCredentialVerification", "skillsStatus"]) {
    assert.ok(apiNode.properties.some((node) => node.name?.getText(tree) === name), `${name} was dropped during merge`);
  }
});

test("verification labels stay independent from severity and blocked execution diagnosis remains neutral", () => {
  assert.deepEqual(SEVERITY_FILTER_OPTIONS.map(([value]) => value), ["all", "critical", "high", "medium", "low"]);
  assert.equal(statusMeta("verification", "confirmed").label, "已确认");
  assert.equal(statusMeta("verification", "unverified").label, "待验证");
  assert.equal(statusMeta("verification", "blocked").label, "验证受阻");
  assert.equal(statusMeta("intent", "blocked").label, "执行出错");
  assert.equal(statusMeta("intent", "unknown_failure").label, "unknown_failure");
  const statusSource = readFileSync(new URL("./status.ts", import.meta.url), "utf8");
  assert.doesNotMatch(statusSource, /基本没真正探成|重试用尽/);
});

test("settings contract retains callback metadata beside explicit worker pools and optional upstream switches", () => {
  const typesSource = readFileSync(new URL("./types.ts", import.meta.url), "utf8");
  const typesTree = ts.createSourceFile("types.ts", typesSource, ts.ScriptTarget.Latest, true, ts.ScriptKind.TS);
  const settings = typesTree.statements.find((node) => ts.isInterfaceDeclaration(node) && node.name.text === "Settings");
  const names = settings.members.map((node) => node.name?.getText(typesTree));
  for (const name of ["callback_addr", "callback_addr_source", "callback_addr_locked", "callback_addr_error", "worker_pools", "workers", "show_egress_ip", "opsec_enabled", "finding_inline_merge_enabled"]) {
    assert.ok(names.includes(name), `Settings.${name} was dropped`);
  }
});

test("side-question query contexts survive route construction without changing checkpoint parent fields", async () => {
  const text = readFileSync(new URL("./side-questions.ts", import.meta.url), "utf8");
  const sideTree = ts.createSourceFile("side-questions.ts", text, ts.ScriptTarget.Latest, true, ts.ScriptKind.TS);
  const helpers = sideTree.statements.filter(
    (node) => ts.isFunctionDeclaration(node) && ["request", "withQuery", "sidePath"].includes(node.name?.text),
  );
  const declaration = sideTree.statements
    .flatMap((node) => ts.isVariableStatement(node) ? [...node.declarationList.declarations] : [])
    .find((node) => node.name.getText(sideTree) === "sideAPI");
  const calls = [];
  const side = compile(
    `${helpers.map((node) => node.getText(sideTree)).join("\n")}\nconst sideAPI = ${declaration.initializer.getText(sideTree)};`,
    "sideAPI",
    { http: async (path, options) => { calls.push({ path, options }); return { items: null }; } },
  );
  const parent = "/api/conversations/12?context_task=20";
  const history = await side.history(parent, 7);
  assert.equal(calls[0].path, "/conversations/12/side-questions?context_task=20&before=7");
  assert.equal(history.items.length, 0);
  await side.ask(parent, "fixture question", "fixture-request");
  assert.equal(calls[1].path, "/conversations/12/side-questions?context_task=20");
  assert.equal(calls[1].options.method, "POST");
  assert.equal(calls[1].options.body, '{"question":"fixture question","client_request_id":"fixture-request"}');
  await side.clear(parent);
  assert.equal(calls[2].path, "/conversations/12/side-questions?context_task=20");
  assert.equal(calls[2].options.method, "DELETE");
});
