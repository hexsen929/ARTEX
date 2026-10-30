"use client";

import * as React from "react";
import Link from "next/link";

import { BrainIcon, RefreshCwIcon } from "lucide-react";

import { Button } from "@/components/ui/button";
import { Dialog, DialogContent, DialogDescription, DialogHeader, DialogTitle } from "@/components/ui/dialog";
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from "@/components/ui/table";
import { memoryAPI } from "@/lib/memory-api";
import { memoryTime, type MemoryUsage, type MemoryUsageQuery } from "@/lib/memory-types";

const RESULT_LABELS: Record<string, string> = {
  recalled: "已召回",
  injected: "已注入",
  used: "已使用",
  read: "按需读取",
  admitted: "已准入",
  index_only: "仅短索引（非完整证据）",
  stale: "已失效，未使用",
  excluded: "已排除",
  source_invalid: "来源失效",
};

export function MemoryUsagePanel({ query = {} }: { query?: MemoryUsageQuery }) {
  const [items, setItems] = React.useState<MemoryUsage[]>([]);
  const [loading, setLoading] = React.useState(true);
  const [error, setError] = React.useState("");
  const [refresh, setRefresh] = React.useState(0);
  const { task_id, conversation_id, session_id, limit = 50 } = query;

  React.useEffect(() => {
    const controller = new AbortController();
    setLoading(true);
    setError("");
    void memoryAPI.usage({ task_id, conversation_id, session_id, limit }, controller.signal).then((result) => {
      if (!controller.signal.aborted) setItems(result);
    }).catch((cause) => {
      if (!controller.signal.aborted) setError(`使用记录读取失败：${(cause as Error).message}`);
    }).finally(() => {
      if (!controller.signal.aborted) setLoading(false);
    });
    return () => controller.abort();
  }, [task_id, conversation_id, session_id, limit, refresh]);

  return (
    <div className="min-w-0 space-y-3">
      <div className="flex flex-wrap items-center justify-between gap-2">
        <p className="text-muted-foreground text-xs">仅显示记忆 ID、版本和使用结果，不在审计中复制正文。最多显示最近 {limit} 条。</p>
        <Button type="button" variant="outline" size="sm" disabled={loading} onClick={() => setRefresh((n) => n + 1)}><RefreshCwIcon className="size-3.5" />刷新使用记录</Button>
      </div>
      {loading ? <p role="status" className="text-muted-foreground p-4 text-sm">正在读取使用记录…</p> : error ? (
        <p role="alert" className="text-destructive break-words p-4 text-sm">{error}</p>
      ) : items.length === 0 ? <p className="text-muted-foreground p-4 text-sm">暂无记忆使用记录；这不代表任务已验证过所有历史结论。</p> : (
        <div className="max-h-[60vh] min-w-0 overflow-auto rounded-md border">
          <Table>
            <TableHeader><TableRow><TableHead>记忆与版本</TableHead><TableHead>使用会话</TableHead><TableHead>Agent</TableHead><TableHead>结果</TableHead><TableHead>时间</TableHead></TableRow></TableHeader>
            <TableBody>{items.map((item) => (
              <TableRow key={item.id}>
                <TableCell><Link href={`/function/memories?entry_id=${item.entry_id}`} className="underline underline-offset-4">#{item.entry_id}</Link><span className="text-muted-foreground ml-1 text-xs">v{item.entry_version}</span></TableCell>
                <TableCell className="max-w-48 break-all whitespace-normal text-xs">{item.task_id > 0 ? <Link className="underline" href={`/function/tasks/detail?id=${item.task_id}`}>任务 #{item.task_id}</Link> : item.conversation_id && item.conversation_id > 0 ? <Link className="underline" href={`/chat?c=${item.conversation_id}`}>对话 #{item.conversation_id}</Link> : "独立会话"}<span className="text-muted-foreground block">{item.session_id || "—"}</span></TableCell>
                <TableCell>{item.agent_key || "—"}</TableCell>
                <TableCell>{RESULT_LABELS[item.result] ?? item.result}</TableCell>
                <TableCell className="whitespace-nowrap text-xs">{memoryTime(item.created_at)}</TableCell>
              </TableRow>
            ))}</TableBody>
          </Table>
        </div>
      )}
    </div>
  );
}

export function MemoryUsageButton({
  taskId,
  conversationId,
  sessionId,
}: {
  taskId?: number | string;
  conversationId?: number | string;
  sessionId?: string;
}) {
  const [open, setOpen] = React.useState(false);
  const task = Number(taskId);
  const conversation = Number(conversationId);
  const identified = (Number.isSafeInteger(task) && task > 0) || (Number.isSafeInteger(conversation) && conversation > 0);
  const query: MemoryUsageQuery = {
    ...(Number.isSafeInteger(task) && task > 0 ? { task_id: task } : {}),
    ...(Number.isSafeInteger(conversation) && conversation > 0 ? { conversation_id: conversation } : {}),
    ...(sessionId ? { session_id: sessionId } : {}),
    limit: 50,
  };
  const title = sessionId || conversationId ? "本会话记忆使用记录" : "本任务记忆使用记录";
  return (
    <Dialog open={open} onOpenChange={setOpen}>
      <Button type="button" size="icon-sm" variant="ghost" title={title} aria-label={title} disabled={!identified} onClick={() => setOpen(true)}><BrainIcon className="size-4" /></Button>
      <DialogContent className="sm:max-w-4xl">
        <DialogHeader><DialogTitle>{title}</DialogTitle><DialogDescription>历史使用记录不等于当前仍在使用。停用或删除记忆会阻止后续召回，历史业务记录仍按原生命周期保留。</DialogDescription></DialogHeader>
        {open ? <MemoryUsagePanel query={query} /> : null}
      </DialogContent>
    </Dialog>
  );
}
