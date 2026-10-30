"use client";

import * as React from "react";

import { ActivityIcon, RefreshCwIcon } from "lucide-react";

import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from "@/components/ui/card";
import { memoryAPI } from "@/lib/memory-api";
import { MEMORY_LEARNING_LABELS, memoryLearningOutcome, memoryTime, type MemoryLearningSnapshot } from "@/lib/memory-types";

export function MemoryLearningStatus({ embedded = false }: { embedded?: boolean }) {
  const [snapshot, setSnapshot] = React.useState<MemoryLearningSnapshot | null>(null);
  const [loading, setLoading] = React.useState(true);
  const [error, setError] = React.useState("");
  const [confirmedAt, setConfirmedAt] = React.useState<string | null>(null);
  const [refresh, setRefresh] = React.useState(0);

  React.useEffect(() => {
    let live = true;
    let controller: AbortController | undefined;
    let timer: ReturnType<typeof setInterval> | undefined;

    const load = async () => {
      if (document.visibilityState !== "visible") return;
      controller?.abort();
      const request = new AbortController();
      controller = request;
      setLoading(true);
      setError("");
      try {
        const result = await memoryAPI.learning(request.signal);
        if (!live || request.signal.aborted) return;
        setSnapshot(result);
        setConfirmedAt(new Date().toISOString());
      } catch (cause) {
        if (live && !request.signal.aborted) setError(`学习状态读取失败：${(cause as Error).message}`);
      } finally {
        if (live && !request.signal.aborted) setLoading(false);
      }
    };

    const visibility = () => {
      if (timer) clearInterval(timer);
      timer = undefined;
      if (document.visibilityState === "visible") {
        void load();
        timer = setInterval(() => void load(), 30_000);
      } else {
        controller?.abort();
        setLoading(false);
      }
    };

    document.addEventListener("visibilitychange", visibility);
    visibility();
    return () => {
      live = false;
      controller?.abort();
      if (timer) clearInterval(timer);
      document.removeEventListener("visibilitychange", visibility);
    };
  }, [refresh]);

  const content = (
    <div className="space-y-3">
      <div className="flex flex-wrap items-center justify-between gap-2">
        <p className="font-medium text-sm">{snapshot ? MEMORY_LEARNING_LABELS[snapshot.status] : loading ? "正在读取学习状态…" : "当前学习状态未确认"}</p>
        <Button type="button" size="sm" variant="outline" disabled={loading} onClick={() => setRefresh((value) => value + 1)}><RefreshCwIcon className="size-3.5" />刷新学习状态</Button>
      </div>
      {error ? <p role="alert" className="text-destructive break-words text-sm">{error}{snapshot ? "。下方仅为上次确认的状态，不代表当前进度。" : "。未将未知进度显示为零或学习完成。"}</p> : null}
      {snapshot ? (
        <>
          <p role="status" className="text-muted-foreground whitespace-pre-wrap break-words text-sm">{snapshot.message}</p>
          <div className="flex flex-wrap gap-2 text-xs">
            <Badge variant="outline">待总结阶段 {snapshot.queued}</Badge>
            <Badge variant="outline">总结中 {snapshot.running}</Badge>
            <Badge variant="outline">已处理阶段 {snapshot.processed}</Badge>
            <Badge variant="outline">失败阶段 {snapshot.failed}</Badge>
          </div>
          <p className="text-muted-foreground text-xs">近一小时：领取阶段 {snapshot.starts_last_hour} / {snapshot.max_calls_per_hour}；实际 HTTP 尝试 {snapshot.attempts_last_hour} / {snapshot.max_calls_per_hour}。任一计数达到上限时，待处理阶段等待下一预算窗口；有限重试也计入实际请求尝试。</p>
          <p className={snapshot.last_result === "failed" ? "text-destructive text-sm" : "text-muted-foreground text-sm"}>{memoryLearningOutcome(snapshot.last_result)}</p>
          {snapshot.last_completed_at ? <p className="text-muted-foreground text-xs">最近阶段处理时间：{memoryTime(snapshot.last_completed_at)}</p> : null}
        </>
      ) : !loading && !error ? <p className="text-muted-foreground text-sm">页面可见时读取；等待阶段结束与状态读取失败会分别显示。</p> : null}
      {confirmedAt ? <p className="text-muted-foreground text-xs">最近确认：{memoryTime(confirmedAt)}；仅在页面可见时每 30 秒刷新。</p> : null}
    </div>
  );

  if (embedded) return <section aria-label="阶段经验学习状态" className="rounded-md border p-3">{content}</section>;
  return <Card><CardHeader><CardTitle className="flex items-center gap-2 text-base"><ActivityIcon className="size-4" />阶段经验学习状态</CardTitle><CardDescription>阶段结束并稳定后才做有界总结，不逐条提炼事实或普通聊天。模型配置可用不代表已经产生经验。</CardDescription></CardHeader><CardContent>{content}</CardContent></Card>;
}
