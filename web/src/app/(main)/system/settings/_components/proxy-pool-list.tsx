"use client";

import * as React from "react";

import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { Switch } from "@/components/ui/switch";
import { Textarea } from "@/components/ui/textarea";
import { filterProxyPool, PROXY_POOL_PAGE_SIZE, proxyPoolPage } from "@/lib/proxy-pool-pagination";
import type { ProxyPoolEntry } from "@/lib/types";

type Props = {
  entries: ProxyPoolEntry[];
  activeId: string;
  busy: boolean;
  draft: React.MutableRefObject<string>;
  onImport: (text: string) => Promise<boolean>;
  onTestAll: () => void;
  onRefresh: () => void;
  onToggle: (entry: ProxyPoolEntry) => void;
  onActivate: (id: string) => void;
  onTest: (id: string) => void;
  onDelete: (id: string) => void;
};

function ProxyPoolImport({ draft, busy, onImport }: Pick<Props, "draft" | "busy" | "onImport">) {
  // Keep a potentially large import draft out of the settings-page render path.
  // The ref also preserves it while the category is not mounted.
  const [text, setText] = React.useState(() => draft.current);
  const [pending, setPending] = React.useState(false);
  const hasText = React.useMemo(() => /\S/.test(text), [text]);
  React.useEffect(() => {
    // An import may finish while this category is unmounted. Read the latest
    // draft again when the global operation lock is released.
    if (!busy && !pending) setText(draft.current);
  }, [busy, draft, pending]);
  const submit = async () => {
    if (busy || pending || !hasText) return;
    setPending(true);
    const submitted = text;
    try {
      if (await onImport(submitted)) {
        setText((current) => current === submitted ? "" : current);
      }
    } finally {
      setPending(false);
    }
  };
  return (
    <details className="rounded-md border px-3 py-2 text-xs">
      <summary className="text-muted-foreground cursor-pointer select-none">批量导入代理</summary>
      <Textarea
        aria-label="批量代理文本"
        className="mt-3 font-mono"
        rows={3}
        placeholder="每行一个代理：URL|名称|备注"
        value={text}
        onChange={(event) => {
          draft.current = event.target.value;
          setText(event.target.value);
        }}
      />
      <div className="mt-2 flex flex-wrap items-center gap-2">
        <Button type="button" size="sm" onClick={() => void submit()} disabled={busy || pending || !hasText}>
          {pending ? "导入中…" : "导入"}
        </Button>
        <span className="text-muted-foreground">显示代理地址时会隐藏账号密码。</span>
      </div>
    </details>
  );
}

export function ProxyPoolList({ entries, activeId, busy, draft, onImport, onTestAll, onRefresh, onToggle, onActivate, onTest, onDelete }: Props) {
  const [query, setQuery] = React.useState("");
  const [requestedPage, setRequestedPage] = React.useState(1);
  const deferredQuery = React.useDeferredValue(query);
  const filtered = React.useMemo(() => filterProxyPool(entries, deferredQuery), [entries, deferredQuery]);
  const { items, page, pages } = proxyPoolPage(filtered, requestedPage);
  const active = React.useMemo(() => entries.find((entry) => entry.id === activeId), [entries, activeId]);
  React.useEffect(() => {
    setRequestedPage(page);
  }, [page]);
  return (
    <div data-testid="proxy-pool-list" className="space-y-3">
      <div className="flex flex-wrap items-center justify-between gap-2">
        <div>
          <p className="text-sm font-medium">代理池 <span className="text-muted-foreground font-normal">{entries.length}</span></p>
          <p className="text-muted-foreground mt-1 text-xs">当前：{active?.name || (activeId ? "已选择代理" : "直连")}</p>
        </div>
        <div className="flex items-center gap-2">
          <Button type="button" variant="outline" size="sm" onClick={onRefresh} disabled={busy}>刷新状态</Button>
          <Button type="button" variant="outline" size="sm" onClick={onTestAll} disabled={busy}>测试全部</Button>
        </div>
      </div>
      <ProxyPoolImport draft={draft} busy={busy} onImport={onImport} />
      <Input
        aria-label="搜索代理"
        placeholder="搜索名称、代理地址、备注、出口 IP"
        value={query}
        onChange={(event) => {
          setQuery(event.target.value);
          setRequestedPage(1);
        }}
      />
      <div className="flex flex-wrap items-center justify-between gap-2 text-xs text-muted-foreground">
        <span>共 {entries.length} 条，匹配 {filtered.length} 条，每页 {PROXY_POOL_PAGE_SIZE} 条</span>
        <div className="flex items-center gap-2">
          <Button type="button" variant="outline" size="sm" aria-label="上一页代理" disabled={page <= 1} onClick={() => setRequestedPage(page - 1)}>上一页</Button>
          <span aria-live="polite">第 {page} / {pages} 页</span>
          <Button type="button" variant="outline" size="sm" aria-label="下一页代理" disabled={page >= pages} onClick={() => setRequestedPage(page + 1)}>下一页</Button>
        </div>
      </div>
      <div className="space-y-2" aria-busy={query !== deferredQuery}>
        {items.length === 0 ? (
          <p className="text-muted-foreground text-xs">{entries.length ? "没有匹配的代理。" : "暂无代理池条目。"}</p>
        ) : items.map((entry) => (
          <div key={entry.id} data-testid="proxy-pool-row" data-proxy-id={entry.id} className="grid gap-3 rounded-md border p-3 sm:grid-cols-[minmax(0,1fr)_auto] sm:items-center">
            <div className="flex min-w-0 items-start gap-3">
              <Switch checked={entry.enabled} onCheckedChange={() => onToggle(entry)} aria-label="启用代理" />
              <div className="grid min-w-0 flex-1 gap-x-5 gap-y-2 text-xs sm:grid-cols-2">
                <div className="min-w-0">
                  <p className="text-muted-foreground">代理地址</p>
                  <p className="truncate font-mono" title={entry.url}>{entry.name ? `${entry.name} · ` : ""}{entry.url}</p>
                </div>
                <div>
                  <p className="text-muted-foreground">测试出口 IP</p>
                  <p className="break-all font-mono">{entry.egress_ip || (entry.last_error ? "探测失败" : "尚未测试")}</p>
                </div>
                <div className="flex flex-wrap items-center gap-x-2 gap-y-1">
                  <span className={entry.health === "healthy" ? "text-emerald-600" : entry.health === "disabled" ? "text-muted-foreground" : "text-amber-600"}>
                    {!entry.enabled ? "已停用" : entry.health === "healthy" ? "健康" : entry.health === "unhealthy-cooling" ? "冷却中" : "待检测"}
                  </span>
                  {entry.id === activeId && <span className="text-primary">当前使用</span>}
                  <span className="text-muted-foreground">失败 {entry.consecutive_failures} 次</span>
                </div>
                <div className="text-muted-foreground">
                  {entry.latency_ms != null ? `${entry.latency_ms} ms` : "暂无延迟数据"}
                  {entry.last_checked_at && !entry.last_checked_at.startsWith("0001-") && ` · ${new Date(entry.last_checked_at).toLocaleTimeString()}`}
                  {entry.last_error && <span className="text-destructive"> · {entry.last_error}</span>}
                </div>
              </div>
            </div>
            <div className="flex justify-end gap-1">
              <Button type="button" variant="ghost" size="sm" disabled={!entry.enabled || entry.id === activeId} onClick={() => onActivate(entry.id)}>使用</Button>
              <Button type="button" variant="ghost" size="sm" onClick={() => onTest(entry.id)}>测试</Button>
              <Button type="button" variant="ghost" size="sm" onClick={() => onDelete(entry.id)}>删除</Button>
            </div>
          </div>
        ))}
      </div>
    </div>
  );
}
