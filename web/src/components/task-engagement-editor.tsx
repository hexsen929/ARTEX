"use client";

import * as React from "react";
import { toast } from "sonner";
import { api } from "@/lib/api";
import type { Task, TaskScopeRow } from "@/lib/types";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { Textarea } from "@/components/ui/textarea";
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
  DialogTrigger,
} from "@/components/ui/dialog";

type GoalDraft = { text: string; goal_type: "" | "inspection" | "outcome"; vulnclass?: string; checks: string };
type ConstraintDraft = { kind: "allow" | "deny"; text: string };
type ScopeDraft = { kind: string; value: string };

function scopeValue(scope: TaskScopeRow): string {
  if (scope.kind === "company") return String(scope.company_id ?? "");
  return scope.value ?? scope.domain ?? scope.net ?? "";
}

export function TaskEngagementEditor({ task, onUpdated }: { task: Task; onUpdated: () => void }) {
  const [open, setOpen] = React.useState(false);
  const [busy, setBusy] = React.useState(false);
  const [ready, setReady] = React.useState(false);
  const [goal, setGoal] = React.useState(task.goal);
  const [description, setDescription] = React.useState(task.description);
  const [reason, setReason] = React.useState("");
  const [goals, setGoals] = React.useState<GoalDraft[]>([]);
  const [constraints, setConstraints] = React.useState<ConstraintDraft[]>([]);
  const [scopes, setScopes] = React.useState<ScopeDraft[]>([]);
  const canEdit = task.paused && !["done", "failed", "timeout"].includes(task.status);

  React.useEffect(() => {
    if (!open) return;
    let active = true;
    setReady(false);
    setGoal(task.goal);
    setDescription(task.description);
    setReason("");
    Promise.all([api.taskGoals(task.id), api.taskConstraints(task.id), api.taskScope(task.id)])
      .then(([g, c, s]) => {
        if (!active) return;
        setGoals(
          g.goals.map((row) => ({
            text: row.text,
            goal_type: row.goal_type ?? "",
            vulnclass: row.vulnclass,
            checks: (row.required_checks ?? []).join("\n"),
          })),
        );
        setConstraints(c.constraints.map((row) => ({ kind: row.kind, text: row.text })));
        setScopes(
          (s.scope ?? [])
            .filter((row) => String(row.task_id) === task.id)
            .map((row) => ({ kind: row.kind, value: scopeValue(row) })),
        );
        setReady(true);
      })
      .catch((error) => {
        if (active) toast.error(error instanceof Error ? error.message : "读取任务范围失败");
      });
    return () => {
      active = false;
    };
  }, [open, task.id, task.goal, task.description]);

  async function save() {
    if (
      !goal.trim() ||
      !reason.trim() ||
      !goals.length ||
      goals.some((row) => !row.text.trim() || !row.goal_type || (row.goal_type === "inspection" && !row.checks.trim()))
    ) {
      toast.error("请填写总目标、变更原因、目标类型和必要检查");
      return;
    }
    setBusy(true);
    try {
      const result = await api.updateTaskEngagement(task.id, {
        goal,
        description,
        reason,
        goals: goals.map(({ checks, ...row }) => ({
          ...row,
          goal_type: row.goal_type as "inspection" | "outcome",
          required_checks: checks
            .split("\n")
            .map((text) => text.trim())
            .filter(Boolean),
        })),
        constraints,
        scopes,
      });
      toast.success(result.note);
      setOpen(false);
      onUpdated();
    } catch (error) {
      toast.error(error instanceof Error ? error.message : "保存失败");
    } finally {
      setBusy(false);
    }
  }

  return (
    <Dialog
      open={open}
      onOpenChange={(value) => {
        if (!busy) setOpen(value);
      }}
    >
      <DialogTrigger asChild>
        <Button
          size="sm"
          variant="outline"
          disabled={!canEdit}
          title={canEdit ? "同步修改目标、约束和范围" : "暂停任务并等待工作退出后可修改"}
        >
          调整目标与范围
        </Button>
      </DialogTrigger>
      <DialogContent className="max-h-[85vh] overflow-y-auto sm:max-w-3xl">
        <DialogHeader>
          <DialogTitle>调整目标与范围</DialogTitle>
          <DialogDescription>
            目标、约束和资产范围会一起保存。原工作将停止，旧结论保留记录，新目标重新复核；保存后任务保持暂停。
          </DialogDescription>
        </DialogHeader>
        {!ready ? (
          <p className="text-sm text-muted-foreground">正在读取目标与范围…</p>
        ) : (
          <div className="space-y-4">
            <div className="space-y-2">
              <Label htmlFor="engagement-description">任务说明</Label>
              <Textarea
                id="engagement-description"
                value={description}
                onChange={(e) => setDescription(e.target.value)}
              />
            </div>
            <div className="space-y-2">
              <Label htmlFor="engagement-goal">总目标</Label>
              <Textarea id="engagement-goal" value={goal} onChange={(e) => setGoal(e.target.value)} />
            </div>
            <div className="space-y-2">
              <Label>检查目标</Label>
              <p className="text-sm text-muted-foreground">
                检查目标可用实际范围内的阴性结果完成；成果目标需要取得对应成果。修改端口时，同时更新目标和必要检查。
              </p>
              {goals.map((row, i) => (
                <div key={i} className="space-y-2 rounded border p-3">
                  <div className="flex gap-2">
                    <select
                      aria-label={`目标 ${i + 1} 类型`}
                      className="rounded border bg-background p-2 text-sm"
                      value={row.goal_type}
                      onChange={(e) =>
                        setGoals(
                          goals.map((g, n) =>
                            n === i ? { ...g, goal_type: e.target.value as GoalDraft["goal_type"] } : g,
                          ),
                        )
                      }
                    >
                      <option value="" disabled>
                        请选择类型
                      </option>
                      <option value="inspection">检查</option>
                      <option value="outcome">成果</option>
                    </select>
                    <Input
                      aria-label={`目标 ${i + 1}`}
                      value={row.text}
                      onChange={(e) => setGoals(goals.map((g, n) => (n === i ? { ...g, text: e.target.value } : g)))}
                    />
                    <Button variant="ghost" onClick={() => setGoals(goals.filter((_, n) => n !== i))}>
                      移除
                    </Button>
                  </div>
                  <Textarea
                    aria-label={`目标 ${i + 1} 必要检查`}
                    placeholder="必要检查，每行一项"
                    value={row.checks}
                    onChange={(e) => setGoals(goals.map((g, n) => (n === i ? { ...g, checks: e.target.value } : g)))}
                  />
                </div>
              ))}
              <Button
                size="sm"
                variant="outline"
                onClick={() => setGoals([...goals, { text: "", goal_type: "inspection", checks: "" }])}
              >
                新增目标
              </Button>
            </div>
            <div className="space-y-2">
              <Label>允许和禁止的操作</Label>
              {constraints.map((row, i) => (
                <div key={i} className="flex gap-2">
                  <select
                    aria-label={`约束 ${i + 1} 类型`}
                    className="rounded border bg-background p-2 text-sm"
                    value={row.kind}
                    onChange={(e) =>
                      setConstraints(
                        constraints.map((c, n) =>
                          n === i ? { ...c, kind: e.target.value as ConstraintDraft["kind"] } : c,
                        ),
                      )
                    }
                  >
                    <option value="allow">允许</option>
                    <option value="deny">禁止</option>
                  </select>
                  <Input
                    aria-label={`约束 ${i + 1}`}
                    value={row.text}
                    onChange={(e) =>
                      setConstraints(constraints.map((c, n) => (n === i ? { ...c, text: e.target.value } : c)))
                    }
                  />
                  <Button variant="ghost" onClick={() => setConstraints(constraints.filter((_, n) => n !== i))}>
                    移除
                  </Button>
                </div>
              ))}
              <Button
                size="sm"
                variant="outline"
                onClick={() => setConstraints([...constraints, { kind: "deny", text: "" }])}
              >
                新增约束
              </Button>
            </div>
            <div className="space-y-2">
              <Label>本任务资产范围</Label>
              {scopes.map((row, i) => (
                <div key={i} className="flex gap-2">
                  <select
                    aria-label={`范围 ${i + 1} 类型`}
                    className="rounded border bg-background p-2 text-sm"
                    value={row.kind}
                    onChange={(e) => setScopes(scopes.map((s, n) => (n === i ? { ...s, kind: e.target.value } : s)))}
                  >
                    {["ip", "cidr", "root_domain", "subdomain", "company", "icp", "keyword"].map((kind) => (
                      <option key={kind} value={kind}>
                        {kind}
                      </option>
                    ))}
                  </select>
                  <Input
                    aria-label={`范围 ${i + 1}`}
                    value={row.value}
                    onChange={(e) => setScopes(scopes.map((s, n) => (n === i ? { ...s, value: e.target.value } : s)))}
                  />
                  <Button variant="ghost" onClick={() => setScopes(scopes.filter((_, n) => n !== i))}>
                    移除
                  </Button>
                </div>
              ))}
              <Button size="sm" variant="outline" onClick={() => setScopes([...scopes, { kind: "ip", value: "" }])}>
                新增范围
              </Button>
            </div>
            <div className="space-y-2">
              <Label htmlFor="engagement-reason">变更原因</Label>
              <Input id="engagement-reason" value={reason} onChange={(e) => setReason(e.target.value)} />
            </div>
          </div>
        )}
        <DialogFooter>
          <Button variant="outline" disabled={busy} onClick={() => setOpen(false)}>
            取消
          </Button>
          <Button disabled={!ready || busy || !canEdit} onClick={save}>
            {busy ? "保存中…" : "同步保存"}
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  );
}
