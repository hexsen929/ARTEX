import type { TaskNode } from "@/lib/types";

export function ExecutionCheckpoint({ node }: { node: TaskNode }) {
  const checkpoint = node.execution_checkpoint;
  if (!checkpoint) return null;
  const pending = Object.keys(checkpoint.pending ?? {}).length;
  return (
    <p className="whitespace-normal text-xs text-muted-foreground">
      原意图 #{node.id} · 第 {node.execution_attempt ?? 1} 轮 · 工具返回 {checkpoint.tool_results}/{checkpoint.tool_calls}
      {checkpoint.last_tool ? ` · 最近：${checkpoint.last_tool}` : " · 尚无工具调用"}
      {pending > 0 ? ` · ${pending} 项结果待核对` : ""}
      {checkpoint.incomplete ? " · 执行记录不完整" : ""}
    </p>
  );
}
