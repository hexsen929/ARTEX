"use client";

import * as React from "react";

import { ActivityIcon, RefreshCwIcon } from "lucide-react";

import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from "@/components/ui/card";
import { memoryAPI } from "@/lib/memory-api";
import { MEMORY_LEARNING_LABELS, MEMORY_LEARNING_RESULT_LABELS, memoryLearningOutcome, memoryLearningOutcomeIsError, memoryTime, type MemoryLearningResult, type MemoryLearningSnapshot } from "@/lib/memory-types";

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
            <Badge variant="outline">未完成总结记录 {snapshot.failed}</Badge>
            {snapshot.retryable_failed !== undefined ? <Badge variant="outline">待有限重试 {snapshot.retryable_failed}</Badge> : null}
            {snapshot.stopped_failed !== undefined ? <Badge variant="outline">已终止或不再重试 {snapshot.stopped_failed}</Badge> : null}
            {snapshot.entry_count !== undefined ? <Badge variant="outline">未删除记忆条目 {snapshot.entry_count}</Badge> : null}
          </div>
          <p className="text-muted-foreground text-xs">已处理及未完成计数是保留中的总结作业记录，不等于当前记忆条目数、API 错误次数或渗透任务成功率；后续重试成功和保留清理会改变计数。未删除条目包含停用、失效及到期记录，有效状态以列表为准。</p>
          {snapshot.retryable_failed === undefined || snapshot.stopped_failed === undefined || snapshot.entry_count === undefined ? <p className="text-muted-foreground text-xs">服务未提供完整的重试分类或当前条目计数；缺失数据未显示为零。</p> : null}
          <div className="space-y-2" aria-label="阶段结果分类">
            <p className="font-medium text-xs">阶段结果分类（保留记录）</p>
            {snapshot.outcome_counts === undefined ? <p className="text-muted-foreground text-xs">服务未提供阶段结果分类；缺失分类未显示为零。</p> : (
              <div className="flex flex-wrap gap-2 text-xs">
                {Object.entries(snapshot.outcome_counts).filter(([, count]) => (count ?? 0) > 0).map(([result, count]) => <Badge key={result} variant="outline">{MEMORY_LEARNING_RESULT_LABELS[result as MemoryLearningResult]} {count}</Badge>)}
                {Object.values(snapshot.outcome_counts).every((count) => count === 0) ? <span className="text-muted-foreground">当前没有已分类的阶段结果。</span> : null}
              </div>
            )}
          </div>
          <p className="text-muted-foreground text-xs">滚动近一小时：领取阶段 {snapshot.starts_last_hour} / {snapshot.max_calls_per_hour}；实际 HTTP 尝试 {snapshot.attempts_last_hour} / {snapshot.max_calls_per_hour}。任一计数达到上限时等待预算恢复；不是整点重置，有限重试也计入实际请求尝试。</p>
          {snapshot.next_budget_at ? <p className="text-muted-foreground text-xs">最早释放一个预算名额：{memoryTime(snapshot.next_budget_at)}。是否开始调用仍取决于队列、模型和来源有效性。</p> : snapshot.status === "budget_wait" ? <p className="text-muted-foreground text-xs">服务未提供预算恢复时间；未推测整点恢复或承诺届时自动调用。</p> : null}
          <p className={memoryLearningOutcomeIsError(snapshot.last_result) ? "text-destructive text-sm" : "text-muted-foreground text-sm"}>{memoryLearningOutcome(snapshot.last_result)}</p>
          {snapshot.last_completed_at ? <p className="text-muted-foreground text-xs">最近阶段处理时间：{memoryTime(snapshot.last_completed_at)}</p> : null}
        </>
      ) : !loading && !error ? <p className="text-muted-foreground text-sm">页面可见时读取；等待阶段结束与状态读取失败会分别显示。</p> : null}
      {confirmedAt ? <p className="text-muted-foreground text-xs">最近确认：{memoryTime(confirmedAt)}；仅在页面可见时每 30 秒刷新。</p> : null}
    </div>
  );

  if (embedded) return <section aria-label="阶段经验学习状态" className="rounded-md border p-3">{content}</section>;
  return <Card><CardHeader><CardTitle className="flex items-center gap-2 text-base"><ActivityIcon className="size-4" />阶段经验学习状态</CardTitle><CardDescription>阶段结束并稳定后才做有界总结，不逐条提炼事实或普通聊天。模型配置可用不代表已经产生经验。</CardDescription></CardHeader><CardContent>{content}</CardContent></Card>;
}
