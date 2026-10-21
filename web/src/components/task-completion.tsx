"use client";

import { useState } from "react";
import { toast } from "sonner";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { api } from "@/lib/api";
import type { Task } from "@/lib/types";

export function TaskCompletionBadge({ task }: { task: Task }) {
  const phase = task.completion?.phase;
  if (task.paused || task.status !== "running") return null;
  if (phase === "interrupted" && task.completion?.failure_kind)
    return <Badge variant="outline">{task.completion.next_retry_at ? "等待复核重试" : "复核失败待恢复"}</Badge>;
  if (phase !== "draining" && phase !== "reviewing") return null;
  return <Badge variant="outline">{phase === "draining" ? "保存收尾结果" : "最终复核"}</Badge>;
}

export function TaskCompletionPanel({ task }: { task: Task }) {
  const [retrying, setRetrying] = useState(false);
  const completion = task.completion;
  if (!completion || completion.phase === "idle") return null;
  const active =
    task.status === "running" && !task.paused && (completion.phase === "draining" || completion.phase === "reviewing");
  let title = "最近一次收官记录";
  if (active) title = completion.phase === "draining" ? "正在保存 Worker 已有结果" : "正在最终复核";
  const failed = task.status === "running" && completion.phase === "interrupted" && !!completion.failure_kind;
  if (failed)
    title = completion.next_retry_at
      ? "等待复核重试"
      : completion.failure_kind === "evidence"
        ? "证据不足，必要工作仍需继续"
        : "复核失败待恢复";
  const actions: Record<string, string> = {
    retry: "等待自动重试；仍可暂停任务。",
    manual_retry: "自动重试已停止。检查失败原因，修复后重新复核。",
    repair_evidence: "补充证据或纠正目标、剩余检查的判定，然后重新复核。",
    repair_history: "修复推理历史或调整模型配置，然后重新复核。",
    configure_model: "检查模型及复核预算配置，然后重新复核。",
    wait_for_exit: "检查 Worker 中断原因，确认已保存结果后重新复核。",
  };
  const retry = async () => {
    setRetrying(true);
    try {
      await api.retryTaskCompletion(task.id, completion.attempt_id, "用户确认已处理复核失败原因，申请重新复核");
      toast.success("已申请重新复核");
    } catch (error) {
      toast.error(error instanceof Error ? error.message : "重新复核失败");
    } finally {
      setRetrying(false);
    }
  };
  return (
    <details className="rounded-md border bg-muted/30 px-3 py-2 text-sm" open={active || failed}>
      <summary className="cursor-pointer font-medium">
        {title}
        <span className="ml-2 font-normal text-muted-foreground">#{completion.attempt_id}</span>
      </summary>
      <div className="mt-2 space-y-2">
        <p className="whitespace-pre-wrap break-words">{completion.reason}</p>
        {active && (
          <p className="text-muted-foreground text-xs">
            本阶段不再发起新探测。{completion.phase === "draining" ? "保存收尾期限" : "复核期限"}：
            {completion.phase === "reviewing"
              ? completion.review_deadline_at
                ? new Date(completion.review_deadline_at).toLocaleString()
                : "等待确认"
              : new Date(completion.deadline_at).toLocaleString()}
            ；仍可暂停任务。
          </p>
        )}
        {failed && (
          <div className="space-y-2">
            <p>
              本类复核已失败 {completion.failure_count ?? 0} 次。
              {actions[completion.recovery_action ?? ""] ?? "检查失败原因后恢复。"}
            </p>
            {completion.next_retry_at && (
              <p className="text-muted-foreground text-xs">
                下次复核：{new Date(completion.next_retry_at).toLocaleString()}；暂停期间不会自动重试。
              </p>
            )}
            {!task.paused && (
              <Button variant="outline" size="sm" disabled={retrying} onClick={retry}>
                {retrying ? "正在申请…" : "修复后重新复核"}
              </Button>
            )}
          </div>
        )}
        {!!completion.recovery_history?.length && (
          <ul className="list-inside list-disc text-muted-foreground text-xs">
            {completion.recovery_history.map((entry, index) => (
              <li key={`${entry.at}-${index}`}>
                {new Date(entry.at).toLocaleString()}：{entry.reason}
              </li>
            ))}
          </ul>
        )}
        {completion.review && <p className="whitespace-pre-wrap break-words">{completion.review}</p>}
        {!!completion.goal_gaps?.length && (
          <div className="space-y-1">
            <p className="font-medium">目标证据缺口</p>
            <p className="text-muted-foreground text-xs">在目标管理中核对原证明，纠正完成判定并继续必要工作。</p>
            <ul className="list-inside list-disc space-y-1">
              {completion.goal_gaps.map((gap) => (
                <li key={`${gap.goal_id}-${gap.expected_version}`} className="whitespace-pre-wrap break-words">
                  目标 #{gap.goal_id}：{gap.reason}
                </li>
              ))}
            </ul>
          </div>
        )}
        {!!completion.remaining_work?.length && (
          <>
            <p className="font-medium">收官时剩余检查及处置依据</p>
            <p className="text-muted-foreground text-xs">停止或中断不代表检查已验证通过。</p>
            <ul className="list-inside list-disc space-y-1">
              {completion.remaining_work.map((work) => (
                <li key={work.intent_id} className="break-words">
                  #{work.intent_id} {work.summary}：{work.reason || "未形成完成结论"}
                </li>
              ))}
            </ul>
          </>
        )}
      </div>
    </details>
  );
}
