"use client";

import * as React from "react";

import { ChevronDownIcon, HistoryIcon } from "lucide-react";

import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Card } from "@/components/ui/card";
import { api } from "@/lib/api";
import { inlineHistoryOrder, inlineReportPayload, inlineReportText } from "@/lib/finding-inline-history";
import type { FindingInlineMergeEvent } from "@/lib/types";

const PAGE_SIZE = 20;

function historyTime(value: string): string {
  const time = new Date(value);
  return Number.isNaN(time.getTime()) ? "时间未记录" : time.toLocaleString("zh-CN");
}

export function FindingInlineHistory({ findingId }: { findingId: string }) {
  const [events, setEvents] = React.useState<FindingInlineMergeEvent[]>([]);
  const [expanded, setExpanded] = React.useState(false);
  const [visible, setVisible] = React.useState(PAGE_SIZE);

  React.useEffect(() => {
    let active = true;
    const abort = new AbortController();
    setEvents([]);
    setExpanded(false);
    setVisible(PAGE_SIZE);
    if (findingId) {
      void api.findingInlineHistory(findingId, abort.signal)
        .then((items) => {
          if (active) setEvents(inlineHistoryOrder(items.filter((item) => item.finding_id === findingId)));
        })
        .catch(() => {
          // An optional audit endpoint must never clear or block the report,
          // evidence, verification controls, or case membership around it.
          if (active) setEvents([]);
        });
    }
    return () => { active = false; abort.abort(); };
  }, [findingId]);

  if (events.length === 0) return null;

  return (
    <Card className="min-w-0">
      <details open={expanded} onToggle={(event) => setExpanded(event.currentTarget.open)}>
        <summary className="flex cursor-pointer list-none items-center gap-2 px-6 text-sm font-medium [&::-webkit-details-marker]:hidden">
          <HistoryIcon className="size-4 shrink-0 text-muted-foreground" />
          原始上报历史
          <Badge variant="secondary">{events.length}</Badge>
          <ChevronDownIcon className={`ml-auto size-4 transition-transform ${expanded ? "rotate-180" : ""}`} />
        </summary>
        {expanded && (
          <div className="space-y-4 px-6 pt-4">
            <p className="text-muted-foreground text-xs">
              以下是同入口候选聚合保留的首次与后续上报快照，仅供追溯，不代表当前验证结论，也不替代漏洞文件夹归并历史。
            </p>
            <ol className="space-y-3">
              {events.slice(0, visible).map((event) => {
                const payload = inlineReportPayload(event.payload);
                const title = inlineReportText(payload.name || payload.title || payload.vulnclass) || "未命名上报";
                const summary = inlineReportText(payload.summary);
                const evidence = inlineReportText(payload.evidence);
                return (
                  <li key={event.id} className="min-w-0 space-y-3 rounded-lg border p-3">
                    <div className="flex flex-wrap items-center gap-2 text-xs">
                      <Badge variant="outline">
                        {event.kind === "original" ? "首次上报快照" : event.kind === "incoming" ? "后续原始上报" : "历史上报"}
                      </Badge>
                      <time dateTime={event.created_at} className="text-muted-foreground" title="该上报快照的审计保存时间">留档：{historyTime(event.created_at)}</time>
                      <span className="text-muted-foreground">审计 #{event.id}</span>
                    </div>
                    <p className="break-words text-sm font-medium">{title}</p>
                    {payload.severity ? <p className="text-muted-foreground text-xs">上报等级：{inlineReportText(payload.severity)}</p> : null}
                    {summary ? <p className="whitespace-pre-wrap break-words text-sm">{summary}</p> : null}
                    {evidence ? (
                      <details className="min-w-0">
                        <summary className="cursor-pointer text-xs">该次上报证据</summary>
                        <pre className="mt-2 max-h-72 overflow-auto whitespace-pre-wrap break-words rounded bg-muted p-3 font-mono text-xs">{evidence}</pre>
                      </details>
                    ) : null}
                    <details className="min-w-0">
                      <summary className="cursor-pointer text-muted-foreground text-xs">原始 JSON 快照（非当前状态）</summary>
                      <pre className="mt-2 max-h-72 overflow-auto whitespace-pre-wrap break-words rounded bg-muted p-3 font-mono text-xs">{inlineReportText(event.payload)}</pre>
                    </details>
                  </li>
                );
              })}
            </ol>
            {visible < events.length ? (
              <Button variant="outline" size="sm" onClick={() => setVisible((count) => count + PAGE_SIZE)}>
                显示更多（剩余 {events.length - visible} 条）
              </Button>
            ) : null}
          </div>
        )}
      </details>
    </Card>
  );
}
