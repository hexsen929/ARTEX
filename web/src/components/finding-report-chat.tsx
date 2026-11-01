"use client";

import * as React from "react";
import { Loader2Icon, SquareIcon } from "lucide-react";

import { Transcript } from "@/components/transcript";
import { ReportScreenshotContext } from "@/components/markdown";
import { Alert, AlertDescription } from "@/components/ui/alert";
import { Button } from "@/components/ui/button";
import { Dialog, DialogContent, DialogDescription, DialogHeader, DialogTitle } from "@/components/ui/dialog";
import { Textarea } from "@/components/ui/textarea";
import { api, http } from "@/lib/api";
import type { Activity } from "@/lib/types";

function mergeActivities(previous: Activity[], incoming: Activity[]) {
  const byID = new Map(previous.map((item) => [item.seq, item]));
  for (const item of incoming) byID.set(item.seq, item);
  return [...byID.values()].sort((left, right) => left.seq - right.seq);
}

// Report editing is an executable, server-bound reporter conversation. Keep it
// separate from FindingAutoChat's task-bound, tool-free side questions. Neither
// titles, local storage nor the DTO's cached conversation ID grant a binding.
export function FindingReportChat({
  taskId,
  findingId,
  findingTitle,
  open,
  onOpenChange,
  onReportUpdated,
}: {
  taskId: string;
  findingId: string;
  findingTitle: string;
  open: boolean;
  onOpenChange: (open: boolean) => void;
  onReportUpdated: () => void;
}) {
  const [conversationId, setConversationId] = React.useState<number | null>(null);
  const [messages, setMessages] = React.useState<Activity[]>([]);
  const [input, setInput] = React.useState("");
  const [loading, setLoading] = React.useState(false);
  const [sending, setSending] = React.useState(false);
  const [stopping, setStopping] = React.useState(false);
  const [running, setRunning] = React.useState(false);
  const [hasMore, setHasMore] = React.useState(false);
  const [loadingOlder, setLoadingOlder] = React.useState(false);
  const [error, setError] = React.useState("");
  const [reload, setReload] = React.useState(0);
  const cursor = React.useRef(0);
  const generation = React.useRef(0);
  const viewport = React.useRef<HTMLDivElement>(null);
  const restoredFor = React.useRef<number | null>(null);
  const scopeKey = JSON.stringify([findingId, taskId, open]);
  const currentScope = React.useRef(scopeKey);
  currentScope.current = scopeKey;
  const reportUpdated = React.useRef(onReportUpdated);
  reportUpdated.current = onReportUpdated;

  React.useEffect(() => { setInput(""); }, [findingId, taskId]);

  React.useEffect(() => {
    if (!open) return;
    const scope = scopeKey;
    const request = ++generation.current;
    const current = () => currentScope.current === scope && generation.current === request;
    setLoading(true);
    setError("");
    setConversationId(null);
    setMessages([]);
    setRunning(false);
    setSending(false);
    setStopping(false);
    setHasMore(false);
    setLoadingOlder(false);
    restoredFor.current = null;
    void (async () => {
      try {
        const binding = await http<{ conversation_id: number; task_id: number; finding_id: number }>(
          `/exploration/findings/${encodeURIComponent(findingId)}/report-conversation`,
          { method: "POST", body: "{}" },
        );
        if (!current()) return;
        if (!Number.isSafeInteger(binding.conversation_id) || binding.conversation_id <= 0 ||
            String(binding.finding_id) !== findingId || String(binding.task_id) !== taskId) {
          throw new Error("报告会话归属与当前发现不一致，未加载或执行对话");
        }
        const history = await api.conversationHistory(binding.conversation_id, 0, 200);
        if (!current()) return;
        cursor.current = history.cursor;
        setMessages(history.items);
        setHasMore(history.hasMore);
        setRunning(history.running);
        setConversationId(binding.conversation_id);
      } catch (reason) {
        if (current()) setError(`打开报告对话失败：${(reason as Error).message}`);
      } finally {
        if (current()) setLoading(false);
      }
    })();
    return () => { generation.current++; };
  }, [findingId, open, reload, scopeKey, taskId]);

  React.useEffect(() => {
    if (!open || !conversationId || !running) return;
    let live = true;
    const scope = scopeKey;
    const request = generation.current;
    let timer: ReturnType<typeof setTimeout>;
    const poll = async () => {
      try {
        const next = await api.conversationMessages(conversationId, cursor.current);
        if (!live || currentScope.current !== scope || generation.current !== request) return;
        setMessages((previous) => mergeActivities(previous, next.items));
        cursor.current = Math.max(cursor.current, next.cursor);
        setRunning(next.running);
        setError("");
        if (next.running) timer = setTimeout(() => void poll(), 1000);
        else reportUpdated.current();
      } catch (reason) {
        if (!live || currentScope.current !== scope || generation.current !== request) return;
        setError(`读取报告进度失败：${(reason as Error).message}；不会自动重发调整要求。`);
        timer = setTimeout(() => void poll(), 2000);
      }
    };
    void poll();
    return () => { live = false; clearTimeout(timer); };
  }, [conversationId, open, running, scopeKey]);

  React.useEffect(() => {
    const element = viewport.current;
    if (!open || !conversationId || !element || loading) return;
    const first = restoredFor.current !== conversationId;
    const nearBottom = element.scrollHeight - element.clientHeight - element.scrollTop < 100;
    let saved: string | null = null;
    try { saved = window.sessionStorage.getItem(`artex.report-conversation-scroll.${conversationId}`); } catch { /* Optional UI preference. */ }
    const frame = requestAnimationFrame(() => {
      if (first) {
        const position = saved === null ? NaN : Number(saved);
        element.scrollTop = Number.isFinite(position) && position >= 0 ? position : element.scrollHeight;
        restoredFor.current = conversationId;
      } else if (nearBottom) element.scrollTop = element.scrollHeight;
    });
    return () => cancelAnimationFrame(frame);
  }, [conversationId, loading, messages.length, open]);

  const send = async () => {
    const message = input.trim();
    if (!conversationId || !message || loading || sending || running || error) return;
    const scope = scopeKey;
    const request = generation.current;
    setSending(true);
    setInput("");
    try {
      await api.sendConversationMessage(conversationId, message);
      if (currentScope.current === scope && generation.current === request) setRunning(true);
    } catch (reason) {
      if (currentScope.current === scope && generation.current === request) {
        setInput(message);
        setError(`发送结果未确认：${(reason as Error).message}。请重新读取会话核对后再决定是否重试；不会自动重发。`);
      }
    } finally {
      if (currentScope.current === scope && generation.current === request) setSending(false);
    }
  };
  const stop = async () => {
    if (!conversationId || !running || stopping) return;
    const scope = scopeKey;
    const request = generation.current;
    setStopping(true);
    try { await api.stopConversation(conversationId); }
    catch (reason) { if (currentScope.current === scope && generation.current === request) setError(`停止失败：${(reason as Error).message}`); }
    finally { if (currentScope.current === scope && generation.current === request) setStopping(false); }
  };
  const loadOlder = async () => {
    if (!conversationId || !hasMore || loadingOlder || !messages.length) return;
    const scope = scopeKey;
    const request = generation.current;
    const oldHeight = viewport.current?.scrollHeight ?? 0;
    setLoadingOlder(true);
    try {
      const page = await api.conversationHistory(conversationId, messages[0].seq, 200);
      if (currentScope.current !== scope || generation.current !== request) return;
      setMessages((previous) => mergeActivities(previous, page.items));
      setHasMore(page.hasMore);
      requestAnimationFrame(() => {
        if (currentScope.current === scope && generation.current === request && viewport.current) viewport.current.scrollTop += viewport.current.scrollHeight - oldHeight;
      });
    } catch (reason) { if (currentScope.current === scope && generation.current === request) setError(`读取历史失败：${(reason as Error).message}`); }
    finally { if (currentScope.current === scope && generation.current === request) setLoadingOlder(false); }
  };

  return (
    <Dialog open={open} onOpenChange={onOpenChange}>
      <DialogContent className="flex h-[min(760px,calc(100svh-2rem))] min-h-0 flex-col overflow-hidden sm:max-w-[760px]">
        <DialogHeader className="shrink-0 pr-6">
          <DialogTitle>调整漏洞报告</DialogTitle>
          <DialogDescription>
            任务 #{taskId} · 发现 #{findingId} · {findingTitle} · 报告撰写 Agent
            {conversationId ? ` · 会话 #${conversationId}` : ""}
          </DialogDescription>
        </DialogHeader>
        <p className="text-muted-foreground text-xs">复用后端核实的报告会话；发送后可调用已授权的报告工具，不属于 Auto 只读旁路。</p>
        {error ? <Alert variant="destructive"><AlertDescription>{error}</AlertDescription></Alert> : null}
        {error ? <Button variant="outline" size="sm" className="self-start" onClick={() => setReload((value) => value + 1)} disabled={loading || sending}>重新读取报告会话</Button> : null}
        <div ref={viewport} className="min-h-0 flex-1 overflow-y-auto" onScroll={(event) => {
          if (!conversationId) return;
          try { window.sessionStorage.setItem(`artex.report-conversation-scroll.${conversationId}`, String(event.currentTarget.scrollTop)); } catch { /* Optional UI preference. */ }
        }}>
          {hasMore ? <Button variant="ghost" size="sm" onClick={() => void loadOlder()} disabled={loadingOlder}>加载更早报告历史</Button> : null}
          {loading ? <p className="flex items-center gap-2 py-8 text-muted-foreground"><Loader2Icon className="size-4 animate-spin" />正在核实报告会话并读取历史…</p> : null}
          {!loading && conversationId && !messages.length ? <p className="py-8 text-muted-foreground text-sm">尚无报告对话内容。填写调整要求后才会运行。</p> : null}
          {!loading && conversationId && messages.length ? <ReportScreenshotContext.Provider value={findingId}><Transcript activity={messages} chat live={running} fetchDetail={(seq) => api.conversationMsgDetail(conversationId, seq)} /></ReportScreenshotContext.Provider> : null}
        </div>
        <div className="shrink-0 space-y-2 border-t pt-3">
          <Textarea aria-label="报告调整要求" placeholder="描述要如何调整当前发现的报告…" value={input} onChange={(event) => setInput(event.target.value)} disabled={!conversationId || loading || sending || running || !!error} rows={3} />
          <div className="flex items-center justify-end gap-2">
            {running ? <Button variant="destructive" size="sm" onClick={() => void stop()} disabled={stopping}><SquareIcon />停止报告撰写</Button> : <Button size="sm" onClick={() => void send()} disabled={!conversationId || loading || sending || !input.trim() || !!error}>发送报告调整</Button>}
          </div>
        </div>
      </DialogContent>
    </Dialog>
  );
}
