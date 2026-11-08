"use client";

import * as React from "react";

import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { Switch } from "@/components/ui/switch";
import { Checkbox } from "@/components/ui/checkbox";
import { AlertDialog, AlertDialogAction, AlertDialogCancel, AlertDialogContent, AlertDialogDescription, AlertDialogFooter, AlertDialogHeader, AlertDialogTitle } from "@/components/ui/alert-dialog";
import { Dialog, DialogContent, DialogDescription, DialogHeader, DialogTitle } from "@/components/ui/dialog";
import { api } from "@/lib/api";
import { assertProxyDiagnosis, proxyDiagnosisPrimaryLabel, proxyProbeStatus, proxyProtocolStatus } from "@/lib/proxy-probe-status";
import { filterProxyPool, PROXY_POOL_PAGE_SIZE, proxyCleanupEligible, proxyLastCheckFailed, proxyPoolPage } from "@/lib/proxy-pool-pagination";
import type { EgressProbeResult, ProxyCapabilityCheck, ProxyProtocolCheck, ProxyPoolBulkRequest, ProxyPoolDiagnosis, ProxyPoolEntry, ProxyProbeProgress } from "@/lib/types";

type Props = {
  entries: ProxyPoolEntry[];
  activeId: string;
  busy: boolean;
  draft: React.MutableRefObject<string>;
  onImport: (text: string, protocol: string) => Promise<boolean>;
  onTestAll: () => Promise<void>;
  onSnapshot: (snapshot: Awaited<ReturnType<typeof api.proxyPool>>) => void;
  onRefresh: () => void;
  onToggle: (entry: ProxyPoolEntry) => void;
  onActivate: (id: string) => void;
  onTest: (id: string) => Promise<void>;
  onDelete: (id: string) => void;
  healthThreshold: number;
  onBulk: (request: ProxyPoolBulkRequest) => Promise<boolean>;
};

function ProxyPoolImport({ draft, busy, onImport }: Pick<Props, "draft" | "busy" | "onImport">) {
  // A bulk editor must not autosize or synchronize a large string through
  // React on every input. Keep only small UI flags in state; the shared ref
  // preserves the full draft while this category/editor is not mounted.
  const [expanded, setExpanded] = React.useState(false);
  const [protocol, setProtocol] = React.useState("auto");
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
      if (await onImport(submitted, protocol)) {
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
        <label className="mt-2 flex flex-wrap items-center gap-2">未写协议的地址
          <select aria-label="无协议地址处理" value={protocol} onChange={(event) => setProtocol(event.target.value)} disabled={pending || busy} className="rounded border bg-background p-1">
            <option value="auto">自动识别（HTTP / HTTPS / SOCKS5）</option>
            <option value="http">按 HTTP 导入</option><option value="https">按 HTTPS 导入</option><option value="socks5">按 SOCKS5 导入</option>
          </select>
        </label>
        <p className="mt-1 text-muted-foreground">已填写协议或分组标签时按原协议测试。自动识别须通过 HTTPS 出口验证，未确认前不能使用；SOCKS 按 SOCKS5 处理。</p>
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
          placeholder="代理 URL 或 IP:端口；支持 #备注、SOCKS5 分组和 URL|名称|备注"
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
          <span className="text-muted-foreground">支持换行或空格分隔。未写协议默认自动识别，随后沿用最近的明确协议；SOCKS5 列表前可单独写 socks5。# 后为备注，账号密码会掩码显示。</span>
        </div>
      </> : null}
    </div>
  );
}

function DiagnosisResult({ label, result }: { label: string; result: EgressProbeResult }) {
  return <section className="space-y-2 rounded-md border p-3 text-sm">
    <p className="font-medium">{label}：{result.ok ? (result.kind === "connectivity" ? "通过" : "回显成功") : "失败"}</p>
    <p className={result.ok ? "text-emerald-600" : "text-destructive"}>{result.ok ? (result.kind === "connectivity" ? `响应 HTTP ${result.status_code}` : `出口 IP：${result.exit_ip}`) : result.error}</p>
    <ul className="space-y-1 text-xs text-muted-foreground">
      {result.probes.map((attempt, index) => <li key={index} className="break-all">{attempt.url} · {attempt.ok ? (attempt.status_code ? `HTTP ${attempt.status_code}` : attempt.ip) : attempt.error} · {attempt.latency_ms} ms</li>)}
    </ul>
  </section>;
}

function capabilityLabel(check: ProxyCapabilityCheck) {
  if (!check.checked) return "未执行";
  return check.ok ? (check.status_code ? `连通性通过 · HTTP ${check.status_code}` : `回显通过 · ${check.ip}`) : check.error || "未通过";
}

function ProtocolChecks({ checks }: { checks: ProxyProtocolCheck[] }) {
  return <div className="space-y-2" data-testid="proxy-protocol-checks">{checks.map((check) => <section key={check.protocol} className="rounded-md border p-2 text-xs">
    <p className="font-medium">代理协议 {check.protocol.toUpperCase()}：{check.recognized ? "已识别" : "未确认"}{!check.supported ? " · 暂不支持转发" : ""}</p>
    <p className="text-muted-foreground">{check.evidence}</p>
    {check.supported ? <><p>访问 HTTPS 网站（校验证书）：{capabilityLabel(check.https)}</p><p>访问 HTTP 网站（明文回显）：{capabilityLabel(check.http)}</p></> : null}
  </section>)}</div>;
}

export function ProxyPoolList({ entries, activeId, busy: parentBusy, draft, onImport, onTestAll, onSnapshot, onRefresh, onToggle, onActivate, onTest, onDelete, healthThreshold, onBulk }: Props) {
  const [progress, setProgress] = React.useState<ProxyProbeProgress | null>(null);
  const [progressError, setProgressError] = React.useState("");
  const [pollVersion, setPollVersion] = React.useState(0);
  const [starting, setStarting] = React.useState(false);
  const [testing, setTesting] = React.useState<Set<string>>(() => new Set());
  const testPending = React.useRef(new Set<string>());
  const snapshotCallback = React.useRef(onSnapshot);
  snapshotCallback.current = onSnapshot;
  const refreshedRound = React.useRef("");
  const [query, setQuery] = React.useState("");
  const [failedOnly, setFailedOnly] = React.useState(false);
  const [requestedPage, setRequestedPage] = React.useState(1);
  const [selected, setSelected] = React.useState<Set<string>>(() => new Set());
  const [confirmation, setConfirmation] = React.useState<{ request: ProxyPoolBulkRequest; title: string; threshold: number } | null>(null);
  const submitting = React.useRef(false);
  const [diagnosis, setDiagnosis] = React.useState<{ id: string; url: string; result?: ProxyPoolDiagnosis; error?: string } | null>(null);
  const diagnosticRequest = React.useRef<AbortController | null>(null);
  const [diagnosing, setDiagnosing] = React.useState(false);
  const busy = parentBusy;
  const closeDiagnosis = () => {
    diagnosticRequest.current?.abort();
    diagnosticRequest.current = null;
    setDiagnosing(false);
    setDiagnosis(null);
  };
  React.useEffect(() => () => { diagnosticRequest.current?.abort(); diagnosticRequest.current = null; }, []);
  React.useEffect(() => {
    if (diagnosis && !entries.some((entry) => entry.id === diagnosis.id && entry.url === diagnosis.url && entry.enabled)) closeDiagnosis();
  }, [entries, diagnosis]);
  const diagnose = async (entry: ProxyPoolEntry) => {
    if (busy || diagnosticRequest.current) return;
    const controller = new AbortController();
    diagnosticRequest.current = controller;
    setDiagnosing(true);
    setDiagnosis({ id: entry.id, url: entry.url });
    try {
      const result = assertProxyDiagnosis(await api.diagnoseProxyPoolEntry(entry.id, controller.signal), entry.id);
      if (diagnosticRequest.current === controller && !controller.signal.aborted) setDiagnosis({ id: entry.id, url: entry.url, result });
    } catch (error) {
      if (diagnosticRequest.current === controller && !controller.signal.aborted) setDiagnosis({ id: entry.id, url: entry.url, error: (error as Error).message });
    } finally {
      if (diagnosticRequest.current === controller) { diagnosticRequest.current = null; setDiagnosing(false); }
    }
  };
  const deferredQuery = React.useDeferredValue(query);
  const filtered = React.useMemo(() => filterProxyPool(failedOnly ? entries.filter(proxyLastCheckFailed) : entries, deferredQuery), [entries, deferredQuery, failedOnly]);
  const { items, page, pages } = proxyPoolPage(filtered, requestedPage);
  const visibleIDs = items.map((entry) => entry.id).join(",");
  React.useEffect(() => {
    if (parentBusy) return;
    const controller = new AbortController();
    let timer: ReturnType<typeof setTimeout>;
    const poll = async () => {
      let active = false;
      try {
        if (document.visibilityState !== "hidden") {
          const next = await api.proxyProbeProgress(visibleIDs ? visibleIDs.split(",") : [], controller.signal);
          if (controller.signal.aborted) return;
          if (!next || !Number.isFinite(next.completed) || !Number.isFinite(next.total) || !next.items) throw new Error("测试进度返回不完整");
          setProgress(next); setProgressError("");
          active = next.state === "running" || next.state === "queued";
          const key = `${next.id}:${next.state}`;
          if (!active && next.state !== "idle" && refreshedRound.current !== key) {
            const snapshot = await api.proxyPool();
            if (controller.signal.aborted) return;
            refreshedRound.current = key; snapshotCallback.current(snapshot);
          }
        }
      } catch (error) {
        if (!controller.signal.aborted) setProgressError((error as Error).message);
      } finally {
        if (!controller.signal.aborted) timer = setTimeout(poll, active ? 1000 : 4000);
      }
    };
    void poll();
    return () => { controller.abort(); clearTimeout(timer); };
  }, [visibleIDs, pollVersion, parentBusy, entries]);
  const startBatch = async () => {
    if (starting) return;
    setStarting(true);
    try { await onTestAll(); setPollVersion((value) => value + 1); } finally { setStarting(false); }
  };
  const testOne = async (id: string) => {
    if (testPending.current.has(id)) return;
    testPending.current.add(id); setTesting(new Set(testPending.current));
    try { await onTest(id); } finally { testPending.current.delete(id); setTesting(new Set(testPending.current)); setPollVersion((value) => value + 1); }
  };
  const cancelBatch = async () => {
    try { await api.cancelProxyPoolTests(); setPollVersion((value) => value + 1); } catch (error) { setProgressError((error as Error).message); }
  };
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
          <Button type="button" variant="outline" size="sm" onClick={() => void startBatch()} disabled={busy || starting || progress?.state === "running" || progress?.state === "queued"}>测试全部</Button>
          <Button type="button" variant="outline" size="sm" onClick={() => confirmBulk("delete", true)} disabled={busy || invalidIDs.length === 0}>清理失效（{invalidIDs.length}）</Button>
        </div>
      </div>
      <ProxyPoolImport draft={draft} busy={busy} onImport={onImport} />
      {progress && progress.state !== "idle" ? <div className="space-y-2 rounded-md border p-3 text-xs" data-testid="proxy-probe-progress" aria-live="polite">
        <p>{({ queued: "等待测试", running: "正在测试", completed: "测试完成", cancelled: "本轮已停止", superseded: "配置已变更，本轮结果作废", save_failed: "测试结果保存失败" } as Record<string, string>)[progress.state]}：{progress.completed} / {progress.total} · 成功 {progress.succeeded} · 失败 {progress.failed} · 执行中 {progress.running}</p>
        <progress aria-label="代理测试进度" className="h-2 w-full" max={Math.max(1, progress.total)} value={progress.completed} />
        {progress.state === "running" || progress.state === "queued" ? <><p>批量最多 {progress.concurrency} 条并行；单条测试使用独立名额。本轮结果逐条可见，健康状态在整批完成后保存。</p><Button type="button" size="sm" variant="outline" onClick={() => void cancelBatch()}>停止本轮测试</Button></> : null}
      </div> : null}
      {progressError ? <p role="alert" className="text-xs text-destructive">进度暂不可读：{progressError}</p> : null}
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
      <p className="text-muted-foreground text-xs">更改搜索或筛选会清空选择。失效清理覆盖整个代理池，按已保存的连续失败阈值（{healthThreshold} 次）判断，跳过当前使用项和未检测项。可先测试全部，进度与完成结果自动刷新。</p>
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
                  {entry.protocol_auto ? <p className="text-muted-foreground">{proxyProtocolStatus(entry)}</p> : null}
                  {entry.protocol_checks?.length ? <details className="mt-1"><summary className="cursor-pointer">各代理协议检测详情</summary><ProtocolChecks checks={entry.protocol_checks} /></details> : null}
                </div>
                <div>
                  <p className="text-muted-foreground">HTTPS 连通性</p>
                  <p className="break-all font-mono">{entry.last_error ? "最近检测未通过" : entry.last_probe_kind === "connectivity" ? `通过 · HTTP ${entry.last_http_status ?? "成功"}` : entry.egress_ip ? "历史回显结果，需重新检测" : "尚未测试"}</p>
                  {entry.last_error && entry.egress_ip ? <p className="text-muted-foreground">上次成功出口：{entry.egress_ip}</p> : null}
                </div>
                <div className="flex flex-wrap items-center gap-x-2 gap-y-1">
                  <span className={proxyProbeStatus(entry, healthThreshold).tone === "healthy" ? "text-emerald-600" : proxyProbeStatus(entry, healthThreshold).tone === "failed" ? "text-destructive" : proxyProbeStatus(entry, healthThreshold).tone === "limited" ? "text-amber-600" : "text-muted-foreground"}>
                    {proxyProbeStatus(entry, healthThreshold).label}
                  </span>
                  {entry.id === activeId && <span className="text-primary">当前使用</span>}
                  <span className="text-muted-foreground">检测失败 {entry.consecutive_failures} 次</span>
                </div>
                <div className="text-muted-foreground">
                  {progress?.items[entry.id] && ["running", "queued"].includes(progress.state) ? <p data-testid="proxy-row-progress">本轮：{progress.items[entry.id].state === "done" ? (progress.items[entry.id].ok ? `通过 ${progress.items[entry.id].protocol?.toUpperCase() ?? ""} · ${progress.items[entry.id].ip ?? ""}` : `失败 · ${progress.items[entry.id].error ?? ""}`) : progress.items[entry.id].state === "running" ? "测试中…" : progress.items[entry.id].state === "superseded" ? "已由单条测试更新" : "等待测试"}</p> : null}
                  {entry.latency_ms != null ? `${entry.latency_ms} ms` : "暂无延迟数据"}
                  {entry.last_checked_at && !entry.last_checked_at.startsWith("0001-") && ` · ${new Date(entry.last_checked_at).toLocaleTimeString()}`}
                  {entry.last_error && <span className="text-destructive"> · {entry.last_error === "全部探测地址均失败" ? "历史检测失败，点击诊断查看当前原因" : entry.last_error}</span>}
                </div>
              </div>
            </div>
            <div className="flex justify-end gap-1">
              <Button type="button" variant="ghost" size="sm" disabled={busy || !entry.enabled || entry.id === activeId || (entry.protocol_auto && !entry.detected_protocol)} onClick={() => onActivate(entry.id)}>使用</Button>
              <Button type="button" variant="ghost" size="sm" disabled={busy || !entry.enabled || testing.has(entry.id)} onClick={() => void testOne(entry.id)}>{testing.has(entry.id) ? "测试中…" : "测试"}</Button>
              <Button type="button" variant="ghost" size="sm" disabled={busy || !entry.enabled} onClick={() => void diagnose(entry)}>诊断</Button>
              <Button type="button" variant="ghost" size="sm" disabled={busy} onClick={() => onDelete(entry.id)}>删除</Button>
            </div>
          </div>
        ))}
      </div>
      <Dialog open={diagnosis !== null} onOpenChange={(open) => { if (!open) closeDiagnosis(); }}>
        <DialogContent className="max-h-[85vh] overflow-y-auto">
          <DialogHeader>
            <DialogTitle>代理协议与连通性诊断</DialogTitle>
            <DialogDescription>通过所选代理访问 HTTPS 检测页并校验证书，不要求返回出口 IP。自定义检测网址只访问该站点。诊断不修改健康状态、当前出口或任务暂停状态。</DialogDescription>
          </DialogHeader>
          <p className="break-all font-mono text-xs">{diagnosis?.url}</p>
          {diagnosing ? <p role="status">正在诊断，请稍候…</p> : null}
          {diagnosis?.error ? <p role="alert" className="text-destructive">{diagnosis.error}</p> : null}
          {diagnosis?.result ? <div className="space-y-3">
            {diagnosis.result.https.protocol_checks?.length ? <ProtocolChecks checks={diagnosis.result.https.protocol_checks} /> : <>
            <p className="text-sm">代理连接协议：{diagnosis.result.https.via?.toUpperCase() || "按已配置协议"}</p>
            <DiagnosisResult label={proxyDiagnosisPrimaryLabel(diagnosis.result)} result={diagnosis.result.https} />
            {!diagnosis.result.http_skipped ? <DiagnosisResult label="访问 HTTP 网站（明文回显）" result={diagnosis.result.http} /> : null}
            </>}
            {diagnosis.result.http_skipped ? <p className="text-muted-foreground text-sm">仅检查 HTTPS 连通性，未查询出口 IP。</p> : null}
            {!diagnosis.result.http_skipped && diagnosis.result.http.ok && !diagnosis.result.https.ok ? <p className="text-amber-600 text-sm">仅访问 HTTP 网站回显通过，HTTPS 未通过；这不代表已找到可完整使用的代理协议。</p> : null}
            {diagnosis.result.probe_mode === "configured" ? <p className="break-all font-mono text-xs">探测服务：{diagnosis.result.probe_url}</p> : null}
            <p className="text-muted-foreground text-xs">本次诊断：{new Date(diagnosis.result.checked_at).toLocaleString()}</p>
          </div> : null}
          <Button type="button" variant="outline" onClick={closeDiagnosis}>关闭诊断</Button>
        </DialogContent>
      </Dialog>
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
