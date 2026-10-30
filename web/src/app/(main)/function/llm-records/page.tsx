"use client";

import * as React from "react";
import {
  RadioIcon,
  SearchIcon,
  ChevronLeftIcon,
  ChevronRightIcon,
  Loader2Icon,
  XIcon,
  Trash2Icon,
  CopyIcon,
  CheckIcon,
} from "lucide-react";

import {
  AlertDialog,
  AlertDialogAction,
  AlertDialogCancel,
  AlertDialogContent,
  AlertDialogDescription,
  AlertDialogFooter,
  AlertDialogHeader,
  AlertDialogTitle,
} from "@/components/ui/alert-dialog";

import { Card } from "@/components/ui/card";
import { Alert, AlertDescription } from "@/components/ui/alert";
import { Input } from "@/components/ui/input";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Switch } from "@/components/ui/switch";
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from "@/components/ui/table";
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from "@/components/ui/select";
import { HttpPacketSyntax } from "@/components/http-code-block";
import { cn } from "@/lib/utils";
import { api } from "@/lib/api";
import type { LLMRecordItem, LLMRecordDetail, LLMTask } from "@/lib/types";

function fmtTime(ts: string) {
  return new Date(ts).toLocaleString("zh-CN", {
    month: "2-digit",
    day: "2-digit",
    hour: "2-digit",
    minute: "2-digit",
    second: "2-digit",
  });
}

function fmtLatency(ms: number) {
  if (ms < 1000) return `${ms}ms`;
  return `${(ms / 1000).toFixed(1)}s`;
}

function fmtTokens(n: number) {
  if (n >= 1000) return `${(n / 1000).toFixed(1)}k`;
  return String(n);
}

function tryFormatJSON(s: string): string {
  try {
    return JSON.stringify(JSON.parse(s), null, 2);
  } catch {
    return s;
  }
}

// 复制当前框内文本的小按钮。复制成功后短暂显示对勾。text 为空/仅占位符时禁用。
function CopyButton({ text }: { text: string }) {
  const [copied, setCopied] = React.useState(false);
  const timer = React.useRef<ReturnType<typeof setTimeout> | null>(null);
  React.useEffect(
    () => () => {
      if (timer.current) clearTimeout(timer.current);
    },
    [],
  );

  const disabled = !text;
  const copy = async () => {
    if (disabled) return;
    try {
      await navigator.clipboard.writeText(text);
    } catch {
      // navigator.clipboard 在非安全上下文(如 http 局域网)不可用，回退到 execCommand。
      const ta = document.createElement("textarea");
      ta.value = text;
      ta.style.position = "fixed";
      ta.style.opacity = "0";
      document.body.appendChild(ta);
      ta.select();
      try {
        document.execCommand("copy");
      } catch {
        /* 忽略：不支持则静默 */
      }
      document.body.removeChild(ta);
    }
    setCopied(true);
    if (timer.current) clearTimeout(timer.current);
    timer.current = setTimeout(() => setCopied(false), 1500);
  };

  return (
    <Button
      variant="ghost"
      size="icon"
      className="size-5 shrink-0"
      disabled={disabled}
      title={copied ? "已复制" : "复制内容"}
      onClick={copy}
    >
      {copied ? <CheckIcon className="size-3 text-emerald-600" /> : <CopyIcon className="size-3" />}
    </Button>
  );
}

const PAGE_SIZES = [25, 50, 100];

export default function LLMRecordsPage() {
  const [page, setPage] = React.useState(0);
  const [size, setSize] = React.useState(50);
  const [session, setSession] = React.useState("");
  const [sessionQ, setSessionQ] = React.useState("");
  const [model, setModel] = React.useState("");

  const [records, setRecords] = React.useState<LLMRecordItem[]>([]);
  const [total, setTotal] = React.useState(0);
  const [loading, setLoading] = React.useState(false);
  const [listError, setListError] = React.useState<string | null>(null);
  const [loadedListKey, setLoadedListKey] = React.useState<string | null>(null);

  // Recording on/off toggle (settings.llm_record; default off). When off the
  // backend records nothing.
  const [recEnabled, setRecEnabled] = React.useState<boolean | null>(null);
  const [recLoading, setRecLoading] = React.useState(true);
  const [recError, setRecError] = React.useState<string | null>(null);
  const recordingSequence = React.useRef(0);
  const mounted = React.useRef(true);
  React.useEffect(() => {
    mounted.current = true;
    return () => { mounted.current = false; recordingSequence.current++; };
  }, []);
  const [recBusy, setRecBusy] = React.useState(false);

  // Inline detail panel (Burp-style split, not a dialog)
  const [selected, setSelected] = React.useState<LLMRecordItem | null>(null);
  const [detail, setDetail] = React.useState<LLMRecordDetail | null>(null);
  const [detailLoading, setDetailLoading] = React.useState(false);
  const [detailError, setDetailError] = React.useState<string | null>(null);
  const detailSequence = React.useRef(0);
  const selectedId = React.useRef(selected?.id);
  selectedId.current = selected?.id;
  // Never render a previous provider's payload under a newly selected row.
  const selectedDetail = detail?.id === selected?.id ? detail : null;
  // 归一化视图 / HTTP 原文视图。原文是排查 provider 侧问题的唯一依据：归一化视图
  // 不含工具 schema，响应里也没有 tool_use 块。
  const [rawView, setRawView] = React.useState(false);

  const hasRaw = !!(selectedDetail?.raw_request || selectedDetail?.raw_response);
  // 开关保持用户选择，但切到一条无原文的旧记录时自动落回解析视图，而不是显示空白。
  const showRaw = rawView && hasRaw;
  // 原文请求体是 JSON，pretty-print 只改排版不改语义，便于阅读；原文响应是 SSE
  // 帧，tryFormatJSON 解析失败会原样返回，故两边共用一个函数即可。
  const reqText = showRaw
    ? selectedDetail?.raw_request && tryFormatJSON(selectedDetail.raw_request)
    : selectedDetail?.request_body && tryFormatJSON(selectedDetail.request_body);
  const respText = showRaw ? selectedDetail?.raw_response : selectedDetail?.response_body && tryFormatJSON(selectedDetail.response_body);

  // Per-task delete (task picker + confirm dialog)
  const [tasks, setTasks] = React.useState<LLMTask[]>([]);
  const [pickedTask, setPickedTask] = React.useState("");
  const [deleteOpen, setDeleteOpen] = React.useState(false);
  const [deleting, setDeleting] = React.useState(false);
  const [reloadTick, setReloadTick] = React.useState(0); // manual refetch trigger
  const [tasksKnown, setTasksKnown] = React.useState(false);
  const [tasksError, setTasksError] = React.useState<string | null>(null);
  const [deleteError, setDeleteError] = React.useState<string | null>(null);
  const queryKey = JSON.stringify([page, size, sessionQ, model, pickedTask]);
  const queryKeyRef = React.useRef(queryKey);
  queryKeyRef.current = queryKey;
  const visibleRecords = loadedListKey === queryKey ? records : [];
  const currentListKnown = loadedListKey === queryKey;

  // Unknown recording state is not the same as a disabled recorder.
  React.useEffect(() => {
    let alive = true;
    const sequence = ++recordingSequence.current;
    setRecLoading(true);
    api.settings().then((settings) => {
      if (typeof settings.llm_record !== "boolean") throw new Error("录制状态响应不完整");
      if (alive && sequence === recordingSequence.current) {
        setRecEnabled(settings.llm_record);
        setRecError(null);
      }
    }).catch((error) => {
      if (alive && sequence === recordingSequence.current) setRecError((error as Error).message);
    }).finally(() => {
      if (alive && sequence === recordingSequence.current) setRecLoading(false);
    });
    return () => { alive = false; };
  }, [reloadTick]);

  const toggleRecording = async (on: boolean) => {
    if (recEnabled === null || recLoading || recError || recBusy) return;
    const sequence = ++recordingSequence.current;
    setRecBusy(true);
    try {
      const settings = await api.setSettings({ llm_record: on });
      if (typeof settings.llm_record !== "boolean") throw new Error("保存响应缺少录制状态，请刷新核对");
      if (mounted.current && sequence === recordingSequence.current) {
        setRecEnabled(settings.llm_record);
        setRecError(null);
      }
    } catch (error) {
      if (mounted.current && sequence === recordingSequence.current) setRecError((error as Error).message);
    } finally {
      if (mounted.current && sequence === recordingSequence.current) setRecBusy(false);
    }
  };

  // Debounce session filter.
  React.useEffect(() => {
    const t = setTimeout(() => setSessionQ(session.trim()), 300);
    return () => clearTimeout(t);
  }, [session]);

  // Reset page on filter change.
  React.useEffect(() => {
    setPage(0);
  }, [sessionQ, model, size, pickedTask]);

  // Keep last successful data, but never label another filter's data as current.
  React.useEffect(() => {
    let alive = true;
    const requestedKey = queryKey;
    setLoading(true);
    api.llmRecords({ model: model || undefined, session: sessionQ || undefined,
      task: pickedTask || undefined, page, size }).then((response) => {
      if (!Array.isArray(response.records) || !Number.isFinite(response.total) || response.total < 0) throw new Error("调用记录响应不完整");
      if (!alive || requestedKey !== queryKeyRef.current) return;
      setRecords(response.records);
      setTotal(response.total);
      setLoadedListKey(requestedKey);
      setListError(null);
    }).catch((error) => {
      if (alive && requestedKey === queryKeyRef.current) setListError((error as Error).message);
    }).finally(() => {
      if (alive && requestedKey === queryKeyRef.current) setLoading(false);
    });
    return () => { alive = false; };
  }, [page, size, sessionQ, model, pickedTask, reloadTick, queryKey]);

  React.useEffect(() => {
    let alive = true;
    api.llmTasks().then((response) => {
      if (!Array.isArray(response.tasks)) throw new Error("任务记录列表响应不完整");
      if (alive) { setTasks(response.tasks); setTasksKnown(true); setTasksError(null); }
    }).catch((error) => { if (alive) setTasksError((error as Error).message); });
    return () => { alive = false; };
  }, [reloadTick]);

  const confirmDelete = () => {
    if (!pickedTask || !tasksKnown || tasksError || deleting) return;
    const requestedTask = pickedTask;
    setDeleting(true);
    setDeleteError(null);
    api.llmRecordsDeleteTask(requestedTask).then(() => {
      if (!mounted.current) return;
      setDeleteOpen(false);
      setSelected((current) => current?.task_id === requestedTask ? null : current);
      setPickedTask((current) => current === requestedTask ? "" : current);
      setPage(0);
      setReloadTick((tick) => tick + 1);
    }).catch((error) => { if (mounted.current) setDeleteError((error as Error).message); })
      .finally(() => { if (mounted.current) setDeleting(false); });
  };

  React.useEffect(() => {
    const sequence = ++detailSequence.current;
    if (!selected) { setDetail(null); setDetailError(null); setDetailLoading(false); return; }
    const id = selected.id;
    let alive = true;
    setDetailLoading(true);
    setDetail(null);
    setDetailError(null);
    api.llmRecordDetail(id).then((response) => {
      if (!response || response.id !== id || typeof response.request_body !== "string" || typeof response.response_body !== "string") throw new Error("调用详情响应不完整");
      if (alive && sequence === detailSequence.current && selectedId.current === id) setDetail(response);
    }).catch((error) => {
      if (alive && sequence === detailSequence.current && selectedId.current === id) setDetailError((error as Error).message);
    }).finally(() => {
      if (alive && sequence === detailSequence.current && selectedId.current === id) setDetailLoading(false);
    });
    return () => { alive = false; };
  }, [selected, reloadTick]);

  const totalPages = Math.max(1, Math.ceil(total / size));
  const rangeStart = total === 0 ? 0 : page * size + 1;
  const rangeEnd = page * size + visibleRecords.length;

  return (
    <div className="flex min-h-0 flex-1 flex-col gap-4">
      {/* Header */}
      <div className="flex items-center justify-between gap-4">
        <div className="flex items-center gap-2">
          <RadioIcon className="h-5 w-5 text-muted-foreground" />
          <h1 className="text-xl font-semibold tracking-tight">LLM 录制</h1>
          <Badge variant="secondary">{currentListKnown ? total : "未知"}</Badge>
        </div>
      </div>

      {(recError || listError || tasksError || deleteError) && <Alert role="alert"><AlertDescription className="space-y-1">
        {recError && <p>录制状态读取或保存失败：{recError}。未自动切换开关，请刷新核对。</p>}
        {listError && <p>调用记录加载失败：{listError}。{currentListKnown ? "下方保留上次成功结果，可能已过期。" : "当前筛选结果未知，不表示没有记录。"}</p>}
        {tasksError && <p>任务记录列表读取失败：{tasksError}。暂停删除操作。</p>}
        {deleteError && <p>删除失败：{deleteError}。</p>}
      </AlertDescription></Alert>}
      {/* Toolbar */}
      <div className="flex flex-wrap items-center gap-2">
        <div className="relative max-w-sm flex-1">
          <SearchIcon className="absolute top-1/2 left-2.5 size-4 -translate-y-1/2 text-muted-foreground" />
          <Input
            placeholder="搜索 Session ID..."
            value={session}
            onChange={(e) => setSession(e.target.value)}
            className="h-8 pl-8"
          />
        </div>
        <Input placeholder="Model" className="h-8 w-48" value={model} onChange={(e) => setModel(e.target.value)} />
        <Select value={pickedTask} onValueChange={setPickedTask} disabled={!tasksKnown || !!tasksError || deleting}>
          <SelectTrigger size="sm" className="w-56">
            <SelectValue placeholder="选择任务…" />
          </SelectTrigger>
          <SelectContent>
            {tasks.length === 0 ? (
              <SelectItem value="__none__" disabled>
                暂无任务记录
              </SelectItem>
            ) : (
              tasks.map((t) => (
                <SelectItem key={t.task_id} value={t.task_id}>
                  <span className="font-mono">#{t.task_id}</span>
                  <span className="ml-2 text-muted-foreground">（{t.count}）</span>
                </SelectItem>
              ))
            )}
          </SelectContent>
        </Select>
        <Button
          variant="destructive"
          size="sm"
          className="h-8"
          disabled={!pickedTask || !tasksKnown || !!tasksError || deleting}
          title={pickedTask ? undefined : "先在上方选择任务"}
          onClick={() => setDeleteOpen(true)}
        >
          <Trash2Icon className="size-3.5" />
          删除任务对话
        </Button>
        <Select value={String(size)} onValueChange={(v) => setSize(Number(v))}>
          <SelectTrigger size="sm" className="w-28">
            <SelectValue />
          </SelectTrigger>
          <SelectContent>
            {PAGE_SIZES.map((n) => (
              <SelectItem key={n} value={String(n)}>
                {n} / 页
              </SelectItem>
            ))}
          </SelectContent>
        </Select>

        {/* Recording on/off — off means no LLM calls are recorded */}
        <div className="flex items-center gap-2 rounded-md border px-2.5 py-1">
          <Switch
            id="llm-rec-toggle"
            size="sm"
            checked={recEnabled === true}
            onCheckedChange={toggleRecording}
            disabled={recBusy || recLoading || !!recError || recEnabled === null}
          />
          <label
            htmlFor="llm-rec-toggle"
            className={cn(
              "cursor-pointer text-xs font-medium select-none",
              recEnabled ? "text-foreground" : "text-muted-foreground",
            )}
          >
            {recLoading ? "读取中…" : recError || recEnabled === null ? "状态未知" : recEnabled ? "录制中" : "已关闭"}
          </label>
        </div>

        <Button size="sm" variant="outline" disabled={recBusy || deleting} onClick={() => setReloadTick((tick) => tick + 1)}>刷新记录与状态</Button>
        <div className="ml-auto flex items-center gap-2 text-xs text-muted-foreground">
          <span className="tabular-nums">
            {currentListKnown ? `${rangeStart}–${rangeEnd} / ${total}` : "数量未知"}
          </span>
          <Button
            variant="outline"
            size="icon"
            className="size-8"
            disabled={page <= 0 || deleting || loading}
            onClick={() => setPage((p) => Math.max(0, p - 1))}
          >
            <ChevronLeftIcon />
          </Button>
          <span className="tabular-nums">
            {page + 1} / {totalPages}
          </span>
          <Button
            variant="outline"
            size="icon"
            className="size-8"
            disabled={!currentListKnown || page + 1 >= totalPages || deleting || loading}
            onClick={() => setPage((p) => Math.min(totalPages - 1, p + 1))}
          >
            <ChevronRightIcon />
          </Button>
        </div>
      </div>

      {/* History table + inline detail (Burp-style split) */}
      <div className="flex h-[calc(100vh-13rem)] min-h-0 flex-col gap-3">
        <Card className="flex min-h-0 flex-1 flex-col overflow-hidden py-0">
          <div className="min-h-0 flex-1 overflow-auto">
            <Table>
              <TableHeader className="sticky top-0 z-10 bg-card">
                <TableRow>
                  <TableHead className="w-[130px]">时间</TableHead>
                  <TableHead className="w-[60px]">任务</TableHead>
                  <TableHead className="w-[90px]">Worker</TableHead>
                  <TableHead className="w-[100px]">Profile</TableHead>
                  <TableHead className="w-[140px]">Model</TableHead>
                  <TableHead className="w-[70px]">延迟</TableHead>
                  <TableHead className="w-[90px]">Tokens</TableHead>
                  <TableHead className="w-[60px]">状态</TableHead>
                </TableRow>
              </TableHeader>
              <TableBody>
                {loading && visibleRecords.length === 0 ? (
                  <TableRow>
                    <TableCell colSpan={8} className="py-12 text-center">
                      <Loader2Icon className="mx-auto h-5 w-5 animate-spin text-muted-foreground" />
                    </TableCell>
                  </TableRow>
                ) : visibleRecords.length === 0 ? (
                  <TableRow>
                    <TableCell colSpan={8} className="py-12 text-center text-sm text-muted-foreground">
                      {listError ? "当前记录读取失败，请重试" : currentListKnown ? "暂无 LLM 调用记录" : "尚未取得当前记录"}
                    </TableCell>
                  </TableRow>
                ) : (
                  visibleRecords.map((rec) => (
                    <TableRow
                      key={rec.id}
                      className={cn("cursor-pointer", selected?.id === rec.id && "bg-accent hover:bg-accent")}
                      onClick={() => setSelected(rec)}
                    >
                      <TableCell className="text-xs text-muted-foreground tabular-nums">{fmtTime(rec.ts)}</TableCell>
                      <TableCell className="text-xs font-mono text-muted-foreground">
                        {rec.task_id ? `#${rec.task_id}` : "-"}
                      </TableCell>
                      <TableCell>
                        <Badge variant="outline" className="text-xs font-mono">
                          {rec.worker || "-"}
                        </Badge>
                      </TableCell>
                      <TableCell>
                        <span className="text-xs">{rec.profile_name || "-"}</span>
                      </TableCell>
                      <TableCell>
                        <span className="text-xs font-mono">{rec.model || "-"}</span>
                      </TableCell>
                      <TableCell>
                        <span className={cn("text-xs", rec.latency_ms > 30000 && "text-amber-500")}>
                          {fmtLatency(rec.latency_ms)}
                        </span>
                      </TableCell>
                      <TableCell>
                        <span className="text-xs">
                          {fmtTokens(rec.input_tokens)} / {fmtTokens(rec.output_tokens)}
                        </span>
                      </TableCell>
                      <TableCell>
                        {rec.status === "ok" ? (
                          <Badge variant="secondary" className="text-xs text-emerald-600">
                            OK
                          </Badge>
                        ) : (
                          <Badge variant="destructive" className="text-xs">
                            Error
                          </Badge>
                        )}
                      </TableCell>
                    </TableRow>
                  ))
                )}
              </TableBody>
            </Table>
          </div>
        </Card>

        {/* Inline detail panel */}
        {selected && (
          <Card className="flex h-[42%] min-h-0 flex-col overflow-hidden py-0">
            {/* Detail header */}
            <div className="flex items-center gap-2 border-b px-3 py-2">
              <Badge variant="outline" className="text-xs font-mono">
                #{selected.id}
              </Badge>
              <Badge variant="outline" className="text-xs font-mono">
                {selected.profile_name || "-"}
              </Badge>
              <Badge variant="outline" className="text-xs font-mono">
                {selected.model || "-"}
              </Badge>
              {selected.task_id && (
                <Badge variant="outline" className="text-xs font-mono">
                  任务 #{selected.task_id}
                </Badge>
              )}
              <span className="text-xs text-muted-foreground">{fmtTime(selected.ts)}</span>
              <span className={cn("text-xs", selected.latency_ms > 30000 && "text-amber-500")}>
                {fmtLatency(selected.latency_ms)}
              </span>
              {selected.status === "ok" ? (
                <Badge variant="secondary" className="text-xs text-emerald-600">
                  OK
                </Badge>
              ) : (
                <Badge variant="destructive" className="text-xs">
                  Error
                </Badge>
              )}
              {/* 原文视图开关。旧记录没有原文，此时禁用而非静默回退，避免看着像
                  「原文与解析一致」。 */}
              <Button
                variant={showRaw ? "secondary" : "ghost"}
                size="sm"
                className="ml-auto h-7 shrink-0 text-xs"
                disabled={!hasRaw}
                title={detailError ? "详情读取失败，请刷新重试" : !selectedDetail ? "详情尚未载入" : hasRaw ? "查看与 provider 实际收发的 HTTP 原文" : "该记录未保存 HTTP 原文"}
                onClick={() => setRawView((v) => !v)}
              >
                原文
              </Button>
              <Button variant="ghost" size="icon" className="size-7 shrink-0" onClick={() => setSelected(null)}>
                <XIcon />
              </Button>
            </div>
            {detailError && <p role="alert" className="border-b p-3 text-xs text-destructive">详情读取失败：{detailError}。未将错误显示为空响应。</p>}
            {/* Request / Response split */}
            <div className="grid min-h-0 flex-1 grid-cols-2 divide-x">
              <div className="flex min-h-0 min-w-0 flex-col">
                <div className="flex items-center gap-2 border-b py-0.5 pr-1.5 pl-3 text-[11px] font-medium text-muted-foreground">
                  <span>Request{showRaw && " · 原文"}</span>
                  <CopyButton text={reqText || ""} />
                </div>
                <div className="min-h-0 flex-1 overflow-auto">
                  {detailLoading ? (
                    <div className="flex items-center gap-2 p-3 text-xs text-muted-foreground">
                      <Loader2Icon className="size-3.5 animate-spin" />
                      加载…
                    </div>
                  ) : detailError ? <p className="p-3 text-xs text-muted-foreground">详情不可用，请刷新重试。</p> : (
                    <pre className="p-3 font-mono text-xs break-all whitespace-pre-wrap">
                      <HttpPacketSyntax raw={reqText || ""} lineNumbers={false} emptyFallback="（空）" />
                    </pre>
                  )}
                </div>
              </div>
              <div className="flex min-h-0 min-w-0 flex-col">
                <div className="flex items-center gap-2 border-b py-0.5 pr-1.5 pl-3 text-[11px] font-medium text-muted-foreground">
                  <span>Response{showRaw && " · 原文（SSE）"}</span>
                  <CopyButton text={respText || ""} />
                </div>
                <div className="min-h-0 flex-1 overflow-auto">
                  {detailLoading ? (
                    <div className="flex items-center gap-2 p-3 text-xs text-muted-foreground">
                      <Loader2Icon className="size-3.5 animate-spin" />
                      加载…
                    </div>
                  ) : detailError ? <p className="p-3 text-xs text-muted-foreground">详情不可用，请刷新重试。</p> : (
                    <pre
                      className={cn(
                        "p-3 font-mono text-xs break-all whitespace-pre-wrap",
                        selected.status !== "ok" && "text-red-600 dark:text-red-400",
                      )}
                    >
                      <HttpPacketSyntax raw={respText || ""} lineNumbers={false} emptyFallback="（空）" />
                    </pre>
                  )}
                </div>
              </div>
            </div>
          </Card>
        )}
      </div>

      <AlertDialog open={deleteOpen} onOpenChange={setDeleteOpen}>
        <AlertDialogContent>
          <AlertDialogHeader>
            <AlertDialogTitle>删除任务「{pickedTask}」的全部 LLM 对话？</AlertDialogTitle>
            <AlertDialogDescription>
              将永久删除该任务的所有 LLM 调用记录（含请求/响应原文），此操作不可撤销。
            </AlertDialogDescription>
          </AlertDialogHeader>
          {deleteError && <p role="alert" className="text-sm text-destructive">删除失败：{deleteError}</p>}
          <AlertDialogFooter>
            <AlertDialogCancel disabled={deleting}>取消</AlertDialogCancel>
            <AlertDialogAction
              onClick={(e) => {
                e.preventDefault();
                confirmDelete();
              }}
              disabled={deleting}
              className="bg-destructive text-destructive-foreground hover:bg-destructive/90"
            >
              {deleting ? "删除中…" : "确认删除"}
            </AlertDialogAction>
          </AlertDialogFooter>
        </AlertDialogContent>
      </AlertDialog>
    </div>
  );
}
