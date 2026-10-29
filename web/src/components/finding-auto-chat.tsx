"use client";

import * as React from "react";

import { SideQuestionWorkspace } from "@/components/side-question-workspace";
import { Alert, AlertDescription } from "@/components/ui/alert";
import { Button } from "@/components/ui/button";
import { useSideQuestions } from "@/hooks/use-side-questions";

// Finding analysis is task-bound, tool-free side Q&A, not a new unbound Auto
// execution conversation. History belongs to the task and is shared with its
// existing side Q&A, not isolated per finding. The server validates the actual
// task/finding relationship; a textual mention is not an authorization grant.
export function FindingAutoChat({
  taskId,
  findingId,
  findingTitle,
  open,
  onOpenChange,
}: {
  taskId?: string;
  findingId: string;
  findingTitle: string;
  open: boolean;
  onOpenChange: (open: boolean) => void;
}) {
  const validTask = !!taskId && /^\d+$/.test(taskId) && Number.isSafeInteger(Number(taskId)) && Number(taskId) > 0;
  const validFinding = /^\d+$/.test(findingId) && Number.isSafeInteger(Number(findingId)) && Number(findingId) > 0;
  const parent = open && validTask && validFinding
    ? `/api/tasks/${encodeURIComponent(taskId ?? "")}/chat?agent=auto&finding_id=${encodeURIComponent(findingId)}`
    : null;
  const side = useSideQuestions(parent);
  const openValue = React.useRef(open);
  openValue.current = open;
  const setWorkspaceOpen = React.useCallback((update: React.SetStateAction<boolean>) => {
    // The shared workspace accepts a React Dispatch, including functional
    // updates. Resolve against the latest controlled value, then synchronize
    // both state owners without putting external callbacks in a state updater.
    const next = typeof update === "function" ? update(openValue.current) : update;
    openValue.current = next;
    side.setOpen(next && !!parent);
    onOpenChange(next);
  }, [onOpenChange, parent, side.setOpen]);
  React.useEffect(() => {
    side.setOpen(open && !!parent);
  }, [open, parent, side.setOpen]);
  const safeTitle = findingTitle.replace(/[\r\n]/g, " ").trim().slice(0, 100);

  if (!open) return null;
  if (!parent) {
    return (
      <div className="fixed right-4 top-20 z-40 max-w-md rounded-xl border bg-background p-4 shadow-xl" role="dialog" aria-label="发现只读分析不可用">
        <Alert>
          <AlertDescription>此发现缺少有效的任务关联，暂不提供任务旁路分析；不会退回无任务的可执行 Auto 会话。</AlertDescription>
        </Alert>
        <Button className="mt-3" variant="outline" onClick={() => onOpenChange(false)}>关闭</Button>
      </div>
    );
  }
  return (
    <SideQuestionWorkspace
      side={{ ...side, open, setOpen: setWorkspaceOpen }}
      mode="floating"
      label={`任务 #${taskId} · 共享任务旁路历史（清空影响共享问答）· Auto 只读分析 · 发现 #${findingId} · ${safeTitle}`}
      placeholder="仅分析已保存的发现与任务证据，不执行工具…"
      suggestions={["解释现有证据与结论", "哪些证据仍然缺失？", "整理修复建议"]}
    >
      <span className="sr-only">发现辅助分析：独立问答，无工具执行。</span>
    </SideQuestionWorkspace>
  );
}
