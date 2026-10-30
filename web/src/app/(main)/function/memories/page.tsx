"use client";

import * as React from "react";
import Link from "next/link";

import { BrainIcon, HistoryIcon, PencilIcon, PlusIcon, RefreshCwIcon, SearchIcon, Trash2Icon } from "lucide-react";
import { toast } from "sonner";

import { MemorySettingsCard } from "@/components/memory-settings-card";
import { MemoryUsagePanel } from "@/components/memory-usage-button";
import { AlertDialog, AlertDialogAction, AlertDialogCancel, AlertDialogContent, AlertDialogDescription, AlertDialogFooter, AlertDialogHeader, AlertDialogTitle } from "@/components/ui/alert-dialog";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Card, CardContent } from "@/components/ui/card";
import { Dialog, DialogContent, DialogDescription, DialogFooter, DialogHeader, DialogTitle } from "@/components/ui/dialog";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from "@/components/ui/select";
import { Sheet, SheetContent, SheetDescription, SheetFooter, SheetHeader, SheetTitle } from "@/components/ui/sheet";
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from "@/components/ui/table";
import { Tabs, TabsContent, TabsList, TabsTrigger } from "@/components/ui/tabs";
import { Textarea } from "@/components/ui/textarea";
import { memoryAPI } from "@/lib/memory-api";
import { MEMORY_KIND_LABELS, MEMORY_STATUS_LABELS, memoryConflict, memorySourceHref, memorySourceLabel, memoryTime, validateMemoryEntryInput, type MemoryEntry, type MemoryEntryInput, type MemoryKind, type MemoryQuery, type MemorySettings, type MemoryStatus } from "@/lib/memory-types";

const PAGE_SIZE = 20;
type Form = { title: string; content: string; kind: MemoryKind; target: string; applicability: string; tags: string; expires_at: string };
const emptyForm = (): Form => ({ title: "", content: "", kind: "lesson", target: "", applicability: "", tags: "", expires_at: "" });

function dateInput(value?: string | null): string {
  if (!value || !Number.isFinite(Date.parse(value))) return "";
  const date = new Date(value);
  return new Date(date.getTime() - date.getTimezoneOffset() * 60_000).toISOString().slice(0, 16);
}

function entryForm(entry: MemoryEntry): Form {
  return { title: entry.title, content: entry.content, kind: entry.kind, target: entry.target || "", applicability: entry.applicability || "", tags: (entry.tags || []).join(", "), expires_at: dateInput(entry.expires_at) };
}

function statusClass(status: MemoryStatus) {
  if (status === "active") return "border-emerald-500/40 text-emerald-700 dark:text-emerald-300";
  if (status === "source_invalid") return "border-red-500/40 text-red-700 dark:text-red-300";
  return "text-muted-foreground";
}

export default function MemoriesPage() {
  const [items, setItems] = React.useState<MemoryEntry[]>([]);
  const [total, setTotal] = React.useState<number | null>(null);
  const [loading, setLoading] = React.useState(true);
  const [listError, setListError] = React.useState("");
  const [selectedError, setSelectedError] = React.useState("");
  const [query, setQuery] = React.useState<MemoryQuery>({ limit: PAGE_SIZE, offset: 0 });
  const [search, setSearch] = React.useState("");
  const [taskFilter, setTaskFilter] = React.useState("");
  const [filterError, setFilterError] = React.useState("");
  const [refresh, setRefresh] = React.useState(0);
  const [tab, setTab] = React.useState("entries");
  const [settings, setSettings] = React.useState<MemorySettings | null>(null);
  const [settingsError, setSettingsError] = React.useState("");
  const [editorOpen, setEditorOpen] = React.useState(false);
  const [editing, setEditing] = React.useState<MemoryEntry | null>(null);
  const [form, setForm] = React.useState<Form>(emptyForm);
  const [editError, setEditError] = React.useState("");
  const [editConflict, setEditConflict] = React.useState(false);
  const [deleting, setDeleting] = React.useState<MemoryEntry | null>(null);
  const [deleteError, setDeleteError] = React.useState("");
  const [extractOpen, setExtractOpen] = React.useState(false);
  const [extractTask, setExtractTask] = React.useState("");
  const [extractError, setExtractError] = React.useState("");
  const [busy, setBusy] = React.useState(false);
  const busyRef = React.useRef(false);
  const mounted = React.useRef(false);
  const editorGeneration = React.useRef(0);
  const readOnlyEntry = editing?.status === "source_invalid";
  const editorLocked = busy || editConflict || readOnlyEntry;

  React.useEffect(() => {
    mounted.current = true;
    const controller = new AbortController();
    void memoryAPI.settings(controller.signal).then((result) => { if (!controller.signal.aborted) setSettings(result); }).catch((cause) => {
      if (!controller.signal.aborted) setSettingsError(`记忆状态读取失败：${(cause as Error).message}`);
    });
    const id = Number(new URLSearchParams(window.location.search).get("entry_id"));
    if (Number.isSafeInteger(id) && id > 0) {
      void memoryAPI.get(id).then((entry) => {
        if (!mounted.current) return;
        setEditing(entry); setForm(entryForm(entry)); setEditorOpen(true);
      }).catch((cause) => { if (mounted.current) setSelectedError(`所选记忆不可用：${(cause as Error).message}`); });
    }
    return () => { mounted.current = false; editorGeneration.current += 1; controller.abort(); };
  }, []);

  React.useEffect(() => {
    const controller = new AbortController();
    setLoading(true); setListError("");
    void memoryAPI.list(query, controller.signal).then((result) => {
      if (!controller.signal.aborted) {
        setItems(result.items); setTotal(result.total);
        if ((query.offset ?? 0) > 0 && (query.offset ?? 0) >= result.total) {
          setQuery((previous) => ({ ...previous, offset: result.total > 0 ? Math.floor((result.total - 1) / PAGE_SIZE) * PAGE_SIZE : 0 }));
        }
      }
    }).catch((cause) => {
      if (!controller.signal.aborted) { setTotal(null); setListError(`记忆列表读取失败：${(cause as Error).message}`); }
    }).finally(() => { if (!controller.signal.aborted) setLoading(false); });
    return () => controller.abort();
  }, [query, refresh]);

  const applySearch = (event: React.FormEvent) => {
    event.preventDefault();
    const id = taskFilter.trim() ? Number(taskFilter) : undefined;
    if (id !== undefined && (!/^\d+$/.test(taskFilter.trim()) || !Number.isSafeInteger(id) || id <= 0)) {
      setFilterError("来源任务 ID 须为正整数"); return;
    }
    setFilterError(""); setQuery((previous) => ({ ...previous, q: search.trim(), task_id: id, offset: 0 }));
  };

  const openEditor = (entry: MemoryEntry | null) => {
    editorGeneration.current += 1;
    setEditing(entry); setForm(entry ? entryForm(entry) : emptyForm()); setEditError(""); setEditConflict(false); setEditorOpen(true);
  };

  const startMutation = () => {
    if (busyRef.current) return false;
    busyRef.current = true; setBusy(true); return true;
  };
  const finishMutation = () => { busyRef.current = false; if (mounted.current) setBusy(false); };
  const reload = () => setRefresh((value) => value + 1);

  const save = async () => {
    if (readOnlyEntry || editConflict || !form.title.trim() || !form.content.trim() || !startMutation()) return;
    const generation = editorGeneration.current;
    setEditError("");
    try {
      const input: MemoryEntryInput = {
        title: form.title.trim(), content: form.content.trim(), kind: form.kind, target: form.target.trim(), applicability: form.applicability.trim(),
        tags: [...new Set(form.tags.split(/[,，\n]/u).map((tag) => tag.trim()).filter(Boolean))],
        expires_at: form.expires_at ? new Date(form.expires_at).toISOString() : null,
      };
      validateMemoryEntryInput(input);
      if (input.expires_at && Date.parse(input.expires_at) <= Date.now()) throw new Error("有效期须晚于当前时间，或留空清除单独有效期");
      if (editing) await memoryAPI.update(editing.id, { ...input, version: editing.version, ...(editing.status === "expired" ? { status: "active" as const } : {}) }); else await memoryAPI.create(input);
      if (!mounted.current || editorGeneration.current !== generation) return;
      setEditorOpen(false); reload(); toast.success(editing ? "记忆已更新" : "记忆已保存，无需审批");
    } catch (cause) {
      if (!mounted.current || editorGeneration.current !== generation) return;
      const collided = memoryConflict(cause); setEditConflict(collided);
      setEditError(collided ? "条目已被其他操作更新，本次未覆盖。请重新载入最新条目后再编辑。" : `保存失败：${(cause as Error).message}`);
      if (collided) reload();
    } finally { finishMutation(); }
  };

  const reloadEditing = async () => {
    if (!editing || !startMutation()) return;
    const generation = editorGeneration.current;
    try {
      const entry = await memoryAPI.get(editing.id);
      if (!mounted.current || editorGeneration.current !== generation) return;
      setEditing(entry); setForm(entryForm(entry)); setEditConflict(false); setEditError("已载入最新条目，请重新确认编辑内容。");
    } catch (cause) { if (mounted.current && editorGeneration.current === generation) setEditError(`重新读取失败：${(cause as Error).message}`); }
    finally { finishMutation(); }
  };

  const toggle = async (entry: MemoryEntry) => {
    if (!startMutation()) return;
    try {
      await memoryAPI.update(entry.id, { version: entry.version, status: entry.status === "active" ? "disabled" : "active" });
      if (mounted.current) { reload(); toast.success(entry.status === "active" ? "已停用，停止后续召回" : "已启用记忆"); }
    } catch (cause) { if (mounted.current) { toast.error(memoryConflict(cause) ? "条目版本已变化，已刷新列表，请重新操作" : (cause as Error).message); reload(); } }
    finally { finishMutation(); }
  };

  const remove = async () => {
    if (!deleting || !startMutation()) return;
    setDeleteError("");
    try {
      const result = await memoryAPI.remove(deleting.id, deleting.version);
      if (!result.deleted) throw new Error("服务未确认删除结果");
      if (mounted.current) { setDeleting(null); reload(); toast.success("记忆已删除，历史业务记录仍保留"); }
    } catch (cause) {
      if (mounted.current) {
        if (memoryConflict(cause)) { setDeleting(null); reload(); toast.error("条目版本已变化，已刷新列表，请重新确认删除"); }
        else setDeleteError(`删除失败：${(cause as Error).message}`);
      }
    } finally { finishMutation(); }
  };

  const extract = async () => {
    const id = extractTask.trim() ? Number(extractTask) : undefined;
    if (id !== undefined && (!/^\d+$/.test(extractTask.trim()) || !Number.isSafeInteger(id) || id <= 0)) { setExtractError("任务 ID 须为正整数，或留空回顾全局来源"); return; }
    if (!startMutation()) return;
    setExtractError("");
    try {
      const result = await memoryAPI.extract(id);
      if (!Number.isSafeInteger(result.queued) || result.queued < 0) throw new Error("服务未确认排队结果");
      if (mounted.current) { setExtractOpen(false); toast.success(result.queued > 0 ? `已排队 ${result.queued} 个来源，后台按预算学习` : "本批没有可排队的来源，已有来源不会重复学习"); }
    } catch (cause) { if (mounted.current) setExtractError(`回顾请求失败：${(cause as Error).message}`); }
    finally { finishMutation(); }
  };

  const offset = query.offset ?? 0;
  const extractionAvailable = !!settings?.enabled && settings.auto_extract && settings.model_available === true && !settingsError;

  return (
    <div className="flex min-w-0 flex-1 flex-col gap-5 p-4 md:p-6">
      <div className="flex flex-wrap items-start justify-between gap-3">
        <div><h1 className="flex items-center gap-2 text-xl font-semibold"><BrainIcon className="size-5" />记忆中心</h1><p className="text-muted-foreground mt-1 text-sm">所有任务积累经验，按相关性自动召回；记忆不是系统指令，也不等于已确认漏洞。</p></div>
        <div className="flex flex-wrap gap-2"><Button variant="outline" disabled={busy || !extractionAvailable} onClick={() => { setExtractTask(taskFilter); setExtractError(""); setExtractOpen(true); }}><HistoryIcon className="size-4" />主动回顾历史</Button><Button disabled={busy} onClick={() => openEditor(null)}><PlusIcon className="size-4" />记住这个</Button></div>
      </div>
      {settingsError ? <p role="alert" className="text-destructive text-sm">{settingsError}。请在设置页签重新读取，未知状态不会用于发起提炼。</p> : settings?.model_available === false ? <p role="alert" className="text-destructive text-sm">自动提炼不可用：{settings.model_error || "尚未配置可用模型"}。<Link className="underline" href="/system/llm">前往 LLM 设置</Link></p> : settings && !settings.enabled ? <p role="status" className="text-muted-foreground text-sm">记忆已关闭：停止新的学习与召回，仍可管理已有条目。</p> : settings && !settings.auto_extract ? <p role="status" className="text-muted-foreground text-sm">自动提炼已关闭，已有记忆仍可召回；主动回顾历史前请先在设置中开启自动提炼。</p> : null}
      {selectedError ? <p role="alert" className="text-destructive text-sm">{selectedError}</p> : null}
      <Tabs value={tab} onValueChange={setTab}>
        <TabsList><TabsTrigger value="entries">记忆条目</TabsTrigger><TabsTrigger value="usage">使用记录</TabsTrigger><TabsTrigger value="settings">记忆设置</TabsTrigger></TabsList>
        <TabsContent value="entries" className="mt-4 space-y-4">
          <form onSubmit={applySearch} className="flex flex-wrap items-center gap-2">
            <Input aria-label="搜索记忆" placeholder="搜索标题、内容或标签" value={search} maxLength={128} onChange={(event) => setSearch(event.target.value)} className="min-w-40 flex-1" />
            <Input aria-label="来源任务 ID" placeholder="来源任务 ID" inputMode="numeric" value={taskFilter} onChange={(event) => setTaskFilter(event.target.value)} className="w-36" />
            <Select value={query.kind || "all"} onValueChange={(value) => setQuery((previous) => ({ ...previous, kind: value === "all" ? "" : value as MemoryKind, offset: 0 }))}><SelectTrigger aria-label="记忆类型" className="w-36"><SelectValue /></SelectTrigger><SelectContent><SelectItem value="all">所有类型</SelectItem>{Object.entries(MEMORY_KIND_LABELS).map(([value, label]) => <SelectItem key={value} value={value}>{label}</SelectItem>)}</SelectContent></Select>
            <Select value={query.status || "all"} onValueChange={(value) => setQuery((previous) => ({ ...previous, status: value === "all" ? "" : value as MemoryStatus, offset: 0 }))}><SelectTrigger aria-label="记忆状态" className="w-32"><SelectValue /></SelectTrigger><SelectContent><SelectItem value="all">所有状态</SelectItem>{Object.entries(MEMORY_STATUS_LABELS).map(([value, label]) => <SelectItem key={value} value={value}>{label}</SelectItem>)}</SelectContent></Select>
            <Button type="submit" variant="outline"><SearchIcon className="size-4" />搜索</Button><Button type="button" size="icon" variant="outline" aria-label="刷新记忆列表" disabled={loading} onClick={reload}><RefreshCwIcon className="size-4" /></Button>
          </form>
          {filterError ? <p role="alert" className="text-destructive text-sm">{filterError}</p> : null}
          <Card><CardContent className="p-0">
            {loading ? <p role="status" className="text-muted-foreground p-6 text-sm">正在读取记忆…</p> : listError ? <div role="alert" className="space-y-2 p-6"><p className="text-destructive text-sm">{listError}</p><Button variant="outline" size="sm" onClick={reload}>重试读取</Button></div> : items.length === 0 ? <div className="text-muted-foreground space-y-2 p-10 text-center text-sm"><p>暂无符合条件的记忆。</p><p>新来源会按自动学习设置处理；也可主动回顾历史或手动记住。</p></div> : (
              <Table><TableHeader><TableRow><TableHead>记忆内容</TableHead><TableHead>类型与状态</TableHead><TableHead>来源与条件</TableHead><TableHead>使用情况</TableHead><TableHead>操作</TableHead></TableRow></TableHeader><TableBody>{items.map((entry) => {
                const href = memorySourceHref(entry);
                return <TableRow key={entry.id}>
                  <TableCell className="min-w-56 max-w-md whitespace-normal"><button type="button" className="text-left font-medium underline-offset-4 hover:underline" onClick={() => openEditor(entry)}>{entry.title}</button><p className="text-muted-foreground mt-1 line-clamp-3 whitespace-pre-wrap break-words text-xs">{entry.content}</p>{entry.tags?.length ? <div className="mt-2 flex flex-wrap gap-1">{entry.tags.map((tag) => <Badge key={tag} variant="outline" className="max-w-40 truncate text-[10px]">{tag}</Badge>)}</div> : null}</TableCell>
                  <TableCell className="space-y-1"><Badge variant="outline">{MEMORY_KIND_LABELS[entry.kind] ?? entry.kind}</Badge><br /><Badge variant="outline" className={statusClass(entry.status)}>{MEMORY_STATUS_LABELS[entry.status] ?? entry.status}</Badge><p className="text-muted-foreground text-xs">{entry.confidence === "observed" ? "已观察 · 仍须核对" : "待验证参考"}</p><p className="text-muted-foreground text-xs">{entry.automatic ? "自动整理" : "人工记忆"} · v{entry.version}</p></TableCell>
                  <TableCell className="max-w-60 whitespace-normal text-xs">{href ? <Link href={href} className="underline underline-offset-4">{memorySourceLabel(entry)}</Link> : <span className="text-muted-foreground">{memorySourceLabel(entry)}</span>}<p className="mt-1 break-all">目标：{entry.target || "通用经验"}</p><p className="text-muted-foreground mt-1 line-clamp-2 break-words">{entry.applicability || "使用前仍须核对当前环境"}</p><p className="text-muted-foreground mt-1">有效至：{entry.expires_at ? memoryTime(entry.expires_at) : "未单独设置"}</p></TableCell>
                  <TableCell className="text-xs"><p>{entry.use_count ?? 0} 次</p><p className="text-muted-foreground mt-1">{memoryTime(entry.last_used_at)}</p></TableCell>
                  <TableCell><div className="flex flex-wrap gap-1"><Button type="button" size="icon-sm" variant="ghost" aria-label={`编辑记忆 ${entry.id}`} disabled={busy} onClick={() => openEditor(entry)}><PencilIcon className="size-4" /></Button>{["active", "disabled"].includes(entry.status) ? <Button type="button" size="sm" variant="ghost" disabled={busy} onClick={() => void toggle(entry)}>{entry.status === "active" ? "停用" : "启用"}</Button> : null}<Button type="button" size="icon-sm" variant="ghost" className="text-destructive" aria-label={`删除记忆 ${entry.id}`} disabled={busy} onClick={() => { setDeleteError(""); setDeleting(entry); }}><Trash2Icon className="size-4" /></Button></div></TableCell>
                </TableRow>;
              })}</TableBody></Table>
            )}
          </CardContent></Card>
          <div className="flex items-center justify-between gap-2 text-sm"><span className="text-muted-foreground">{total === null ? "数量未确认" : `共 ${total} 条 · 第 ${Math.floor(offset / PAGE_SIZE) + 1} 页`}</span><div className="flex gap-2"><Button variant="outline" size="sm" disabled={loading || offset === 0} onClick={() => setQuery((previous) => ({ ...previous, offset: Math.max(0, offset - PAGE_SIZE) }))}>上一页</Button><Button variant="outline" size="sm" disabled={loading || total === null || offset + PAGE_SIZE >= total} onClick={() => setQuery((previous) => ({ ...previous, offset: offset + PAGE_SIZE }))}>下一页</Button></div></div>
        </TabsContent>
        <TabsContent value="usage" className="mt-4"><MemoryUsagePanel query={{ limit: 50 }} /></TabsContent>
        <TabsContent value="settings" className="mt-4"><MemorySettingsCard onChange={(result) => { setSettings(result); setSettingsError(""); }} /></TabsContent>
      </Tabs>

      <Sheet open={editorOpen} onOpenChange={(open) => { if (!busy) { setEditorOpen(open); if (!open) editorGeneration.current += 1; } }}>
        <SheetContent className="flex w-full flex-col overflow-y-auto sm:max-w-xl">
          <SheetHeader><SheetTitle>{readOnlyEntry ? `查看失效记忆 #${editing?.id}` : editing ? `编辑记忆 #${editing.id}` : "记住这个"}</SheetTitle><SheetDescription>保存后按设置自动生效，无需审批。填写经验与适用条件，不填写密码、Cookie、私钥或原始流量。</SheetDescription></SheetHeader>
          <form className="flex min-h-0 flex-1 flex-col gap-4 px-4 pb-4" onSubmit={(event) => { event.preventDefault(); void save(); }}>
            <div className="space-y-2"><Label htmlFor="memory-title">标题</Label><Input id="memory-title" value={form.title} maxLength={160} disabled={editorLocked} onChange={(event) => setForm((previous) => ({ ...previous, title: event.target.value }))} required /></div>
            <div className="space-y-2"><Label htmlFor="memory-content">记忆正文</Label><Textarea id="memory-content" value={form.content} rows={7} maxLength={4800} disabled={editorLocked} onChange={(event) => setForm((previous) => ({ ...previous, content: event.target.value }))} required /><p className="text-muted-foreground text-xs">正文最多 4,800 个 UTF-8 字节，约 1,600 个汉字；长详情留在原来源，不整段复制历史。</p></div>
            <div className="space-y-2"><Label htmlFor="memory-kind">类型</Label><Select value={form.kind} disabled={editorLocked} onValueChange={(value) => setForm((previous) => ({ ...previous, kind: value as MemoryKind }))}><SelectTrigger id="memory-kind"><SelectValue /></SelectTrigger><SelectContent>{Object.entries(MEMORY_KIND_LABELS).map(([value, label]) => <SelectItem key={value} value={value}>{label}</SelectItem>)}</SelectContent></Select></div>
            <div className="space-y-2"><Label htmlFor="memory-target">适用目标（留空为通用经验）</Label><Input id="memory-target" value={form.target} maxLength={500} disabled={editorLocked} onChange={(event) => setForm((previous) => ({ ...previous, target: event.target.value }))} /></div>
            <div className="space-y-2"><Label htmlFor="memory-applicability">适用条件</Label><Textarea id="memory-applicability" value={form.applicability} rows={3} maxLength={2000} disabled={editorLocked} onChange={(event) => setForm((previous) => ({ ...previous, applicability: event.target.value }))} placeholder="例如：操作系统、工具版本、已验证条件和限制" /></div>
            <div className="space-y-2"><Label htmlFor="memory-tags">标签（逗号分隔）</Label><Input id="memory-tags" value={form.tags} maxLength={1000} disabled={editorLocked} onChange={(event) => setForm((previous) => ({ ...previous, tags: event.target.value }))} /></div>
            <div className="space-y-2"><Label htmlFor="memory-expiry">有效至（可选，使用本机时区）</Label><Input id="memory-expiry" type="datetime-local" value={form.expires_at} disabled={editorLocked} onChange={(event) => setForm((previous) => ({ ...previous, expires_at: event.target.value }))} /></div>
            {editing ? <div className="text-muted-foreground space-y-1 rounded-md border p-3 text-xs"><p>来源：{memorySourceLabel(editing)} · {editing.source_kind || "manual"} {editing.source_id || ""} · 来源版本 {editing.source_version}</p><p>创建：{memoryTime(editing.created_at)} · 更新：{memoryTime(editing.updated_at)}</p><p>记忆仅供参考，原漏洞的验证状态和任务授权保持独立。</p></div> : null}
            {editError ? <p role="alert" className="text-destructive break-words text-sm">{editError}</p> : null}
            <SheetFooter className="mt-auto px-0">{readOnlyEntry ? <p className="text-muted-foreground text-sm">来源已失效，本条仅供历史查看，不会再次召回或自动复活。</p> : <Button type="submit" disabled={editorLocked || !form.title.trim() || !form.content.trim()}>{busy ? "处理中…" : editing?.status === "expired" ? "保存并重新启用" : "保存记忆"}</Button>}{editConflict ? <Button type="button" variant="outline" disabled={busy} onClick={() => void reloadEditing()}>重新载入最新条目</Button> : null}</SheetFooter>
          </form>
        </SheetContent>
      </Sheet>

      <AlertDialog open={!!deleting} onOpenChange={(open) => { if (!open && !busy) setDeleting(null); }}><AlertDialogContent><AlertDialogHeader><AlertDialogTitle>删除这条记忆？</AlertDialogTitle><AlertDialogDescription>将删除“{deleting?.title}”并停止后续召回。不会删除原始任务证据、历史聊天、检查点、旧导出或已发送给模型的请求。</AlertDialogDescription></AlertDialogHeader>{deleteError ? <p role="alert" className="text-destructive text-sm">{deleteError}</p> : null}<AlertDialogFooter><AlertDialogCancel disabled={busy}>取消</AlertDialogCancel><AlertDialogAction variant="destructive" disabled={busy} onClick={(event) => { event.preventDefault(); void remove(); }}>{busy ? "删除中…" : "确认删除记忆"}</AlertDialogAction></AlertDialogFooter></AlertDialogContent></AlertDialog>

      <Dialog open={extractOpen} onOpenChange={(open) => { if (!busy) setExtractOpen(open); }}><DialogContent><DialogHeader><DialogTitle>主动回顾历史来源</DialogTitle><DialogDescription>会把经过筛选的来源发送给记忆设置所选模型。每批最多扫描 50 个来源，按小时预算排队，不会一次导入所有历史记录。</DialogDescription></DialogHeader><div className="space-y-2"><Label htmlFor="memory-extract-task">来源任务 ID（留空为全局）</Label><Input id="memory-extract-task" inputMode="numeric" value={extractTask} disabled={busy} onChange={(event) => setExtractTask(event.target.value)} /></div><p className="text-muted-foreground text-xs">自动学习起点：{memoryTime(settings?.learn_after)}。此操作仅排队，完成时间受模型和预算影响。</p>{extractError ? <p role="alert" className="text-destructive text-sm">{extractError}</p> : null}<DialogFooter><Button variant="outline" disabled={busy} onClick={() => setExtractOpen(false)}>取消</Button><Button disabled={busy || !extractionAvailable} onClick={() => void extract()}>{busy ? "排队中…" : "确认回顾并排队"}</Button></DialogFooter></DialogContent></Dialog>
    </div>
  );
}
