"use client";

import { useState } from "react";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { api } from "@/lib/api";
import type { TaskGoal } from "@/lib/types";

export function GoalCorrection({
  taskId,
  goal,
  onChanged,
}: {
  taskId: string;
  goal: TaskGoal;
  onChanged: () => Promise<void>;
}) {
  const [editing, setEditing] = useState(false);
  const [reason, setReason] = useState("");
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");
  const reopen = async () => {
    setBusy(true);
    setError("");
    try {
      await api.reopenGoal(taskId, goal.id, goal.content_version ?? 0, reason.trim());
      setReason("");
      setEditing(false);
      await onChanged();
    } catch (e) {
      setError(e instanceof Error ? e.message : "目标纠错失败");
    } finally {
      setBusy(false);
    }
  };
  const labels: Record<string, string> = {
    prove: "证据审核通过",
    reject: "证据不足",
    reopen: "原判定已纠正",
    edit: "目标已修改",
  };
  return (
    <div className="space-y-1">
      {!!goal.audit?.length && (
        <details className="text-xs text-muted-foreground">
          <summary className="cursor-pointer">证据与纠错记录（{goal.audit.length}）</summary>
          <ul className="max-w-xl space-y-1 whitespace-pre-wrap break-words">
            {goal.audit.map((entry, index) => (
              <li key={`${entry.at}-${index}`}>
                {new Date(entry.at).toLocaleString()} · {labels[entry.event] ?? entry.event} · {entry.actor}：
                {entry.reason}
                {entry.evidence_id ? `（证据 #${entry.evidence_id}）` : ""}
              </li>
            ))}
          </ul>
        </details>
      )}
      {goal.state === "met" && !editing && (
        <Button variant="ghost" size="sm" onClick={() => setEditing(true)}>
          纠正完成判定
        </Button>
      )}
      {editing && (
        <div className="flex flex-wrap items-center gap-1">
          <Input
            className="h-8 min-w-48"
            value={reason}
            onChange={(e) => setReason(e.target.value)}
            placeholder="说明原证据缺少什么成果或必要检查"
            disabled={busy}
          />
          <Button size="sm" disabled={busy || !reason.trim()} onClick={() => void reopen()}>
            {busy ? "正在保存…" : "恢复为待完成"}
          </Button>
          <Button variant="ghost" size="sm" disabled={busy} onClick={() => setEditing(false)}>
            取消
          </Button>
        </div>
      )}
      {error && <p className="text-xs text-destructive">{error}</p>}
    </div>
  );
}
