"use client";

import * as React from "react";

import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { Switch } from "@/components/ui/switch";
import { Checkbox } from "@/components/ui/checkbox";
import { AlertDialog, AlertDialogAction, AlertDialogCancel, AlertDialogContent, AlertDialogDescription, AlertDialogFooter, AlertDialogHeader, AlertDialogTitle } from "@/components/ui/alert-dialog";
import { filterProxyPool, PROXY_POOL_PAGE_SIZE, proxyCleanupEligible, proxyLastCheckFailed, proxyPoolPage } from "@/lib/proxy-pool-pagination";
import type { ProxyPoolBulkRequest, ProxyPoolEntry } from "@/lib/types";

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
  healthThreshold: number;
  onBulk: (request: ProxyPoolBulkRequest) => Promise<boolean>;
};

function ProxyPoolImport({ draft, busy, onImport }: Pick<Props, "draft" | "busy" | "onImport">) {
  // A bulk editor must not autosize or synchronize a large string through
  // React on every input. Keep only small UI flags in state; the shared ref
  // preserves the full draft while this category/editor is not mounted.
  const [expanded, setExpanded] = React.useState(false);
  const [pending, setPending] = React.useState(false);
  const [hasText, setHasText] = React.useState(() => /\S/.test(draft.current));
  const editor = React.useRef<HTMLTextAreaElement>(null);
  React.useEffect(() => {
    if (!busy && !pending) {
      if (editor.current && editor.current.value !== draft.current) editor.current.value = draft.current;
      setHasText(/\S/.test(draft.current));
    }
  }, [busy, draft, pending, expanded]);
  const submit = async () => {
    if (busy || pending || !hasText) return;
    const submitted = editor.current?.value ?? draft.current;
    setPending(true);
    try {
      if (await onImport(submitted)) {
        if (editor.current?.value === submitted && draft.current === "") editor.current.value = "";
        setHasText(/\S/.test(draft.current));
      }
    } finally {
      setPending(false);
    }
  };
  return (
    <div className="rounded-md border px-3 py-2 text-xs">
      <Button
        type="button"
        variant="ghost"
        size="sm"
        className="text-muted-foreground -ml-2"
        aria-expanded={expanded}
        onClick={() => setExpanded((open) => !open)}
      >
        批量导入代理
      </Button>
      {expanded ? <>
        <textarea
          ref={editor}
          aria-label="批量代理文本"
          className="mt-3 block h-32 max-h-32 min-w-0 w-full overflow-auto rounded-md border border-input bg-transparent px-2.5 py-2 font-mono text-xs outline-none focus-visible:border-ring focus-visible:ring-2"
          style={{ fieldSizing: "fixed" }}
          rows={3}
          wrap="off"
          spellCheck={false}
          autoCorrect="off"
          autoCapitalize="off"
          placeholder="每行一个代理：URL|名称|备注"
          defaultValue={draft.current}
          onChange={(event) => {
            draft.current = event.target.value;
            setHasText(/\S/.test(event.target.value));
          }}
        />
        <div className="mt-2 flex flex-wrap items-center gap-2">
          <Button type="button" size="sm" onClick={() => void submit()} disabled={busy || pending || !hasText}>
            {pending ? "导入中…" : "导入"}
          </Button>
          <span className="text-muted-foreground">显示代理地址时会隐藏账号密码。</span>
        </div>
      </> : null}
    </div>
  );
}

export function ProxyPoolList({ entries, activeId, busy, draft, onImport, onTestAll, onRefresh, onToggle, onActivate, onTest, onDelete, healthThreshold, onBulk }: Props) {
  const [query, setQuery] = React.useState("");
  const [failedOnly, setFailedOnly] = React.useState(false);
  const [requestedPage, setRequestedPage] = React.useState(1);
  const [selected, setSelected] = React.useState<Set<string>>(() => new Set());
  const [confirmation, setConfirmation] = React.useState<{ request: ProxyPoolBulkRequest; title: string; threshold: number } | null>(null);
  const submitting = React.useRef(false);
  const deferredQuery = React.useDeferredValue(query);
  const filtered = React.useMemo(() => filterProxyPool(failedOnly ? entries.filter(proxyLastCheckFailed) : entries, deferredQuery), [entries, deferredQuery, failedOnly]);
  const { items, page, pages } = proxyPoolPage(filtered, requestedPage);
  const active = React.useMemo(() => entries.find((entry) => entry.id === activeId), [entries, activeId]);
  const entryIDs = React.useMemo(() => new Set(entries.map((entry) => entry.id)), [entries]);
  const invalidIDs = React.useMemo(() => entries.filter((entry) => entry.id !== activeId && proxyCleanupEligible(entry, healthThreshold)).map((entry) => entry.id), [entries, activeId, healthThreshold]);
  const selectedCount = React.useMemo(() => [...selected].filter((id) => entryIDs.has(id)).length, [selected, entryIDs]);
  const pageSelected = items.filter((entry) => selected.has(entry.id)).length;
  React.useEffect(() => {
    setSelected((previous) => {
      const next = new Set([...previous].filter((id) => entryIDs.has(id)));
      return next.size === previous.size ? previous : next;
    });
  }, [entryIDs]);
  const togglePage = (checked: boolean) => setSelected((previous) => {
    const next = new Set(previous);
    for (const entry of items) { if (checked) next.add(entry.id); else next.delete(entry.id); }
    return next;
  });
  const confirmBulk = (action: "delete" | "disable", cleanup = false) => {
    if (busy) return;
    const ids = cleanup ? invalidIDs : [...selected].filter((id) => entryIDs.has(id));
    if (ids.length === 0) return;
    setConfirmation({
      request: { action, ids, only_unhealthy: cleanup, allow_active: !cleanup && ids.includes(activeId), expected_active_id: activeId },
      title: cleanup ? `确认清理 ${ids.length} 条失效代理？` : `确认${action === "delete" ? "删除" : "禁用"} ${ids.length} 条代理？`,
      threshold: healthThreshold,
    });
  };
  const submitBulk = async () => {
    if (!confirmation || busy || submitting.current) return;
    const snapshot = confirmation;
    submitting.current = true;
    setConfirmation(null);
    try {
      if (await onBulk(snapshot.request)) {
        const processed = new Set(snapshot.request.ids);
        setSelected((previous) => new Set([...previous].filter((id) => !processed.has(id))));
      }
    } finally { submitting.current = false; }
  };
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
        <div className="flex flex-wrap items-center gap-2">
          <Button type="button" variant="outline" size="sm" onClick={onRefresh} disabled={busy}>刷新状态</Button>
          <Button type="button" variant="outline" size="sm" onClick={onTestAll} disabled={busy}>测试全部</Button>
          <Button type="button" variant="outline" size="sm" onClick={() => confirmBulk("delete", true)} disabled={busy || invalidIDs.length === 0}>清理失效（{invalidIDs.length}）</Button>
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
          setSelected(new Set());
        }}
      />
      <label className="flex items-center gap-2 text-xs">
        <Checkbox aria-label="只看最近检测失败的代理" checked={failedOnly} onCheckedChange={(checked) => { setFailedOnly(checked === true); setRequestedPage(1); setSelected(new Set()); }} />
        只看最近检测失败的代理（不限连续失败次数）
      </label>
      <div className="flex flex-wrap items-center gap-2 rounded-md border p-2 text-xs" data-testid="proxy-pool-bulk-toolbar">
        <label className="flex items-center gap-2">
          <Checkbox aria-label="全选本页代理" checked={pageSelected === 0 ? false : pageSelected === items.length ? true : "indeterminate"} disabled={busy || items.length === 0} onCheckedChange={(checked) => togglePage(checked === true)} />
          本页
        </label>
        <Button type="button" variant="ghost" size="sm" disabled={busy || filtered.length === 0 || query !== deferredQuery} onClick={() => setSelected(new Set(filtered.map((entry) => entry.id)))}>全选匹配（{filtered.length}）</Button>
        <span aria-live="polite">已选 {selectedCount} 条（跨页保留）</span>
        <Button type="button" variant="ghost" size="sm" disabled={busy || selectedCount === 0} onClick={() => setSelected(new Set())}>清空选择</Button>
        <Button type="button" variant="outline" size="sm" disabled={busy || selectedCount === 0} onClick={() => confirmBulk("disable")}>禁用选中</Button>
        <Button type="button" variant="destructive" size="sm" disabled={busy || selectedCount === 0} onClick={() => confirmBulk("delete")}>删除选中</Button>
      </div>
      <p className="text-muted-foreground text-xs">更改搜索或筛选会清空选择。失效清理覆盖整个代理池，按已保存的连续失败阈值（{healthThreshold} 次）判断，跳过当前使用项和未检测项。可先测试全部，稍后刷新状态再清理。</p>
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
              <Checkbox aria-label={`选择代理 ${entry.id}`} checked={selected.has(entry.id)} disabled={busy} onCheckedChange={(checked) => setSelected((previous) => { const next = new Set(previous); if (checked === true) next.add(entry.id); else next.delete(entry.id); return next; })} />
              <Switch checked={entry.enabled} disabled={busy} onCheckedChange={() => onToggle(entry)} aria-label="启用代理" />
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
              <Button type="button" variant="ghost" size="sm" disabled={busy || !entry.enabled || entry.id === activeId} onClick={() => onActivate(entry.id)}>使用</Button>
              <Button type="button" variant="ghost" size="sm" disabled={busy} onClick={() => onTest(entry.id)}>测试</Button>
              <Button type="button" variant="ghost" size="sm" disabled={busy} onClick={() => onDelete(entry.id)}>删除</Button>
            </div>
          </div>
        ))}
      </div>
      <AlertDialog open={confirmation !== null} onOpenChange={(open) => { if (!open) setConfirmation(null); }}>
        <AlertDialogContent>
          <AlertDialogHeader>
            <AlertDialogTitle>{confirmation?.title}</AlertDialogTitle>
            <AlertDialogDescription>
              {confirmation?.request.only_unhealthy ? `清理整个代理池中这 ${confirmation.request.ids.length} 条已连续失败达到 ${confirmation.threshold} 次的代理；恢复健康、失效次数不足或成为当前使用项的代理将跳过。` : `操作包含所选的全部页面，共 ${confirmation?.request.ids.length ?? 0} 条。`}
              {confirmation?.request.allow_active ? "包含当前使用的代理，操作后会切换到剩余已启用代理；若仅剩失效成员则暂停相关任务，没有已启用成员时改为直连。" : "当前使用的代理保持不变。"}
              {confirmation?.request.action === "delete" ? "删除后需要重新导入才能恢复。" : "禁用后仍保留记录，可逐条重新启用。"}
            </AlertDialogDescription>
          </AlertDialogHeader>
          <AlertDialogFooter>
            <AlertDialogCancel>取消</AlertDialogCancel>
            <AlertDialogAction disabled={busy} onClick={() => void submitBulk()}>确认{confirmation?.request.action === "delete" ? "删除" : "禁用"}</AlertDialogAction>
          </AlertDialogFooter>
        </AlertDialogContent>
      </AlertDialog>
    </div>
  );
}
