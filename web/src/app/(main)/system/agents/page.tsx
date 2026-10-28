"use client";

import * as React from "react";
import { toast } from "sonner";
import { AlertCircleIcon, Bot, PlusIcon, RefreshCwIcon, Trash2Icon } from "lucide-react";

import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { Textarea } from "@/components/ui/textarea";
import {
  Card,
  CardContent,
  CardDescription,
  CardHeader,
  CardTitle,
} from "@/components/ui/card";
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
  DialogTrigger,
} from "@/components/ui/dialog";
import {
  AlertDialog,
  AlertDialogAction,
  AlertDialogCancel,
  AlertDialogContent,
  AlertDialogDescription,
  AlertDialogFooter,
  AlertDialogHeader,
  AlertDialogTitle,
  AlertDialogTrigger,
} from "@/components/ui/alert-dialog";
import { Sheet, SheetContent, SheetDescription, SheetHeader, SheetTitle } from "@/components/ui/sheet";
import { AgentEditor } from "@/components/agent-editor";
import { api } from "@/lib/api";
import { agentRuntimeCapabilities, bindingCountLabel } from "@/lib/tool-bindings";
import type { Agent } from "@/lib/types";

// AgentGridCard is one clickable tile opening the agent's editor drawer. Custom
// (non-builtin) agents get a delete button.
function AgentGridCard({
  agent,
  onOpen,
  onDeleted,
}: {
  agent: Agent;
  onOpen: () => void;
  onDeleted: () => void;
}) {
  const capabilities = agentRuntimeCapabilities(agent);
  async function del() {
    try {
      await api.deleteAgent(agent.key);
      toast.success(`已删除 Agent「${agent.name}」`);
      onDeleted();
    } catch (e) {
      toast.error("删除失败：" + (e as Error).message);
    }
  }
  return (
    <div className="hover:border-primary/50 group relative flex flex-col gap-2 rounded-lg border p-4 transition-colors">
      <button type="button" data-testid={`agent-card-${agent.key}`} onClick={onOpen} className="flex flex-col gap-2 text-left">
        <div className="flex flex-wrap items-center gap-2">
          <Bot className="text-muted-foreground size-4" />
          <span className="text-sm font-medium">{agent.name}</span>
          <span className="text-muted-foreground font-mono text-xs">{agent.key}</span>
          {agent.builtin ? (
            <Badge variant="secondary" className="px-1.5 py-0 text-[10px]">
              内置
            </Badge>
          ) : (
            <Badge variant="outline" className="px-1.5 py-0 text-[10px]">
              自定义
            </Badge>
          )}
          {!agent.enabled && (
            <Badge variant="outline" className="text-destructive px-1.5 py-0 text-[10px]">
              已停用
            </Badge>
          )}
        </div>
        <p className="text-muted-foreground line-clamp-2 min-h-8 text-xs">
          {agent.description || "（无描述）"}
        </p>
        <div className="text-muted-foreground flex flex-wrap gap-1.5 text-[10px]">
          <span className="rounded border px-1.5 py-0.5">{capabilities.mcp_binding_mode === "inherited" ? `MCP 沿用 ${capabilities.execution_agent_key}` : capabilities.mcp_binding_mode === "unsupported" ? "不使用 MCP" : `MCP 绑定 ${bindingCountLabel(agent.mcp_count, agent.binding_counts_available)}`}</span>
          <span className="rounded border px-1.5 py-0.5">{capabilities.skill_binding_mode === "unsupported" ? "不使用 Skill" : `${capabilities.skill_binding_mode === "readonly" ? "Skill 指导" : "Skill 绑定"} ${bindingCountLabel(agent.skill_count, agent.binding_counts_available)}`}</span>
          <span className="rounded border px-1.5 py-0.5">{capabilities.tool_binding_mode === "inherited" ? `工具沿用 ${capabilities.execution_agent_key}` : capabilities.tool_binding_mode === "unsupported" ? "无执行工具" : `${capabilities.tool_binding_mode === "limited" ? "基础工具绑定" : "工具绑定"} ${bindingCountLabel(agent.tool_count, agent.binding_counts_available)}`}</span>
        </div>
        {capabilities.note && <p className="text-muted-foreground text-xs">{capabilities.note}</p>}
        {agent.binding_counts_available === false && <p className="text-destructive text-xs">绑定统计不可用，不代表没有授权。</p>}
        {agent.automation && <p data-testid={`agent-automation-${agent.key}`} className={agent.automation.available ? "text-muted-foreground text-xs" : "text-amber-700 text-xs dark:text-amber-300"}>{agent.automation.available ? agent.automation.reason : `自动流程状态未知：${agent.automation.reason || "读取失败"}`}</p>}
      </button>
      {!agent.builtin && (
        <AlertDialog>
          <AlertDialogTrigger asChild>
            <Button
              variant="ghost"
              size="icon-sm"
              className="text-muted-foreground hover:text-destructive absolute top-2 right-2 opacity-0 transition-opacity group-hover:opacity-100"
            >
              <Trash2Icon className="size-3.5" />
            </Button>
          </AlertDialogTrigger>
          <AlertDialogContent>
            <AlertDialogHeader>
              <AlertDialogTitle>删除 Agent「{agent.name}」？</AlertDialogTitle>
              <AlertDialogDescription>
                将一并删除它的提示词、变量、可见性与工具绑定。此操作不可撤销。
              </AlertDialogDescription>
            </AlertDialogHeader>
            <AlertDialogFooter>
              <AlertDialogCancel>取消</AlertDialogCancel>
              <AlertDialogAction onClick={del}>删除</AlertDialogAction>
            </AlertDialogFooter>
          </AlertDialogContent>
        </AlertDialog>
      )}
    </div>
  );
}

function CreateAgentDialog({ onCreated }: { onCreated: (key: string) => void }) {
  const [open, setOpen] = React.useState(false);
  const [key, setKey] = React.useState("");
  const [name, setName] = React.useState("");
  const [description, setDescription] = React.useState("");
  const [busy, setBusy] = React.useState(false);

  async function create() {
    setBusy(true);
    try {
      const a = await api.createAgent(key.trim(), name.trim(), description.trim());
      toast.success(`已创建 Agent「${a.name}」`);
      setOpen(false);
      setKey("");
      setName("");
      setDescription("");
      onCreated(a.key);
    } catch (e) {
      toast.error("创建失败：" + (e as Error).message);
    } finally {
      setBusy(false);
    }
  }

  const keyOk = /^[a-z][a-z0-9_]*$/.test(key.trim());
  const canCreate = keyOk && name.trim().length > 0 && !busy;

  return (
    <Dialog open={open} onOpenChange={setOpen}>
      <DialogTrigger asChild>
        <Button size="sm">
          <PlusIcon /> 新建 Agent
        </Button>
      </DialogTrigger>
      <DialogContent>
        <DialogHeader>
          <DialogTitle>新建自定义 Agent</DialogTitle>
          <DialogDescription>
            创建一个会话型助手。key 用于内部标识，创建后不可更改；名称与描述用于识别。
          </DialogDescription>
        </DialogHeader>
        <div className="grid gap-4 py-2">
          <div className="grid gap-1.5">
            <Label htmlFor="agent-key">Key</Label>
            <Input
              id="agent-key"
              placeholder="如 research_helper"
              value={key}
              onChange={(e) => setKey(e.target.value)}
              className="font-mono"
            />
            {key.length > 0 && !keyOk && (
              <span className="text-destructive text-xs">小写字母开头，仅含小写字母/数字/下划线</span>
            )}
          </div>
          <div className="grid gap-1.5">
            <Label htmlFor="agent-name">名称</Label>
            <Input
              id="agent-name"
              placeholder="如 研究助手"
              value={name}
              onChange={(e) => setName(e.target.value)}
            />
          </div>
          <div className="grid gap-1.5">
            <Label htmlFor="agent-desc">描述</Label>
            <Textarea
              id="agent-desc"
              placeholder="一句话说明这个 Agent 是干什么的"
              rows={2}
              value={description}
              onChange={(e) => setDescription(e.target.value)}
            />
          </div>
        </div>
        <DialogFooter>
          <Button onClick={create} disabled={!canCreate}>
            创建
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  );
}

export default function AgentsPage() {
  const [agents, setAgents] = React.useState<Agent[]>([]);
  const [editKey, setEditKey] = React.useState<string | null>(null);
  const [loading, setLoading] = React.useState(true);
  const [loadError, setLoadError] = React.useState("");
  const requestGeneration = React.useRef(0);

  const reload = React.useCallback(() => {
    const generation = ++requestGeneration.current;
    setLoading(true);
    api.agents().then((items) => {
      if (generation !== requestGeneration.current) return;
      setAgents(items); setLoadError("");
    }).catch((error: unknown) => {
      if (generation === requestGeneration.current) setLoadError(error instanceof Error ? error.message : "Agent 列表读取失败");
    }).finally(() => { if (generation === requestGeneration.current) setLoading(false); });
  }, []);
  React.useEffect(() => {
    reload();
    return () => { requestGeneration.current++; };
  }, [reload]);

  const editing = agents.find((a) => a.key === editKey) ?? null;

  return (
    <div className="flex flex-1 flex-col gap-4 md:gap-6">
      <div className="flex items-start justify-between gap-3">
        <div>
          <h1 className="text-xl font-semibold tracking-tight">Agent</h1>
          <p className="text-muted-foreground text-sm">
            内置 Agent 的提示词/配置，以及自定义会话 Agent 的创建与管理
          </p>
        </div>
        <CreateAgentDialog
          onCreated={(key) => {
            reload();
            setEditKey(key);
          }}
        />
      </div>

      {loadError && <div role="alert" data-testid="agents-load-error" className="flex flex-wrap items-center gap-2 rounded-md border border-amber-300 bg-amber-50 p-3 text-sm text-amber-900 dark:bg-amber-950/30 dark:text-amber-200"><AlertCircleIcon className="size-4" /><span>Agent 列表读取失败，已载入数据保留：{loadError}</span><Button size="sm" variant="outline" onClick={reload}><RefreshCwIcon />重试</Button></div>}
      <p className="text-muted-foreground text-xs">这里展示启用的显式绑定，不是实际可用能力或成功调用数；运行时还受任务、全局开关与资源就绪状态限制。继承角色单独注明执行来源。</p>

      <Card>
        <CardHeader>
          <CardTitle>Agent 清单</CardTitle>
          <CardDescription>{loading && agents.length === 0 ? "正在加载…" : loadError && agents.length === 0 ? "列表尚未载入" : `共 ${agents.length} 个`}</CardDescription>
        </CardHeader>
        <CardContent>
          {agents.length === 0 ? (
            <p className="text-muted-foreground py-6 text-center text-sm">{loading ? "正在加载 Agent…" : loadError ? "读取失败，请重试。" : "（暂无 Agent）"}</p>
          ) : (
            <div className="grid gap-3 sm:grid-cols-2 lg:grid-cols-3">
              {agents.map((a) => (
                <AgentGridCard key={a.key} agent={a} onOpen={() => setEditKey(a.key)} onDeleted={reload} />
              ))}
            </div>
          )}
        </CardContent>
      </Card>

      <Sheet open={!!editing} onOpenChange={(o) => !o && setEditKey(null)}>
        <SheetContent
          side="right"
          className="flex flex-col gap-0 p-0 data-[side=right]:w-full data-[side=right]:sm:max-w-4xl"
        >
          {editing && (
            <>
              <SheetHeader className="px-4">
                <SheetTitle className="flex items-center gap-2">
                  {editing.name}
                  <span className="text-muted-foreground font-mono text-xs">{editing.key}</span>
                  {!editing.builtin && (
                    <Badge variant="outline" className="px-1.5 py-0 text-[10px]">
                      自定义
                    </Badge>
                  )}
                </SheetTitle>
                <SheetDescription>{editing.description || "提示词、配置、可见资源与工具绑定"}</SheetDescription>
              </SheetHeader>
              <AgentEditor agentKey={editing.key} onSaved={reload} />
            </>
          )}
        </SheetContent>
      </Sheet>
    </div>
  );
}
