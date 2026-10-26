"use client";

import * as React from "react";

import { useRouter } from "next/navigation";

import { ActivityIcon, ArrowRightToLineIcon, Loader2Icon, TerminalIcon, Trash2Icon, UnlinkIcon, XIcon } from "lucide-react";
import { toast } from "sonner";

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
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Card } from "@/components/ui/card";
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from "@/components/ui/table";
import { api } from "@/lib/api";
import { safeHref } from "@/lib/safe-href";
import { canUnbindTaskSession } from "@/lib/session-bindings";
import type { ShellSession } from "@/lib/types";
import { cn } from "@/lib/utils";

import { fmtTime } from "./format";

/** 拓扑图点击主机后传下来的过滤条件：host_asset_id 可能是节点 id 也可能是 ip。 */
export interface HostFilter {
  id: string;
  ip: string;
}

function matchesHost(s: ShellSession, f: HostFilter | null): boolean {
  if (!f) return true;
  return s.host_asset_id === f.id || s.host_asset_id === f.ip;
}

function StatusBadge({ status }: { status: string }) {
  const alive = status === "alive";
  return (
    <Badge
      variant={alive ? "secondary" : "outline"}
      className={cn("gap-1.5 text-xs", alive ? "text-emerald-600" : "text-muted-foreground")}
    >
      <span className={cn("size-1.5 rounded-full", alive ? "bg-emerald-500" : "bg-neutral-400")} />
      {alive ? "存活" : "失连"}
    </Badge>
  );
}

// 会话（webshell）台账：对标 webshell 管理器。列表数据与 10s 轮询由页面层负责，
// 这里只管展示、宿主机过滤、打开工作台与删除。
export function SessionsTab({
  selectedTaskId,
  sessions,
  loading,
  hostFilter,
  onClearHostFilter,
  onOpenWorkbench,
  onChanged,
}: {
  selectedTaskId?: string;
  sessions: ShellSession[];
  loading: boolean;
  hostFilter: HostFilter | null;
  onClearHostFilter: () => void;
  onOpenWorkbench: (s: ShellSession) => void;
  onChanged: () => void;
}) {
  const [pendingDelete, setPendingDelete] = React.useState<ShellSession | null>(null);
  const [deleting, setDeleting] = React.useState(false);
  const [pendingUnbind, setPendingUnbind] = React.useState<{ taskId: string; session: ShellSession } | null>(null);
  const [unbinding, setUnbinding] = React.useState(false);
  const [probingId, setProbingId] = React.useState<string | null>(null);
  // 移交内网：确认弹窗目标 + 进行中状态。
  const [handoffTarget, setHandoffTarget] = React.useState<ShellSession | null>(null);
  const [handingOff, setHandingOff] = React.useState(false);
  const router = useRouter();
  const latestChanged = React.useRef(onChanged);
  latestChanged.current = onChanged;

  React.useEffect(() => {
    // A confirmation opened under an older task filter must not act on the new
    // filter. In-flight mutations retain their captured task/session identity.
    setPendingUnbind(null);
    setPendingDelete(null);
    setHandoffTarget(null);
  }, [selectedTaskId]);

  const visible = sessions.filter((s) => matchesHost(s, hostFilter));

  // 人工探活:成功刷新心跳;失败后端已诚实标 dead,这里 toast 原因并刷新列表。
  const probe = (target: ShellSession) => {
    setProbingId(target.id);
    api
      .probeSession(target.id)
      .then((r) => {
        if (r.alive) {
          toast.success(`会话 #${target.id} 探活成功(${r.ms ?? "?"}ms)`);
        } else {
          toast.error(`会话 #${target.id} 已失连,标记 dead:${r.error ?? "探针失败"}`);
        }
        latestChanged.current();
      })
      .catch((e: unknown) => toast.error(e instanceof Error ? e.message : "探活失败"))
      .finally(() => setProbingId(null));
  };

  const confirmDelete = () => {
    const target = pendingDelete;
    if (!target) return;
    setDeleting(true);
    api
      .deleteSession(target.id)
      .then(() => {
        toast.success(`会话 #${target.id} 已删除`);
        setPendingDelete(null);
        latestChanged.current();
      })
      .catch((e: unknown) => toast.error(e instanceof Error ? e.message : "删除失败"))
      .finally(() => setDeleting(false));
  };

  const confirmUnbind = () => {
    const target = pendingUnbind;
    if (!target || unbinding) return;
    if (target.taskId !== selectedTaskId || !canUnbindTaskSession(target.taskId, target.session.created_by_task)) {
      setPendingUnbind(null);
      return;
    }
    setUnbinding(true);
    api.taskUnbindSession(target.taskId, target.session.id)
      .then((result) => {
        toast.success(result.unbound
          ? `已解除任务 #${target.taskId} 对会话 #${target.session.id} 的关联，会话未停止`
          : `任务 #${target.taskId} 已无此显式关联，会话未停止`);
        setPendingUnbind(null);
        latestChanged.current();
      })
      .catch((error: unknown) => toast.error(error instanceof Error ? error.message : "解除关联失败"))
      .finally(() => setUnbinding(false));
  };

  // 移交内网:后端以立足点主机为初始 scope 创建新任务,成功后跳到新任务详情页。
  const confirmHandoff = () => {
    const target = handoffTarget;
    if (!target || handingOff) return;
    setHandingOff(true);
    api
      .handoffSession(target.id)
      .then((r) => {
        toast.success(`已创建内网任务 #${r.task_id}`);
        setHandoffTarget(null);
        router.push(`/function/tasks/detail?id=${encodeURIComponent(r.task_id)}`);
      })
      .catch((e: unknown) => toast.error(e instanceof Error ? e.message : "移交失败"))
      .finally(() => setHandingOff(false));
  };

  return (
    <>
      {hostFilter && (
        <div className="flex items-center gap-2 px-1 pb-2 text-xs">
          <Badge variant="secondary" className="gap-1.5">
            宿主：{hostFilter.ip || hostFilter.id}
            <button type="button" onClick={onClearHostFilter} className="hover:text-foreground" title="清除过滤">
              <XIcon className="size-3" />
            </button>
          </Badge>
          <span className="text-muted-foreground">来自拓扑图点击 · 仅显示该主机的会话</span>
        </div>
      )}
      <Card className="flex min-h-0 flex-1 flex-col overflow-hidden py-0">
        <div className="min-h-0 flex-1 overflow-auto">
          <Table>
            <TableHeader className="sticky top-0 z-10 bg-card">
              <TableRow>
                <TableHead className="w-[70px]">ID</TableHead>
                <TableHead className="w-[90px]">所属任务</TableHead>
                <TableHead className="w-[140px]">马型</TableHead>
                <TableHead>URL</TableHead>
                <TableHead className="w-[130px]">宿主</TableHead>
                <TableHead className="w-[90px]">状态</TableHead>
                <TableHead className="w-[140px]">心跳</TableHead>
                <TableHead className={selectedTaskId ? "w-[440px]" : "w-[300px]"}>操作</TableHead>
              </TableRow>
            </TableHeader>
            <TableBody>
              {loading && visible.length === 0 ? (
                <TableRow>
                  <TableCell colSpan={8} className="py-12 text-center">
                    <Loader2Icon className="mx-auto h-5 w-5 animate-spin text-muted-foreground" />
                  </TableCell>
                </TableRow>
              ) : visible.length === 0 ? (
                <TableRow>
                  <TableCell colSpan={8} className="py-12 text-center text-sm text-muted-foreground">
                    {hostFilter ? "该主机下暂无会话" : "暂无会话"}
                  </TableCell>
                </TableRow>
              ) : (
                visible.map((s) => {
                  // s.url 来自目标侧数据：只允许 http(s) 进 href，否则降级为纯文本。
                  const href = safeHref(s.url);
                  return (
                    <TableRow key={s.id}>
                      <TableCell className="font-mono text-xs text-muted-foreground">#{s.id}</TableCell>
                      <TableCell className="font-mono text-xs text-muted-foreground">
                        {s.created_by_task ? `#${s.created_by_task}` : "—"}
                      </TableCell>
                      <TableCell>
                        <div className="flex items-center gap-1.5">
                          <Badge variant="secondary" className="font-mono text-xs">
                            {s.kind || "-"}
                          </Badge>
                          {s.lang && (
                            <Badge variant="outline" className="font-mono text-xs">
                              {s.lang}
                            </Badge>
                          )}
                        </div>
                      </TableCell>
                      <TableCell className="max-w-0">
                        {href ? (
                          <a
                            href={href}
                            target="_blank"
                            rel="noreferrer"
                            className="block truncate font-mono text-xs text-blue-600 hover:underline dark:text-blue-400"
                            title={s.url}
                          >
                            {s.url}
                          </a>
                        ) : (
                          <code className="block truncate font-mono text-xs" title={s.url}>
                            {s.url || "—"}
                          </code>
                        )}
                      </TableCell>
                      <TableCell>
                        <div className="flex flex-col">
                          <span className="truncate font-mono text-xs" title={s.host_asset_id}>
                            {s.host_asset_id || "—"}
                          </span>
                          {s.platform && <span className="text-[10px] text-muted-foreground">{s.platform}</span>}
                        </div>
                      </TableCell>
                      <TableCell>
                        <StatusBadge status={s.status} />
                      </TableCell>
                      <TableCell className="text-xs text-muted-foreground tabular-nums">
                        {fmtTime(s.last_beat)}
                      </TableCell>
                      <TableCell>
                        <div className="flex flex-wrap items-center gap-1">
                          <Button
                            variant="outline"
                            size="sm"
                            className="h-7 gap-1"
                            title="对目标发无害探针,校验会话存活"
                            disabled={probingId === s.id}
                            onClick={() => probe(s)}
                          >
                            {probingId === s.id ? (
                              <Loader2Icon className="size-3.5 animate-spin" />
                            ) : (
                              <ActivityIcon className="size-3.5" />
                            )}
                            探活
                          </Button>
                          <Button variant="outline" size="sm" className="h-7 gap-1" onClick={() => onOpenWorkbench(s)}>
                            <TerminalIcon className="size-3.5" />
                            工作台
                          </Button>
                          {s.status === "alive" && (
                            <Button
                              variant="outline"
                              size="sm"
                              className="h-7 gap-1"
                              title="以该会话立足点主机为初始 scope 创建内网任务"
                              onClick={() => setHandoffTarget(s)}
                            >
                              <ArrowRightToLineIcon className="size-3.5" />
                              移交内网
                            </Button>
                          )}
                          {canUnbindTaskSession(selectedTaskId, s.created_by_task) && selectedTaskId ? (
                            <Button
                              variant="outline"
                              size="sm"
                              className="h-7 gap-1"
                              title="只解除本任务的显式使用权，不停止或删除共享会话"
                              disabled={unbinding}
                              onClick={() => setPendingUnbind({ taskId: selectedTaskId, session: s })}
                            >
                              <UnlinkIcon className="size-3.5" />
                              解除本任务关联
                            </Button>
                          ) : null}
                          <Button
                            variant="ghost"
                            size="icon"
                            className="size-7 text-muted-foreground hover:text-red-600"
                            title="全局删除会话（影响所有关联任务）"
                            onClick={() => setPendingDelete(s)}
                          >
                            <Trash2Icon className="size-3.5" />
                          </Button>
                        </div>
                      </TableCell>
                    </TableRow>
                  );
                })
              )}
            </TableBody>
          </Table>
        </div>
      </Card>

      <AlertDialog open={handoffTarget !== null} onOpenChange={(o) => !o && setHandoffTarget(null)}>
        <AlertDialogContent>
          <AlertDialogHeader>
            <AlertDialogTitle>移交会话 #{handoffTarget?.id} 到内网任务？</AlertDialogTitle>
            <AlertDialogDescription>
              将创建一个新的内网探索任务，初始测试范围仅包含该会话的立足点主机（
              {handoffTarget?.host_asset_id || "未知"}
              ），后续发现的相邻网段需在任务页批准后才会放行。
            </AlertDialogDescription>
          </AlertDialogHeader>
          <AlertDialogFooter>
            <AlertDialogCancel disabled={handingOff}>取消</AlertDialogCancel>
            <AlertDialogAction
              disabled={handingOff}
              onClick={(e) => {
                e.preventDefault();
                confirmHandoff();
              }}
            >
              {handingOff ? "创建中…" : "确认移交"}
            </AlertDialogAction>
          </AlertDialogFooter>
        </AlertDialogContent>
      </AlertDialog>

      <AlertDialog open={pendingUnbind !== null} onOpenChange={(open) => !open && !unbinding && setPendingUnbind(null)}>
        <AlertDialogContent>
          <AlertDialogHeader>
            <AlertDialogTitle>解除任务 #{pendingUnbind?.taskId} 对会话 #{pendingUnbind?.session.id} 的关联？</AlertDialogTitle>
            <AlertDialogDescription>
              仅解除该任务的显式会话使用权，不停止会话、不删除会话，不影响所属任务或其他任务的使用权。
              解除后该会话会从本任务台账移除；这与“全局删除会话”不同。
            </AlertDialogDescription>
          </AlertDialogHeader>
          <AlertDialogFooter>
            <AlertDialogCancel disabled={unbinding}>取消</AlertDialogCancel>
            <AlertDialogAction disabled={unbinding} onClick={(event) => { event.preventDefault(); confirmUnbind(); }}>
              {unbinding ? "解除中…" : "确认解除关联，不停止会话"}
            </AlertDialogAction>
          </AlertDialogFooter>
        </AlertDialogContent>
      </AlertDialog>

      <AlertDialog open={pendingDelete !== null} onOpenChange={(o) => !o && setPendingDelete(null)}>
        <AlertDialogContent>
          <AlertDialogHeader>
            <AlertDialogTitle>全局删除会话 #{pendingDelete?.id}？</AlertDialogTitle>
            <AlertDialogDescription>
              将断开并从平台移除该会话（{pendingDelete?.kind} · {pendingDelete?.host_asset_id}），影响所有关联任务。
              这不是仅解除当前任务关联；目标上的端点文件不会被清除。此操作不可撤销。
            </AlertDialogDescription>
          </AlertDialogHeader>
          <AlertDialogFooter>
            <AlertDialogCancel disabled={deleting}>取消</AlertDialogCancel>
            <AlertDialogAction
              variant="destructive"
              disabled={deleting}
              onClick={(e) => {
                e.preventDefault();
                confirmDelete();
              }}
            >
              {deleting ? "删除中…" : "确认全局删除"}
            </AlertDialogAction>
          </AlertDialogFooter>
        </AlertDialogContent>
      </AlertDialog>
    </>
  );
}
