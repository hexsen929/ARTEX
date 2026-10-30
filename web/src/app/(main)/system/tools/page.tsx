"use client";

import * as React from "react";
import Link from "next/link";
import { toast } from "sonner";
import { AlertCircleIcon, PlayIcon, PlusIcon, RefreshCwIcon, RotateCcwIcon, SaveIcon, SearchIcon, Trash2Icon } from "lucide-react";

import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { Switch } from "@/components/ui/switch";
import { Checkbox } from "@/components/ui/checkbox";
import { Textarea } from "@/components/ui/textarea";
import { Separator } from "@/components/ui/separator";
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from "@/components/ui/select";
import {
  Card,
  CardContent,
  CardDescription,
  CardHeader,
  CardTitle,
} from "@/components/ui/card";
import { Tabs, TabsList, TabsTrigger, TabsContent } from "@/components/ui/tabs";
import {
  Sheet,
  SheetContent,
  SheetDescription,
  SheetHeader,
  SheetTitle,
} from "@/components/ui/sheet";
import { api } from "@/lib/api";
import { agentBindingSupport, toolUsageDisplay } from "@/lib/tool-bindings";
import type { Agent, Tool } from "@/lib/types";

// Traffic tools are host tools gated by the global 流量捕获 switch: bindable, but
// only usable when capture is on. Keep in sync with traffic.SeedToolMetas.
const TRAFFIC_TOOL_KEYS = new Set(["traffic_search", "traffic_get"]);

function useToolEditorLifetime() {
  const lifetime = React.useRef({ active: true, generation: 0 });
  React.useEffect(() => {
    lifetime.current.active = true;
    lifetime.current.generation++;
    return () => {
      lifetime.current.active = false;
      lifetime.current.generation++;
    };
  }, []);
  return lifetime;
}

// paramRows flattens schema.properties into an ordered row list for editing.
// name/type/required are read-only (welded to the Go handler); description/default
// are editable. Non-scalar params (array/object) can't carry an editable default.
// parentKey is set for rows that live inside an array param's items.properties.
type ParamRow = {
  name: string;
  type: string;
  required: boolean;
  description: string;
  defaultStr: string; // "" = no default declared
  scalar: boolean;
  parentKey?: string; // set when this is a sub-field of an array param's items
};

// eslint-disable-next-line @typescript-eslint/no-explicit-any
function toRows(schema: Record<string, any>): ParamRow[] {
  const props = (schema?.properties ?? {}) as Record<string, Record<string, unknown>>;
  const required = new Set<string>((schema?.required as string[]) ?? []);
  const rows: ParamRow[] = [];
  for (const [name, p] of Object.entries(props)) {
    const type = String(p?.type ?? "");
    const hasDefault = p != null && "default" in p && (p as Record<string, unknown>).default != null;
    rows.push({
      name, type,
      required: required.has(name),
      description: String(p?.description ?? ""),
      defaultStr: hasDefault ? String((p as Record<string, unknown>).default) : "",
      scalar: ["string", "integer", "number", "boolean"].includes(type),
    });
    // Expand items.properties for array params so sub-fields are editable.
    if (type === "array") {
      // eslint-disable-next-line @typescript-eslint/no-explicit-any
      const itemProps = (p as any)?.items?.properties as Record<string, Record<string, unknown>> | undefined;
      // eslint-disable-next-line @typescript-eslint/no-explicit-any
      const itemRequired = new Set<string>(((p as any)?.items?.required as string[]) ?? []);
      if (itemProps) {
        for (const [subName, subP] of Object.entries(itemProps)) {
          const subType = String((subP as Record<string, unknown>)?.type ?? "");
          const subHasDefault = subP != null && "default" in subP && subP.default != null;
          rows.push({
            name: subName, type: subType,
            required: itemRequired.has(subName),
            description: String(subP?.description ?? ""),
            defaultStr: subHasDefault ? String(subP.default) : "",
            scalar: ["string", "integer", "number", "boolean"].includes(subType),
            parentKey: name,
          });
        }
      }
    }
  }
  return rows;
}

// coerceDefault turns the edited default string back into a typed JSON value, or
// undefined to drop the "default" key entirely.
function coerceDefault(type: string, raw: string): unknown {
  const s = raw.trim();
  if (s === "") return undefined;
  if (type === "integer" || type === "number") {
    const n = Number(s);
    return Number.isNaN(n) ? undefined : n;
  }
  if (type === "boolean") return s === "true";
  return raw;
}

// applyRows writes edited rows back into a deep-copied schema (structure untouched).
// eslint-disable-next-line @typescript-eslint/no-explicit-any
function applyRows(schema: Record<string, any>, rows: ParamRow[]): Record<string, any> {
  const next = structuredClone(schema ?? {});
  const props = (next.properties ?? {}) as Record<string, Record<string, unknown>>;
  for (const r of rows) {
    if (r.parentKey) {
      // Sub-field of an array param's items.properties
      // eslint-disable-next-line @typescript-eslint/no-explicit-any
      const parent = props[r.parentKey] as any;
      const subP = parent?.items?.properties?.[r.name] as Record<string, unknown> | undefined;
      if (!subP) continue;
      subP.description = r.description;
      if (r.scalar) {
        const dv = coerceDefault(r.type, r.defaultStr);
        if (dv === undefined) delete subP.default;
        else subP.default = dv;
      }
    } else {
      const p = props[r.name];
      if (!p) continue;
      p.description = r.description;
      if (r.scalar) {
        const dv = coerceDefault(r.type, r.defaultStr);
        if (dv === undefined) delete p.default;
        else p.default = dv;
      }
    }
  }
  return next;
}

// ToolEditor is the full edit form for one tool, rendered inside the drawer.
function ToolEditor({
  tool,
  agents,
  captureOn,
  bindingsReady,
  onSaved,
  onClose,
}: {
  tool: Tool;
  agents: Agent[];
  captureOn: boolean;
  bindingsReady: boolean;
  onSaved: () => void;
  onClose: () => void;
}) {
  // traffic tools can't be bound/enabled until the global 流量捕获 switch is on.
  const trafficGated = TRAFFIC_TOOL_KEYS.has(tool.key) && !captureOn;
  const [description, setDescription] = React.useState(tool.description);
  const [bound, setBound] = React.useState<string[]>(tool.agents);
  const [enabled, setEnabled] = React.useState(tool.enabled);
  const [rows, setRows] = React.useState<ParamRow[]>(() => toRows(tool.schema));
  const [saving, setSaving] = React.useState(false);
  const lifetime = useToolEditorLifetime();
  const mutationBusy = React.useRef(false);
  const draftSignature = JSON.stringify({ description, bound, enabled, rows });
  const latestDraft = React.useRef(draftSignature);
  latestDraft.current = draftSignature;

  const setRow = (i: number, patch: Partial<ParamRow>) =>
    setRows((rs) => rs.map((r, j) => (j === i ? { ...r, ...patch } : r)));
  const toggleAgent = (k: string) =>
    setBound((b) => (b.includes(k) ? b.filter((x) => x !== k) : [...b, k]));

  async function save() {
    if (!bindingsReady || mutationBusy.current) return;
    mutationBusy.current = true;
    const generation = lifetime.current.generation;
    const submitted = draftSignature;
    setSaving(true);
    try {
      await api.saveTool(tool.key, {
        description,
        schema: applyRows(tool.schema, rows),
        agents: bound,
        enabled,
      });
      onSaved();
      if (lifetime.current.active && generation === lifetime.current.generation) {
        toast.success(`已保存工具「${tool.key}」`);
        if (latestDraft.current === submitted) onClose();
      }
    } catch (e) {
      if (lifetime.current.active && generation === lifetime.current.generation) toast.error("保存失败：" + (e as Error).message);
    } finally {
      mutationBusy.current = false;
      if (lifetime.current.active && generation === lifetime.current.generation) setSaving(false);
    }
  }
  async function reset() {
    if (mutationBusy.current) return;
    mutationBusy.current = true;
    const generation = lifetime.current.generation;
    const submitted = draftSignature;
    setSaving(true);
    try {
      await api.resetTool(tool.key);
      onSaved();
      if (lifetime.current.active && generation === lifetime.current.generation) {
        toast.success(`已恢复「${tool.key}」为代码默认`);
        if (latestDraft.current === submitted) onClose();
      }
    } catch (e) {
      if (lifetime.current.active && generation === lifetime.current.generation) toast.error("恢复失败：" + (e as Error).message);
    } finally {
      mutationBusy.current = false;
      if (lifetime.current.active && generation === lifetime.current.generation) setSaving(false);
    }
  }

  return (
    <div className="flex min-h-0 flex-1 flex-col">
      <div className="flex min-h-0 flex-1 flex-col gap-4 overflow-y-auto px-4">
        {trafficGated && (
          <div className="border-amber-500/40 bg-amber-500/10 text-muted-foreground rounded-md border px-3 py-2 text-xs">
            该工具依赖<b>流量捕获</b>。请先在「系统配置」开启流量捕获，才能绑定给 Agent 并启用。
          </div>
        )}
        {/* binding + switch */}
        <div className="flex flex-wrap items-center gap-x-6 gap-y-3">
          <div className="grid gap-1.5">
            <Label className="text-muted-foreground text-xs">绑定 Agent（决定该工具给哪些 Agent）</Label>
            <div className="flex flex-wrap gap-3">
              {agents.map((ag) => {
                const support = agentBindingSupport(ag, "tool", tool.key);
                return <label key={ag.key} className="flex items-center gap-2 text-sm" title={support.reason}>
                  <Checkbox
                    aria-label={`${ag.key} 工具绑定`}
                    checked={bound.includes(ag.key)}
                    disabled={trafficGated || !bindingsReady || !support.allowed}
                    onCheckedChange={() => toggleAgent(ag.key)}
                  />
                  {ag.name}
                  <span className="text-muted-foreground font-mono text-xs">{ag.key}</span>
                  {!support.allowed && <span className="text-muted-foreground text-xs">{support.reason}{bound.includes(ag.key) ? " 已有配置仅保留。" : ""}</span>}
                </label>;
              })}
              {!bindingsReady && <span className="text-destructive text-xs">绑定配置尚未完整载入，暂不接受修改。</span>}
              {bindingsReady && agents.length === 0 && <span className="text-muted-foreground text-xs">（无 Agent）</span>}
            </div>
          </div>
          <div className="flex items-center gap-2">
            <Switch checked={enabled} onCheckedChange={setEnabled} id={`en-${tool.key}`} />
            <Label htmlFor={`en-${tool.key}`} className="text-sm">
              启用
            </Label>
          </div>
        </div>

        {/* description */}
        <div className="grid gap-1.5">
          <Label className="text-muted-foreground text-xs">工具描述（发送给模型）</Label>
          <Textarea
            className="font-mono text-xs"
            rows={6}
            value={description}
            onChange={(e) => setDescription(e.target.value)}
          />
        </div>

        {/* params */}
        <div className="grid gap-2">
          <Label className="text-muted-foreground text-xs">
            参数（名称 / 类型 / 必填只读；描述与默认值可改）
          </Label>
          {rows.length === 0 && <span className="text-muted-foreground text-xs">（无参数）</span>}
          {rows.map((r, i) => (
            <div
              key={(r.parentKey ?? "") + "." + r.name}
              className={r.parentKey
                ? "border-l-2 border-muted ml-3 pl-3 grid gap-2 py-2"
                : "grid gap-2 rounded-md border p-3"
              }
            >
              <div className="flex flex-wrap items-center gap-2">
                {r.parentKey && <span className="text-muted-foreground font-mono text-[10px]">↳</span>}
                <span className="font-mono text-sm">{r.name}</span>
                <Badge variant="secondary" className="px-1.5 py-0 text-[10px]">
                  {r.type || "?"}
                </Badge>
                {r.required && (
                  <Badge variant="outline" className="px-1.5 py-0 text-[10px]">
                    必填
                  </Badge>
                )}
                {r.parentKey && (
                  <span className="text-muted-foreground text-[10px]">items 子字段</span>
                )}
              </div>
              <div className="grid gap-2">
                <div className="grid gap-1">
                  <Label className="text-muted-foreground text-[11px]">描述</Label>
                  <Input
                    className="text-xs"
                    value={r.description}
                    onChange={(e) => setRow(i, { description: e.target.value })}
                  />
                </div>
                <div className="grid gap-1">
                  <Label className="text-muted-foreground text-[11px]">默认值</Label>
                  <Input
                    className="text-xs"
                    placeholder={r.scalar ? "（空=无默认）" : "仅标量支持"}
                    disabled={!r.scalar}
                    value={r.defaultStr}
                    onChange={(e) => setRow(i, { defaultStr: e.target.value })}
                  />
                </div>
              </div>
            </div>
          ))}
        </div>
      </div>

      <Separator className="mt-4" />
      <div className="flex flex-wrap gap-2 p-4">
        <Button size="sm" onClick={save} disabled={saving || !bindingsReady}>
          <SaveIcon /> 保存
        </Button>
        <Button size="sm" variant="outline" onClick={reset} disabled={saving || !bindingsReady}>
          <RotateCcwIcon /> 恢复默认
        </Button>
      </div>
    </div>
  );
}

// ToolGridCard is one clickable tile in the catalog grid.
function ToolGridCard({ tool, onClick }: { tool: Tool; onClick: () => void }) {
  const usage = toolUsageDisplay(tool);
  return (
    <button
      type="button"
      data-testid={`tool-card-${tool.key}`}
      onClick={onClick}
      className="hover:border-primary/50 hover:bg-muted/40 focus-visible:ring-ring flex flex-col gap-2 rounded-lg border p-4 text-left transition-colors focus-visible:ring-2 focus-visible:outline-none"
    >
      <div className="flex flex-wrap items-center gap-2">
        <span className="font-mono text-sm font-medium">{tool.key}</span>
        {tool.system ? (
          <Badge variant="secondary" className="px-1.5 py-0 text-[10px]">
            系统
          </Badge>
        ) : (
          <Badge variant="outline" className="px-1.5 py-0 text-[10px]">
            自定义·{tool.kind}
          </Badge>
        )}
        {tool.deferred && usage.applicable && (
          <Badge variant="outline" className="px-1.5 py-0 text-[10px]">
            deferred
          </Badge>
        )}
        {!tool.enabled && (
          <Badge variant="outline" className="text-destructive px-1.5 py-0 text-[10px]">
            已停用
          </Badge>
        )}
        <Badge
          data-testid={`tool-usage-${tool.key}`}
          variant="secondary"
          className="ml-auto px-1.5 py-0 text-[10px] tabular-nums"
          title={usage.detail}
        >
          {usage.label}
        </Badge>
      </div>
      <p className="text-muted-foreground line-clamp-1 h-4 text-xs">
        {tool.description || "（无描述）"}
      </p>
      {!usage.applicable && <p className="text-muted-foreground text-xs">经 Bash 使用；独立调用次数不适用。</p>}
      <div className="mt-auto flex flex-wrap gap-1 pt-1">
        {tool.agents.length === 0 && (
          <span className="text-muted-foreground text-[10px]">（未绑定 Agent）</span>
        )}
        {tool.agents.map((a) => (
          <Badge key={a} variant="outline" className="px-1.5 py-0 text-[10px]">
            {a}
          </Badge>
        ))}
      </div>
    </button>
  );
}

export default function ToolsPage() {
  const [tools, setTools] = React.useState<Tool[]>([]);
  const [agents, setAgents] = React.useState<Agent[]>([]);
  const [captureOn, setCaptureOn] = React.useState(false);
  const [selectedKey, setSelectedKey] = React.useState<string | null>(null);
  const [customEdit, setCustomEdit] = React.useState<Tool | "new" | null>(null);
  const [loading, setLoading] = React.useState(true);
  const [loadError, setLoadError] = React.useState("");
  const [agentsError, setAgentsError] = React.useState("");
  const [settingsError, setSettingsError] = React.useState("");
  const [resourcesReady, setResourcesReady] = React.useState(false);
  const requests = React.useRef({ tools: 0, resources: 0 });

  const reload = React.useCallback(() => {
    const request = ++requests.current.tools;
    setLoading(true);
    api.tools().then((items) => {
      if (request !== requests.current.tools) return;
      setTools(items); setLoadError("");
    }).catch((error: unknown) => {
      if (request === requests.current.tools) setLoadError(error instanceof Error ? error.message : "工具列表读取失败");
    }).finally(() => { if (request === requests.current.tools) setLoading(false); });
  }, []);
  const loadResources = React.useCallback(() => {
    const request = ++requests.current.resources;
    setResourcesReady(false);
    Promise.allSettled([api.agents(), api.settings()]).then(([agentResult, settingsResult]) => {
      if (request !== requests.current.resources) return;
      if (agentResult.status === "fulfilled") { setAgents(agentResult.value); setAgentsError(""); }
      else setAgentsError(agentResult.reason instanceof Error ? agentResult.reason.message : "Agent 绑定目录读取失败");
      if (settingsResult.status === "fulfilled") { setCaptureOn(!!settingsResult.value.traffic_capture); setSettingsError(""); }
      else setSettingsError(settingsResult.reason instanceof Error ? settingsResult.reason.message : "系统开关读取失败");
      setResourcesReady(agentResult.status === "fulfilled" && settingsResult.status === "fulfilled");
    });
  }, []);
  React.useEffect(() => {
    reload();
    loadResources();
    return () => { requests.current.tools++; requests.current.resources++; };
  }, [reload, loadResources]);
  const refreshAll = () => { reload(); loadResources(); };

  const [query, setQuery] = React.useState("");

  const selected = tools.find((t) => t.key === selectedKey) ?? null;
  const matchTool = React.useCallback((t: Tool) => {
    const q = query.trim().toLowerCase();
    if (!q) return true;
    return (
      t.key.toLowerCase().includes(q) ||
      t.description.toLowerCase().includes(q) ||
      t.agents.some((a) => a.toLowerCase().includes(q))
    );
  }, [query]);
  const systemTools = tools.filter((t) => t.system && matchTool(t));
  const customTools = tools.filter((t) => !t.system && matchTool(t));
  const allSystemCount = tools.filter((t) => t.system).length;
  const allCustomCount = tools.filter((t) => !t.system).length;
  // clicking a built-in tool opens the binding drawer; a custom tool opens its full editor.
  const openTool = (t: Tool) => (t.system ? setSelectedKey(t.key) : setCustomEdit(t));

  return (
    <div className="flex flex-1 flex-col gap-4 md:gap-6">
      <div className="flex flex-wrap items-start justify-between gap-3">
        <div>
          <h1 className="text-xl font-semibold tracking-tight">工具</h1>
          <p className="text-muted-foreground text-sm">系统工具绑定、自定义可调用工具及 shell 环境声明；绑定与调用记录不代表运行健康检查。</p>
        </div>
        <div className="relative w-64">
          <SearchIcon className="text-muted-foreground absolute left-2.5 top-1/2 size-3.5 -translate-y-1/2" />
          <Input
            className="h-8 pl-8 text-sm"
            placeholder="搜索工具名、描述、Agent…"
            value={query}
            onChange={(e) => setQuery(e.target.value)}
          />
        </div>
        <Button size="sm" variant="outline" onClick={refreshAll}><RefreshCwIcon className={loading ? "animate-spin" : ""} />刷新</Button>
      </div>

      {(loadError || agentsError || settingsError) && <div role="alert" data-testid="tools-load-error" className="space-y-1 rounded-md border border-amber-300 bg-amber-50 p-3 text-sm text-amber-900 dark:bg-amber-950/30 dark:text-amber-200"><p className="flex items-center gap-2"><AlertCircleIcon className="size-4" />部分数据读取失败，已载入的数据保留；错误不代表没有记录。</p>{loadError && <p className="text-xs">工具：{loadError}</p>}{agentsError && <p className="text-xs">Agent：{agentsError}</p>}{settingsError && <p className="text-xs">系统开关：{settingsError}</p>}</div>}
      <p className="text-muted-foreground text-xs">“目录调用”只统计可调用工具的调用尝试，不等于成功次数；shell 环境声明经 Bash 使用，不产生独立目录调用。<Link href="/function/commands" prefetch={false} className="ml-2 text-primary hover:underline">查看命令记录</Link></p>

      <Tabs defaultValue="system">
        <TabsList>
          <TabsTrigger value="system">系统工具</TabsTrigger>
          <TabsTrigger value="custom">自定义工具</TabsTrigger>
        </TabsList>

        <TabsContent value="system">
          <Card>
            <CardHeader>
              <CardTitle>系统工具</CardTitle>
              <CardDescription>
                {loadError && tools.length === 0 ? "工具列表尚未载入" : query.trim()
                  ? `${systemTools.length} / ${allSystemCount} 个匹配，点击卡片编辑描述、参数默认值与 Agent 绑定`
                  : `共 ${allSystemCount} 个，点击卡片编辑描述、参数默认值与 Agent 绑定`}
              </CardDescription>
            </CardHeader>
            <CardContent>
              {systemTools.length === 0 ? (
                <p className="text-muted-foreground py-6 text-center text-sm">
                  {loading ? "正在加载工具…" : loadError ? "工具列表读取失败，请刷新重试。" : query.trim() ? "没有匹配的系统工具" : "（暂无系统工具）"}
                </p>
              ) : (
                <div className="grid gap-3 sm:grid-cols-2 lg:grid-cols-3">
                  {systemTools.map((t) => (
                    <ToolGridCard key={t.key} tool={t} onClick={() => openTool(t)} />
                  ))}
                </div>
              )}
            </CardContent>
          </Card>
        </TabsContent>

        <TabsContent value="custom">
          <Card>
            <CardHeader>
              <div className="flex items-start justify-between gap-3">
                <div>
                  <CardTitle>自定义工具</CardTitle>
                  <CardDescription>
                    {loadError && tools.length === 0 ? "工具列表尚未载入" : query.trim()
                      ? `shell/command/script/http，${customTools.length} / ${allCustomCount} 个匹配，点击卡片编辑`
                      : `shell(bash 声明) / command(命令) / script(Python) / http(API)，共 ${allCustomCount} 个，点击卡片编辑`}
                  </CardDescription>
                </div>
                <Button size="sm" onClick={() => setCustomEdit("new")}>
                  <PlusIcon /> 新建自定义工具
                </Button>
              </div>
            </CardHeader>
            <CardContent>
              {customTools.length === 0 ? (
                <p className="text-muted-foreground py-6 text-center text-sm">
                  {loading ? "正在加载工具…" : loadError ? "工具列表读取失败，请刷新重试。" : query.trim() ? "没有匹配的自定义工具" : "（暂无自定义工具，点击右上角「新建自定义工具」）"}
                </p>
              ) : (
                <div className="grid gap-3 sm:grid-cols-2 lg:grid-cols-3">
                  {customTools.map((t) => (
                    <ToolGridCard key={t.key} tool={t} onClick={() => openTool(t)} />
                  ))}
                </div>
              )}
            </CardContent>
          </Card>
        </TabsContent>
      </Tabs>

      <Sheet open={!!selected} onOpenChange={(o) => !o && setSelectedKey(null)}>
        <SheetContent
          side="right"
          className="gap-0 p-0 data-[side=right]:w-full data-[side=right]:sm:max-w-2xl"
        >
          {selected && (
            <>
              <SheetHeader className="px-4">
                <SheetTitle className="font-mono">{selected.key}</SheetTitle>
                <SheetDescription>编辑描述、参数默认值与 Agent 绑定</SheetDescription>
              </SheetHeader>
              <ToolEditor
                key={selected.key}
                tool={selected}
                agents={agents}
                captureOn={captureOn}
                bindingsReady={resourcesReady}
                onSaved={reload}
                onClose={() => setSelectedKey(null)}
              />
            </>
          )}
        </SheetContent>
      </Sheet>

      <CustomToolDialog
        key={customEdit === "new" ? "new" : customEdit?.key || "closed"}
        edit={customEdit}
        agents={agents}
        bindingsReady={resourcesReady}
        onClose={() => setCustomEdit(null)}
        onSaved={(closeEditor = true) => {
          if (closeEditor) setCustomEdit(null);
          reload();
        }}
      />
    </div>
  );
}

// ---- 自定义工具编辑器 ----

type ExecState = {
  command: string;
  code: string;
  method: string;
  url: string;
  headers: string;
  body: string;
  timeout_ms: string;
  proxy: string;
  use_recording_proxy: boolean;
};

function CustomToolDialog({
  edit,
  agents,
  bindingsReady,
  onClose,
  onSaved,
}: {
  edit: Tool | "new" | null;
  agents: Agent[];
  bindingsReady: boolean;
  onClose: () => void;
  onSaved: (closeEditor?: boolean) => void;
}) {
  const isNew = edit === "new";
  const tool = edit && edit !== "new" ? edit : null;
  const [key, setKey] = React.useState("");
  const [description, setDescription] = React.useState("");
  const [kind, setKind] = React.useState<"shell" | "command" | "script" | "http">("shell");
  const [ex, setEx] = React.useState<ExecState>({
    command: "", code: "", method: "GET", url: "", headers: "", body: "", timeout_ms: "", proxy: "", use_recording_proxy: false,
  });
  const [schemaText, setSchemaText] = React.useState("");
  const [bound, setBound] = React.useState<string[]>([]);
  const [deferred, setDeferred] = React.useState(false);
  const [enabled, setEnabled] = React.useState(true);
  const [saving, setSaving] = React.useState(false);
  const [paramsText, setParamsText] = React.useState("");
  const [testing, setTesting] = React.useState(false);
  const [testResult, setTestResult] = React.useState<{ output: string; is_error: boolean } | null>(null);
  const lifetime = useToolEditorLifetime();
  const mutationBusy = React.useRef(false);
  const testRequest = React.useRef(0);
  const testBusy = React.useRef(false);
  const draftSignature = JSON.stringify({ key, description, kind, ex, schemaText, bound, deferred, enabled });
  const testSignature = JSON.stringify({ kind, ex, paramsText });
  const latestDraft = React.useRef(draftSignature);
  const latestTest = React.useRef(testSignature);
  latestDraft.current = draftSignature;
  latestTest.current = testSignature;

  React.useEffect(() => { setTestResult(null); }, [testSignature]);

  // (re)load form state when opening.
  React.useEffect(() => {
    if (!edit) return;
    setParamsText(""); setTestResult(null);
    if (edit === "new") {
      setKey(""); setDescription(""); setKind("shell");
      setEx({ command: "", code: "", method: "GET", url: "", headers: "", body: "", timeout_ms: "", proxy: "", use_recording_proxy: false });
      setSchemaText(""); setBound([]); setDeferred(false); setEnabled(true);
      return;
    }
    const t = edit;
    const e = (t.exec ?? {}) as Record<string, unknown>;
    setKey(t.key);
    setDescription(t.description);
    setKind((t.kind as "shell" | "command" | "script" | "http") ?? "shell");
    setEx({
      command: String(e.command ?? ""),
      code: String(e.code ?? ""),
      method: String(e.method ?? "GET"),
      url: String(e.url ?? ""),
      headers: e.headers ? JSON.stringify(e.headers, null, 2) : "",
      body: String(e.body ?? ""),
      timeout_ms: e.timeout_ms ? String(e.timeout_ms) : "",
      proxy: String(e.proxy ?? ""),
      use_recording_proxy: !!e.use_recording_proxy,
    });
    setSchemaText(t.schema && Object.keys(t.schema).length ? JSON.stringify(t.schema, null, 2) : "");
    setBound(t.agents ?? []);
    setDeferred(!!t.deferred);
    setEnabled(t.enabled);
  }, [edit]);

  const toggleAgent = (k: string) => setBound((b) => (b.includes(k) ? b.filter((x) => x !== k) : [...b, k]));

  function buildExec(): Record<string, unknown> {
    if (kind === "shell") return {};
    const t = ex.timeout_ms ? Number(ex.timeout_ms) : undefined;
    if (kind === "command") return { command: ex.command, timeout_ms: t };
    if (kind === "script") return { code: ex.code, timeout_ms: t };
    let headers: Record<string, string> = {};
    if (ex.headers.trim()) {
      try { headers = JSON.parse(ex.headers); } catch { /* validated on save */ }
    }
    return { method: ex.method, url: ex.url, headers, body: ex.body, timeout_ms: t, proxy: ex.proxy, use_recording_proxy: ex.use_recording_proxy };
  }

  async function save() {
    if (!bindingsReady || mutationBusy.current) return;
    if (isNew && !/^[a-z][a-z0-9_]*$/.test(key.trim())) {
      toast.error("key 需小写字母开头，仅含小写字母/数字/下划线");
      return;
    }
    let schema: Record<string, unknown> = {};
    if (schemaText.trim()) {
      try { schema = JSON.parse(schemaText); } catch { toast.error("参数 JSON Schema 格式错误"); return; }
    }
    if (kind === "http" && ex.headers.trim()) {
      try { JSON.parse(ex.headers); } catch { toast.error("headers JSON 格式错误"); return; }
    }
    if (kind === "http") {
      const props = (schema as { properties?: Record<string, unknown> }).properties;
      if (!props || Object.keys(props).length === 0) {
        toast.error("http 工具必须提供参数 JSON Schema（含 properties，不能留空）");
        return;
      }
    }
    mutationBusy.current = true;
    const generation = lifetime.current.generation;
    const submitted = draftSignature;
    setSaving(true);
    const payload = { description, schema: kind === "shell" ? {} : schema, agents: bound, enabled, kind, exec: buildExec(), deferred: kind === "shell" ? false : deferred };
    try {
      if (isNew) await api.createCustomTool({ key: key.trim(), ...payload });
      else await api.updateCustomTool(tool!.key, payload);
      const current = lifetime.current.active && generation === lifetime.current.generation;
      if (current) toast.success(isNew ? `已创建自定义工具「${key.trim()}」` : `已保存工具「${tool!.key}」`);
      // Refresh persisted data even when this old editor has closed, but never
      // close a newly opened tool or discard edits made after submission.
      onSaved(current && latestDraft.current === submitted);
    } catch (e) {
      if (lifetime.current.active && generation === lifetime.current.generation) toast.error("保存失败：" + (e as Error).message);
    } finally {
      mutationBusy.current = false;
      if (lifetime.current.active && generation === lifetime.current.generation) setSaving(false);
    }
  }
  async function del() {
    if (!tool || mutationBusy.current) return;
    mutationBusy.current = true;
    const generation = lifetime.current.generation;
    setSaving(true);
    try {
      await api.deleteCustomTool(tool.key);
      const current = lifetime.current.active && generation === lifetime.current.generation;
      if (current) toast.success(`已删除工具「${tool.key}」`);
      onSaved(current);
    } catch (e) {
      if (lifetime.current.active && generation === lifetime.current.generation) toast.error("删除失败：" + (e as Error).message);
    } finally {
      mutationBusy.current = false;
      if (lifetime.current.active && generation === lifetime.current.generation) setSaving(false);
    }
  }
  // Executes exactly once using the submitted unsaved specification. Closing or
  // changing the editor suppresses stale UI results; it never replays execution.
  async function runTest() {
    if (testBusy.current) return;
    let params: Record<string, unknown> = {};
    if (paramsText.trim()) {
      try { params = JSON.parse(paramsText); } catch { toast.error("测试参数 JSON 格式错误"); return; }
    }
    if (kind === "http" && ex.headers.trim()) {
      try { JSON.parse(ex.headers); } catch { toast.error("headers JSON 格式错误"); return; }
    }
    const generation = lifetime.current.generation;
    const request = ++testRequest.current;
    const submitted = testSignature;
    testBusy.current = true;
    setTesting(true);
    setTestResult(null);
    try {
      const r = await api.testCustomTool({ kind, exec: buildExec(), params });
      if (lifetime.current.active && generation === lifetime.current.generation && request === testRequest.current && submitted === latestTest.current) setTestResult(r);
    } catch (e) {
      if (lifetime.current.active && generation === lifetime.current.generation && request === testRequest.current && submitted === latestTest.current) setTestResult({ output: (e as Error).message, is_error: true });
    } finally {
      testBusy.current = false;
      if (lifetime.current.active && generation === lifetime.current.generation && request === testRequest.current) setTesting(false);
    }
  }

  return (
    <Sheet open={!!edit} onOpenChange={(o) => !o && onClose()}>
      <SheetContent
        side="right"
        data-testid="custom-tool-editor"
        className="flex flex-col gap-0 p-0 data-[side=right]:w-full data-[side=right]:sm:max-w-3xl"
      >
        <SheetHeader className="px-4">
          <SheetTitle>{isNew ? "新建自定义工具" : `编辑 ${tool?.key}`}</SheetTitle>
          <SheetDescription>shell=bash 环境声明(只需名称+描述，告知模型可用 bash 调用)；command/script/http 需写执行规格。</SheetDescription>
        </SheetHeader>

        <div className="flex min-h-0 flex-1 flex-col gap-4 overflow-y-auto px-4 pb-4">
          <div className="grid gap-1.5">
            <Label className="text-xs">Key</Label>
            <Input className="font-mono" placeholder="如 nmap_scan" value={key} disabled={!isNew}
              onChange={(e) => setKey(e.target.value)} />
          </div>
          <div className="grid gap-1.5">
            <Label className="text-xs">描述(发给模型)</Label>
            <Textarea aria-label="自定义工具描述" rows={2} value={description} onChange={(e) => setDescription(e.target.value)} />
          </div>

          <div className="grid gap-1.5">
            <Label className="text-xs">类型</Label>
            <Select value={kind} onValueChange={(v) => setKind(v as "shell" | "command" | "script" | "http")}>
              <SelectTrigger className="w-56"><SelectValue /></SelectTrigger>
              <SelectContent>
                <SelectItem value="shell">shell（bash 环境声明）</SelectItem>
                <SelectItem value="command">command（shell 命令模板）</SelectItem>
                <SelectItem value="script">script（Python 脚本）</SelectItem>
                <SelectItem value="http">http（API 请求）</SelectItem>
              </SelectContent>
            </Select>
            {kind === "shell" && (
              <p className="text-muted-foreground text-xs">适合 nmap、sqlmap、ffuf 等出名工具——模型已知用法，只需声明"可在 bash 中调用"即可。名称+描述会追加到 Bash 工具描述里。</p>
            )}
          </div>

          {kind === "command" && (
            <div className="grid gap-1.5">
              <Label className="text-xs">命令模板（占位符 {"{param}"}，如 nmap -p {"{ports}"} {"{target}"}）</Label>
              <Textarea aria-label="自定义工具命令模板" className="font-mono text-xs" rows={2} value={ex.command}
                onChange={(e) => setEx({ ...ex, command: e.target.value })} />
            </div>
          )}
          {kind === "script" && (
            <div className="grid gap-1.5">
              <Label className="text-xs">Python 正文（参数走 stdin JSON / os.environ["TOOL_X"]）</Label>
              <Textarea className="font-mono text-xs" rows={10} value={ex.code}
                placeholder={'import json,sys\nargs=json.load(sys.stdin)\nprint(...)'}
                onChange={(e) => setEx({ ...ex, code: e.target.value })} />
            </div>
          )}
          {kind === "http" && (
            <div className="grid gap-2">
              <div className="flex gap-2">
                <div className="grid gap-1.5">
                  <Label className="text-xs">Method</Label>
                  <Input className="w-24" value={ex.method} onChange={(e) => setEx({ ...ex, method: e.target.value })} />
                </div>
                <div className="grid flex-1 gap-1.5">
                  <Label className="text-xs">URL（可含 {"{param}"}）</Label>
                  <Input className="font-mono text-xs" value={ex.url} onChange={(e) => setEx({ ...ex, url: e.target.value })} />
                </div>
              </div>
              <div className="grid gap-1.5">
                <Label className="text-xs">Headers（JSON，可含 {"{param}"}）</Label>
                <Textarea className="font-mono text-xs" rows={2} value={ex.headers}
                  placeholder={'{"Authorization": "Bearer {token}"}'} onChange={(e) => setEx({ ...ex, headers: e.target.value })} />
              </div>
              <div className="grid gap-1.5">
                <Label className="text-xs">Body（可含 {"{param}"}）</Label>
                <Textarea className="font-mono text-xs" rows={2} value={ex.body} onChange={(e) => setEx({ ...ex, body: e.target.value })} />
              </div>
              <div className="flex items-center gap-4">
                <div className="grid gap-1.5">
                  <Label className="text-xs">代理 URL（空=直连）</Label>
                  <Input className="font-mono text-xs w-56" value={ex.proxy} onChange={(e) => setEx({ ...ex, proxy: e.target.value })} />
                </div>
                <label className="mt-4 flex items-center gap-2 text-sm">
                  <Checkbox checked={ex.use_recording_proxy} onCheckedChange={(v) => setEx({ ...ex, use_recording_proxy: !!v })} />
                  走记录代理
                </label>
              </div>
            </div>
          )}

          {kind !== "shell" && (
            <div className="flex items-center gap-3">
              <div className="grid gap-1.5">
                <Label className="text-xs">超时(ms，空=默认)</Label>
                <Input type="number" className="w-32" value={ex.timeout_ms} onChange={(e) => setEx({ ...ex, timeout_ms: e.target.value })} />
              </div>
            </div>
          )}

          {kind !== "shell" && (
            <div className="grid gap-1.5">
              <Label className="text-xs">
                参数 JSON Schema{kind === "http" ? "（http 工具必填，需含 properties）" : "（留空 = 自动给 {args} 薄壳）"}
              </Label>
              <Textarea className="font-mono text-xs" rows={4} value={schemaText}
                placeholder={'{"type":"object","properties":{"target":{"type":"string"}},"required":["target"]}'}
                onChange={(e) => setSchemaText(e.target.value)} />
            </div>
          )}

          <div className="grid gap-1.5">
            <Label className="text-muted-foreground text-xs">绑定 Agent</Label>
            <div className="flex flex-wrap gap-3">
              {agents.map((a) => {
                const support = agentBindingSupport(a, "tool", key.trim());
                return <label key={a.key} className="flex items-center gap-2 text-sm" title={support.reason}>
                  <Checkbox aria-label={`${a.key} 工具绑定`} checked={bound.includes(a.key)} disabled={!bindingsReady || !support.allowed} onCheckedChange={() => toggleAgent(a.key)} />
                  {a.name}<span className="text-muted-foreground font-mono text-xs">{a.key}</span>
                  {!support.allowed && <span className="text-muted-foreground text-xs">{support.reason}{bound.includes(a.key) ? " 已有配置仅保留。" : ""}</span>}
                </label>;
              })}
            </div>
            {!bindingsReady && <p className="text-destructive text-xs">绑定目录尚未完整载入，暂不接受修改。</p>}
            {kind === "shell" && <p className="text-muted-foreground text-xs">环境声明仅补充已绑定执行角色的 Bash 说明，不创建独立可调用工具。<Link href="/function/commands" prefetch={false} className="ml-2 text-primary hover:underline">查看命令记录</Link></p>}
          </div>

          <div className="flex items-center gap-6">
            <label className="flex items-center gap-2 text-sm">
              <Switch checked={enabled} onCheckedChange={setEnabled} /> 启用
            </label>
            {kind !== "shell" && (
              <label className="flex items-center gap-2 text-sm">
                <Switch checked={deferred} onCheckedChange={setDeferred} /> deferred（大量不常用工具才开）
              </label>
            )}
          </div>

          {kind !== "shell" && (
            <div className="grid gap-1.5 rounded-md border p-3">
              <Label className="text-xs font-medium">测试运行（用当前表单，不会保存）</Label>
              <p className="text-muted-foreground text-xs">会实际执行一次。修改表单或切换编辑器后不展示旧结果，也不会自动重试。</p>
              <Textarea aria-label="自定义工具测试参数" className="font-mono text-xs" rows={2} value={paramsText}
                placeholder={'示例参数 JSON，如 {"target":"example.com"}'}
                onChange={(e) => setParamsText(e.target.value)} />
              <div>
                <Button size="sm" variant="outline" onClick={runTest} disabled={testing}>
                  <PlayIcon /> {testing ? "运行中…" : "测试运行"}
                </Button>
              </div>
              {testResult && (
                <pre
                  data-testid="custom-tool-test-result"
                  className={
                    "max-h-64 overflow-auto whitespace-pre-wrap break-words rounded bg-muted p-2 font-mono text-xs " +
                    (testResult.is_error ? "text-destructive" : "")
                  }
                >
                  {testResult.output || "（无输出）"}
                </pre>
              )}
            </div>
          )}
        </div>

        <Separator />
        <div className="flex items-center gap-2 p-4">
          <Button size="sm" onClick={save} disabled={saving || !bindingsReady}>
            <SaveIcon /> {isNew ? "创建" : "保存"}
          </Button>
          {!isNew && (
            <Button size="sm" variant="outline" className="text-destructive" onClick={del} disabled={saving}>
              <Trash2Icon /> 删除
            </Button>
          )}
        </div>
      </SheetContent>
    </Sheet>
  );
}
