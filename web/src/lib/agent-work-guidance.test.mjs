import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import test from "node:test";

import { readAgentWorkGuidance } from "./agent-work-guidance.ts";

test("runtime guidance is absent for old responses and unconfigured custom roles", () => {
  for (const source of [undefined, null, {}, { work_guidance_version: "v1" }, { work_guidance: "" }, { work_guidance: " \n " }]) {
    assert.equal(readAgentWorkGuidance(source), undefined);
  }
});

test("guidance preserves the exact runtime text without mutating saved prompt content", () => {
  const source = Object.freeze({
    prompt: "USER CUSTOM BODY {{.Now}}",
    work_guidance_version: " 2026-10-29.v1 ",
    work_guidance: "\n运行时质量规范\n- 只记录真实结果。\n",
  });
  assert.deepEqual(readAgentWorkGuidance(source), {
    version: "2026-10-29.v1",
    text: "\n运行时质量规范\n- 只记录真实结果。\n",
  });
  assert.equal(source.prompt, "USER CUSTOM BODY {{.Now}}");
  assert.equal(source.work_guidance_version, " 2026-10-29.v1 ");
  assert.deepEqual(readAgentWorkGuidance({ work_guidance: "quality text" }), { version: "", text: "quality text" });
});

test("agent editor displays guidance separately and saves only the editable body", () => {
  const editor = readFileSync(new URL("../components/agent-editor.tsx", import.meta.url), "utf8");
  assert.match(editor, /<details[^>]*data-testid="agent-work-guidance"/);
  assert.match(editor, /guidance\.text/);
  assert.match(editor, /saveAgentPrompt\(agentKey, prompt\)/);
  assert.match(editor, /setPrompt\(d\.prompt \?\? ""\)/);
  assert.doesNotMatch(editor, /setPrompt\([^\n]*(?:work_guidance|guidance\.text)/);
  assert.match(editor, /readAgentWorkGuidance\(r\)/);
  assert.match(editor, /generation !== previewGeneration\.current/);
  assert.match(editor, /<AgentEditorInner key=\{agentKey\} agentKey=\{agentKey\}/);
  assert.match(editor, /平台在运行时独立追加/);
  assert.match(editor, /不变更工具权限、范围与审批/);
  assert.match(editor, /正文预览/);
  assert.match(editor, /完整运行时还会加入工具和任务等上下文/);
});
