import test from "node:test";
import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import { topologyMappingStatus, topologyMappingLabel } from "./topology-mapping.ts";

const source = (path) => readFileSync(new URL(path, import.meta.url), "utf8");

test("topology state keeps finished/failed/interrupted sessions without inventing success", () => {
  for (const status of ["running", "finished", "failed", "interrupted"]) {
    const state = topologyMappingStatus({ running: status === "running", status, conversation_id: 71, association: "native" });
    assert.equal(state.conversation_id, 71);
    assert.equal(state.status, status);
    assert.doesNotMatch(topologyMappingLabel(status), /已成功|成功完成/);
  }
  assert.deepEqual(topologyMappingStatus({ running: false, status: "none" }), { running: false, status: "none" });
  assert.equal(topologyMappingStatus({ running: false, status: "finished", conversation_id: 71, association: "legacy_title" }).association, "legacy_title");
  for (const malformed of [null, {}, { running: false }, { running: false, status: "none", conversation_id: 71 }, { running: false, status: "completed", conversation_id: 71 }, { running: true, status: "failed", conversation_id: 71 }, { running: false, status: "finished" }, { running: false, status: "finished", conversation_id: 71, finished_at: "not-a-time" }]) {
    assert.throws(() => topologyMappingStatus(malformed));
  }
});

test("topology UI retains native conversation link and guards task-scoped asynchronous results", () => {
  const ui = source("../app/(main)/function/tasks/detail/_tabs/coverage-graph-tab.tsx");
  assert.match(ui, /href=\{`\/chat\?c=\$\{mappingState\.conversation_id\}`\}/);
  assert.match(ui, /if \(!mappingState\.running\) return/);
  assert.match(ui, /selectedTask !== mappingTaskRef\.current \|\| sequence !== mappingSequence\.current/);
  assert.match(ui, /拓扑梳理状态不可用/);
  assert.match(ui, /历史标题候选，任务归属未核实/);
  assert.match(ui, /mappingState\.association === "native" && mappingState\.running/);
  assert.doesNotMatch(ui, /setNetworkReportAt\(new Date\(\)\)/);
  const server = source("../../../server/task_asset_mapping.go");
  assert.match(server, /topology_mapping_task/);
  assert.match(server, /resolveChatAgent\(mappingConversation\)/);
  assert.match(server, /chatUnavailableReason\(mappingConversation\)/);
  assert.doesNotMatch(server, /s\.chatAgentRef\(\) == nil/);
  assert.match(server, /NOT EXISTS\(SELECT 1 FROM conversation_activities a WHERE a\.conversation_id=c\.id AND a\.kind=\$1\)/);
});

test("LLM recording unknown state stays disabled and request generations prevent stale payloads", () => {
  const ui = source("../app/(main)/function/llm-records/page.tsx");
  assert.match(ui, /useState<boolean \| null>\(null\)/);
  assert.match(ui, /disabled=\{recBusy \|\| recLoading \|\| !!recError \|\| recEnabled === null\}/);
  assert.match(ui, /typeof settings\.llm_record !== "boolean"/);
  assert.match(ui, /sequence === recordingSequence\.current/);
  assert.match(ui, /requestedKey !== queryKeyRef\.current/);
  assert.match(ui, /detail\?\.id === selected\?\.id \? detail : null/);
  assert.match(ui, /selectedId\.current === id/);
  assert.match(ui, /当前筛选结果未知，不表示没有记录/);
  assert.match(ui, /详情读取失败/);
  assert.match(ui, /删除失败/);
  assert.doesNotMatch(ui, /\.catch\(\(\) => \{\}\)/);
  assert.doesNotMatch(ui, /setRecEnabled\(!on\)/);
});

test("conversation usage errors are not converted to empty success and dashboard preserves prior values", () => {
  const api = source("./api.ts");
  const method = api.slice(api.indexOf("conversationTokens: () =>"), api.indexOf("explorationGraph:", api.indexOf("conversationTokens: () =>")));
  assert.doesNotMatch(method, /\.catch/);
  assert.match(method, /!Array\.isArray\(items\)/);
  const ui = source("../app/(main)/dashboard/page.tsx");
  assert.match(ui, /useState<ConvTokenSummary\[\] \| null>\(null\)/);
  assert.match(ui, /setConvTokenError\(\(error as Error\)\.message\)/);
  assert.match(ui, /当前用量未知/);
  assert.match(ui, /保留上次成功数据/);
  assert.match(ui, /tokenDisplayAvailable \? fmtTokens/);
  assert.match(ui, /Poll it independently/);
});
