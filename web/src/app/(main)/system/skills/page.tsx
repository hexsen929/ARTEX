"use client";

import * as React from "react";
import { toast } from "sonner";
import {
  ChevronRightIcon,
  FolderIcon,
  FolderOpenIcon,
  FolderPlusIcon,
  FileTextIcon,
  FilePlusIcon,
  PlusIcon,
  Trash2Icon,
  UploadIcon,
  AlertTriangleIcon,
  RefreshCwIcon,
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
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { Textarea } from "@/components/ui/textarea";
import { Checkbox } from "@/components/ui/checkbox";
import { Popover, PopoverContent, PopoverTrigger } from "@/components/ui/popover";
import { ScrollArea } from "@/components/ui/scroll-area";
import { Badge } from "@/components/ui/badge";
import {
  Sheet,
  SheetContent,
  SheetFooter,
  SheetHeader,
  SheetTitle,
} from "@/components/ui/sheet";
import { Tabs, TabsContent, TabsList, TabsTrigger } from "@/components/ui/tabs";
import { cn } from "@/lib/utils";
import { api } from "@/lib/api";
import { agentBindingSupport } from "@/lib/tool-bindings";
import { SKILL_USAGE_SCOPE_NOTE, skillExplicitLoads, summarizeSkillUsage } from "@/lib/skill-observability";
import { acknowledgeMissingSkillNotices, missingSkillLibraryLabel, missingSkillNoticeSummary } from "@/lib/missing-skill-notices";
import type { Agent, SkillItem, MCPServer, SkillCall, MissingSkill } from "@/lib/types";

function fmtTime(ts?: string) {
  if (!ts) return "未记录显式加载";
  return new Date(ts).toLocaleString("zh-CN", {
    month: "2-digit",
    day: "2-digit",
    hour: "2-digit",
    minute: "2-digit",
  });
}

// ── Tree node ──────────────────────────────────────────────────────────────
interface TreeNode {
  name: string;
  path: string;   // relative to skill root, dirs WITHOUT trailing slash
  type: "file" | "dir";
  children: TreeNode[];
}

// Backend returns dirs as "scripts/" (trailing slash) and files as "scripts/a.py".
function buildTree(entries: string[]): TreeNode[] {
  const root: TreeNode[] = [];
  const dirMap = new Map<string, TreeNode>();

  function ensureDir(dirPath: string, parentNodes: TreeNode[]): TreeNode[] {
    if (dirMap.has(dirPath)) return dirMap.get(dirPath)!.children;
    const parts = dirPath.split("/");
    let nodes = root;
    let cur = "";
    for (const part of parts) {
      cur = cur ? `${cur}/${part}` : part;
      if (!dirMap.has(cur)) {
        const d: TreeNode = { name: part, path: cur, type: "dir", children: [] };
        nodes.push(d);
        dirMap.set(cur, d);
      }
      nodes = dirMap.get(cur)!.children;
    }
    return nodes;
  }

  for (const entry of [...entries].sort()) {
    if (entry.endsWith("/")) {
      // Explicit directory entry — ensure the node exists (may already be created)
      ensureDir(entry.slice(0, -1), root);
    } else {
      // File — ensure parent dirs exist, then push file node
      const parts = entry.split("/");
      let nodes = root;
      let cur = "";
      for (let i = 0; i < parts.length - 1; i++) {
        cur = cur ? `${cur}/${parts[i]}` : parts[i];
        if (!dirMap.has(cur)) {
          const d: TreeNode = { name: parts[i], path: cur, type: "dir", children: [] };
          nodes.push(d);
          dirMap.set(cur, d);
        }
        nodes = dirMap.get(cur)!.children;
      }
      const fname = parts[parts.length - 1];
      if (fname) nodes.push({ name: fname, path: entry, type: "file", children: [] });
    }
  }
  sortNodes(root);
  return root;
}

// sortNodes orders every level like a file explorer: directories before files,
// then case-insensitive by name. Recurses into children.
function sortNodes(nodes: TreeNode[]): void {
  nodes.sort((a, b) => {
    if (a.type !== b.type) return a.type === "dir" ? -1 : 1;
    return a.name.localeCompare(b.name, undefined, { sensitivity: "base" });
  });
  for (const n of nodes) {
    if (n.children.length > 0) sortNodes(n.children);
  }
}

// ── State types ───────────────────────────────────────────────────────────
type Selected =
  | { skill: string; path: null }
  | { skill: string; path: string };

type Creating = {
  skill: string;
  inDir: string;
  kind: "file" | "dir";
} | null;

type PendingDelete =
  | { kind: "skill"; skill: string }
  | { kind: "file"; skill: string; path: string }
  | { kind: "dir"; skill: string; path: string }
  | null;

// ── Overview (empty-state) ──────────────────────────────────────────────────
// Shown when nothing is selected: a library-wide snapshot from data already loaded
// (the skill list carries per-skill usage; missing is fetched alongside). No extra
// requests — this is pure aggregation over props.
function SkillsOverview({
  skills, missing, onSelect, loading, listError, usageAvailable, missingAvailable,
}: {
  skills: SkillItem[];
  missing: MissingSkill[];
  onSelect: (name: string) => void;
  loading: boolean;
  listError: string;
  usageAvailable?: boolean;
  missingAvailable: boolean;
}) {
  const agg = React.useMemo(() => summarizeSkillUsage(skills, usageAvailable), [skills, usageAvailable]);
  if (skills.length === 0) {
    return <div className="flex h-full items-center justify-center"><p className="text-sm text-muted-foreground">
      {loading ? "正在读取 Skill 目录…" : listError ? "Skill 列表读取失败，请刷新重试；这不表示没有 Skill。" : "暂无 Skill，点击左侧「新建」或「上传压缩包」开始"}
    </p></div>;
  }
  const stats = [
    { label: "Skill 目录", value: skills.length, hint: `${agg.loadableCount} 个已确认可加载 · ${agg.invalid.length} 个有问题${agg.loadabilityUnknown ? ` · ${agg.loadabilityUnknown} 个未提供诊断` : ""}` },
    { label: "显式加载记录", value: agg.totalCalls ?? "统计不可用", hint: agg.unknownCount ? `${agg.unknownCount} 个统计状态未知` : `${agg.usedCount} 个有记录` },
    { label: "未记录显式加载", value: agg.totalCalls === null ? "—" : agg.unrecorded.length, hint: "不等于未使用自动规则" },
    { label: "待处理未解析调用", value: missingAvailable ? missing.reduce((sum, item) => sum + item.calls, 0) : "读取不可用", hint: missingAvailable ? (missing.length ? `涉及 ${missing.length} 个 Skill` : "暂无待处理记录") : "保留上次记录，请刷新重试" },
  ];
  return <div data-testid="skills-overview" className="mx-auto w-full max-w-4xl space-y-6">
    <div><h2 className="text-base font-semibold">技能库总览</h2><p className="text-muted-foreground mt-1 text-sm">{SKILL_USAGE_SCOPE_NOTE}</p></div>
    <div className="grid grid-cols-2 gap-3 lg:grid-cols-4">{stats.map((stat) => <div key={stat.label} className="rounded-lg border p-3"><p className="text-xl font-semibold tabular-nums">{stat.value}</p><p className="text-xs font-medium">{stat.label}</p><p className="text-muted-foreground mt-1 text-[11px]">{stat.hint}</p></div>)}</div>
    {agg.invalid.length > 0 ? <div role="alert" className="space-y-2 rounded-lg border border-amber-300 p-3 text-xs"><p className="font-medium">以下目录存在加载问题，不计为可加载 Skill：</p>{agg.invalid.map((skill) => <button key={skill.name} type="button" className="block text-left text-primary hover:underline" onClick={() => onSelect(skill.name)}>{skill.name}：{skill.load_error || "SKILL.md 或元数据需要检查"}</button>)}</div> : null}
    <div className="space-y-2"><Label className="text-xs text-muted-foreground">显式加载记录排行</Label>
      {agg.ranked.length === 0 ? <p className="rounded-lg border border-dashed p-4 text-center text-xs text-muted-foreground">{agg.totalCalls === null ? "统计不可用，暂不判断哪些 Skill 没有加载记录。" : "尚未记录显式 Skill 加载；内置自动规则不在此统计。"}</p> : <div className="space-y-1.5">{agg.ranked.slice(0, 8).map(({ skill, calls }) => <button key={skill.name} type="button" onClick={() => onSelect(skill.name)} className="flex w-full items-center gap-3 rounded-md px-2 py-1.5 text-left hover:bg-muted"><span className="w-40 shrink-0 truncate font-mono text-xs">{skill.name}</span><span className="relative h-2 flex-1 overflow-hidden rounded-full bg-muted"><span className="absolute inset-y-0 left-0 rounded-full bg-primary/70" style={{ width: `${agg.topCalls ? calls / agg.topCalls * 100 : 0}%` }} /></span><span className="w-16 text-right text-xs tabular-nums text-muted-foreground">{calls} 次</span></button>)}</div>}
    </div>
    <div className="grid gap-6 sm:grid-cols-2"><div className="space-y-2"><Label className="text-xs text-muted-foreground">最近显式加载</Label>{agg.recent.length ? agg.recent.map((skill) => <button key={skill.name} type="button" onClick={() => onSelect(skill.name)} className="flex w-full items-center gap-2 rounded px-1.5 py-1 text-left hover:bg-muted"><span className="min-w-0 flex-1 truncate font-mono text-xs">{skill.name}</span><span className="text-[11px] text-muted-foreground">{fmtTime(skill.last_used)}</span></button>) : <p className="text-xs text-muted-foreground">{agg.totalCalls === null ? "统计不可用" : "暂无显式加载时间记录。"}</p>}</div>
      <div className="space-y-2"><Label className="text-xs text-muted-foreground">未记录显式 Skill 加载{agg.totalCalls !== null ? `（${agg.unrecorded.length}）` : ""}</Label>{agg.totalCalls === null ? <p className="text-xs text-muted-foreground">统计未就绪，不标记为未加载。</p> : agg.unrecorded.length ? <div className="flex flex-wrap gap-1.5">{agg.unrecorded.map((skill) => <button key={skill.name} type="button" onClick={() => onSelect(skill.name)}><Badge variant="outline" className="max-w-48 cursor-pointer truncate font-mono text-xs font-normal">{skill.name}</Badge></button>)}</div> : <p className="text-xs text-muted-foreground">可统计的 Skill 均有显式加载记录。</p>}</div>
    </div>
  </div>;
}

// ── Page ──────────────────────────────────────────────────────────────────
export default function SkillsPage() {
  const [skills, setSkills] = React.useState<SkillItem[]>([]);
  const [listLoading, setListLoading] = React.useState(true);
  const [listError, setListError] = React.useState("");
  const [usageAvailable, setUsageAvailable] = React.useState<boolean | undefined>();
  const [usageSummaryError, setUsageSummaryError] = React.useState("");
  const [registryError, setRegistryError] = React.useState("");
  const [agentsLoading, setAgentsLoading] = React.useState(true);
  const [agentsError, setAgentsError] = React.useState("");
  const [mcpLoading, setMcpLoading] = React.useState(true);
  const [mcpError, setMcpError] = React.useState("");
  const [visibilityStatus, setVisibilityStatus] = React.useState<Record<string, "loading" | "ready" | "error">>({});
  const [visibilityErrors, setVisibilityErrors] = React.useState<Record<string, string>>({});
  const [mcpSaving, setMcpSaving] = React.useState(false);
  const [refreshRevision, setRefreshRevision] = React.useState(0);
  const catalogGeneration = React.useRef(0);
  const visibilityGeneration = React.useRef<Record<string, number>>({});
  const visibilityBusy = React.useRef(new Set<string>());
  const mcpBusy = React.useRef(false);
  const [agents, setAgents] = React.useState<Agent[]>([]);
  const [selected, setSelected] = React.useState<Selected | null>(null);
  const [visibility, setVisibility] = React.useState<Record<string, string[]>>({});
  const [expanded, setExpanded] = React.useState<Set<string>>(new Set());

  // file editor
  const [fileContent, setFileContent] = React.useState("");
  const [fileLoading, setFileLoading] = React.useState(false);
  const [fileError, setFileError] = React.useState("");
  const [fileFor, setFileFor] = React.useState("");
  const fileGeneration = React.useRef(0);
  const selectedRef = React.useRef<Selected | null>(null);
  selectedRef.current = selected;
  const [dirty, setDirty] = React.useState(false);
  const [saving, setSaving] = React.useState(false);

  // inline create — using a ref to avoid stale closure in onBlur
  const [creating, setCreating] = React.useState<Creating>(null);
  const [newEntryName, setNewEntryName] = React.useState("");
  const inlineRef = React.useRef<HTMLInputElement>(null);
  // cancelRef prevents onBlur from committing when Escape was pressed
  const cancelRef = React.useRef(false);

  // new skill dialog
  const [newOpen, setNewOpen] = React.useState(false);
  const [newName, setNewName] = React.useState("");
  const [newDesc, setNewDesc] = React.useState("");
  const [newLicense, setNewLicense] = React.useState("");
  const [newCompat, setNewCompat] = React.useState("");
  const [newInst, setNewInst] = React.useState("");
  const [newMcps, setNewMcps] = React.useState<string[]>([]);
  const [newVisibility, setNewVisibility] = React.useState<string[]>([]);
  const [mcpOptions, setMcpOptions] = React.useState<MCPServer[]>([]);
  const [creatingSkill, setCreatingSkill] = React.useState(false);
  const [uploading, setUploading] = React.useState(false);
  const uploadRef = React.useRef<HTMLInputElement>(null);

  // skill detail — MCP edit state (optimistic, rolls back on error)
  const [detailMcps, setDetailMcps] = React.useState<string[]>([]);

  // delete confirmation
  const [pendingDelete, setPendingDelete] = React.useState<PendingDelete>(null);
  const [deleting, setDeleting] = React.useState(false);

  // 调用统计：列表页的次数/最近调用随 api.skills() 一起回来；选中某个 skill 时再拉它的
  // 最近调用明细。missing = 当时 Agent 的可见注册表未解析成功、尚未处理的调用。
  const [usageCalls, setUsageCalls] = React.useState<SkillCall[]>([]);
  const [usageLoading, setUsageLoading] = React.useState(false);
  const [usageError, setUsageError] = React.useState("");
  const [usageFor, setUsageFor] = React.useState("");
  const usageGeneration = React.useRef(0);
  const [missing, setMissing] = React.useState<MissingSkill[]>([]);
  const [missingError, setMissingError] = React.useState("");
  const [missingLoading, setMissingLoading] = React.useState(true);
  const [clearingMissing, setClearingMissing] = React.useState(false);
  const missingGeneration = React.useRef(0);
  const clearingMissingRef = React.useRef(false);
  const missingMounted = React.useRef(false);

  const refreshMissing = React.useCallback(async () => {
    if (clearingMissingRef.current) return;
    const generation = ++missingGeneration.current;
    setMissingLoading(true);
    try {
      const notices = await api.missingSkills(200);
      if (missingMounted.current && generation === missingGeneration.current) {
        setMissing(notices);
        setMissingError("");
      }
    } catch (error) {
      if (missingMounted.current && generation === missingGeneration.current) {
        setMissingError(`读取提示失败：${(error as Error).message}`);
      }
    } finally {
      if (missingMounted.current && generation === missingGeneration.current) setMissingLoading(false);
    }
  }, []);

  const clearMissing = async () => {
    if (clearingMissingRef.current) return;
    if (!window.confirm("将此前的未解析提示标记为已处理？原始调用账本（含归档）、Skill 文件和成功调用记录都会保留；之后的新未解析调用仍会提示。")) return;
    clearingMissingRef.current = true;
    setClearingMissing(true);
    setMissingError("");
    const generation = ++missingGeneration.current;
    try {
      const notices = await acknowledgeMissingSkillNotices(
        () => api.clearMissingSkills(),
        () => api.missingSkills(200),
      );
      if (missingMounted.current && generation === missingGeneration.current) {
        setMissing(notices);
        toast.success("历史提示已标记处理，原始调用记录已保留");
      }
    } catch (error) {
      if (missingMounted.current && generation === missingGeneration.current) {
        // Keep the old list visible when the save or following reload fails.
        setMissingError(`清除或刷新失败：${(error as Error).message}。请重新加载确认，原始记录未删除。`);
      }
    } finally {
      clearingMissingRef.current = false;
      if (missingMounted.current) setClearingMissing(false);
    }
  };

  // ── Data: every failure stays visible; stale responses never overwrite a newer refresh. ──
  const refreshVisibility = React.useCallback(async (name: string) => {
    const generation = (visibilityGeneration.current[name] ?? 0) + 1;
    visibilityGeneration.current[name] = generation;
    setVisibilityStatus((current) => ({ ...current, [name]: "loading" }));
    try {
      const ids = await api.skillVisibility(name);
      if (!missingMounted.current || visibilityGeneration.current[name] !== generation) return;
      setVisibility((current) => ({ ...current, [name]: ids }));
      setVisibilityErrors((current) => ({ ...current, [name]: "" }));
      setVisibilityStatus((current) => ({ ...current, [name]: "ready" }));
    } catch (error) {
      if (!missingMounted.current || visibilityGeneration.current[name] !== generation) return;
      setVisibilityErrors((current) => ({ ...current, [name]: `读取授权失败：${(error as Error).message}` }));
      setVisibilityStatus((current) => ({ ...current, [name]: "error" }));
    }
  }, []);

  const load = React.useCallback(() => {
    const generation = ++catalogGeneration.current;
    setListLoading(true);
    setAgentsLoading(true);
    setMcpLoading(true);
    setRefreshRevision((current) => current + 1);
    api.agents().then((items) => {
      if (missingMounted.current && generation === catalogGeneration.current) { setAgents(items); setAgentsError(""); }
    }).catch((error: unknown) => {
      if (missingMounted.current && generation === catalogGeneration.current) setAgentsError(`Agent 列表读取失败：${(error as Error).message}`);
    }).finally(() => {
      if (missingMounted.current && generation === catalogGeneration.current) setAgentsLoading(false);
    });
    api.mcpServers().then((items) => {
      if (missingMounted.current && generation === catalogGeneration.current) { setMcpOptions(items); setMcpError(""); }
    }).catch((error: unknown) => {
      if (missingMounted.current && generation === catalogGeneration.current) setMcpError(`MCP 列表读取失败：${(error as Error).message}`);
    }).finally(() => {
      if (missingMounted.current && generation === catalogGeneration.current) setMcpLoading(false);
    });
    void refreshMissing();
    api.skillsStatus().then((result) => {
      if (!missingMounted.current || generation !== catalogGeneration.current) return;
      const items = result.skills ?? [];
      setSkills(items);
      setListError("");
      setUsageAvailable(result.usage_available);
      setUsageSummaryError(result.usage_available === false ? (result.usage_error || "显式加载统计读取失败，次数未知。") : "");
      setRegistryError(result.registry_available === false ? (result.registry_error || "Skill 库加载检查未完成，请检查目录诊断。") : "");
      items.forEach((skill) => { void refreshVisibility(skill.name); });
    }).catch((error: unknown) => {
      if (missingMounted.current && generation === catalogGeneration.current) setListError(`Skill 列表读取失败：${(error as Error).message}`);
    }).finally(() => {
      if (missingMounted.current && generation === catalogGeneration.current) setListLoading(false);
    });
  }, [refreshMissing, refreshVisibility]);

  React.useEffect(() => {
    missingMounted.current = true;
    load();
    return () => {
      missingMounted.current = false;
      missingGeneration.current++;
      catalogGeneration.current++;
      usageGeneration.current++;
      fileGeneration.current++;
      for (const name of Object.keys(visibilityGeneration.current)) visibilityGeneration.current[name]++;
    };
  }, [load]);

  // ── Upload a .zip skill ───────────────────────────────────────────────────
  async function uploadZip(file: File, overwrite = false) {
    setUploading(true);
    try {
      const r = await api.uploadSkill(file, overwrite);
      toast.success(`已安装 Skill：${r.name}（${r.files} 个文件）`);
      load();
    } catch (e) {
      const msg = (e as Error).message;
      // offer overwrite when the skill already exists
      if (!overwrite && msg.includes("已存在")) {
        if (window.confirm(`${msg}\n\n是否覆盖同名 Skill？`)) {
          await uploadZip(file, true);
          return;
        }
      } else {
        toast.error("上传失败：" + msg);
      }
    } finally {
      setUploading(false);
    }
  }
  function onUploadPick(e: React.ChangeEvent<HTMLInputElement>) {
    const f = e.target.files?.[0];
    e.target.value = ""; // reset so picking the same file again re-fires
    if (f) uploadZip(f);
  }

  React.useEffect(() => {
    if (creating) {
      // Small timeout so the element is actually in the DOM before focusing
      setTimeout(() => inlineRef.current?.focus(), 20);
    }
  }, [creating]);

  // Sync detail-panel MCP state when the selected skill changes.
  React.useEffect(() => {
    if (!selected || selected.path !== null) return;
    const sk = skills.find((s) => s.name === selected.skill);
    setDetailMcps(sk?.mcps ?? []);
  }, [selected, skills]);

  // Explicit-load details are hot-table records only; summary totals may include archives.
  React.useEffect(() => {
    const generation = ++usageGeneration.current;
    if (!selected || selected.path !== null) { setUsageCalls([]); setUsageFor(""); setUsageError(""); setUsageLoading(false); return; }
    const name = selected.skill;
    setUsageLoading(true);
    setUsageError("");
    if (usageFor !== name) setUsageCalls([]);
    api.skillUsageStatus(name, 20).then((result) => {
      if (!missingMounted.current || generation !== usageGeneration.current) return;
      if (result.usage_available === false) throw new Error(result.usage_error || "近期显式加载记录暂不可读");
      setUsageCalls(result.calls ?? []);
      setUsageFor(name);
      setUsageError("");
    }).catch((error: unknown) => {
      if (missingMounted.current && generation === usageGeneration.current) setUsageError(`读取近期加载明细失败：${(error as Error).message}`);
    }).finally(() => {
      if (missingMounted.current && generation === usageGeneration.current) setUsageLoading(false);
    });
    return () => { usageGeneration.current++; };
  }, [selected, refreshRevision]);

  React.useEffect(() => {
    const generation = ++fileGeneration.current;
    if (!selected || selected.path === null) { setFileContent(""); setFileFor(""); setFileError(""); setDirty(false); setFileLoading(false); return; }
    const key = `${selected.skill}/${selected.path}`;
    // A metadata refresh must not discard unsaved file edits.
    if (fileFor === key && dirty && !fileError) return;
    setFileLoading(true);
    setFileError("");
    if (fileFor !== key) { setFileContent(""); setDirty(false); }
    api.readSkillFile(selected.skill, selected.path).then((content) => {
      if (!missingMounted.current || generation !== fileGeneration.current) return;
      setFileContent(content); setFileFor(key); setDirty(false); setFileError("");
    }).catch((error: unknown) => {
      if (missingMounted.current && generation === fileGeneration.current) setFileError(`读取文件失败：${(error as Error).message}`);
    }).finally(() => {
      if (missingMounted.current && generation === fileGeneration.current) setFileLoading(false);
    });
    return () => { fileGeneration.current++; };
  }, [selected, refreshRevision]);

  // ── Expand helpers ────────────────────────────────────────────────────────
  function toggleExpanded(key: string) {
    setExpanded((prev) => {
      const next = new Set(prev);
      if (next.has(key)) next.delete(key); else next.add(key);
      return next;
    });
  }
  function ensureExpanded(skill: string, dirPath: string) {
    setExpanded((prev) => {
      const next = new Set(prev);
      next.add(skill);
      if (dirPath) {
        let cur = "";
        for (const part of dirPath.split("/")) {
          cur = cur ? `${cur}/${part}` : part;
          next.add(`${skill}:${cur}`);
        }
      }
      return next;
    });
  }

  // ── Inline create ─────────────────────────────────────────────────────────
  function startCreate(skill: string, inDir: string, kind: "file" | "dir") {
    cancelRef.current = false;
    ensureExpanded(skill, inDir);
    setCreating({ skill, inDir, kind });
    setNewEntryName("");
  }

  // Use a ref snapshot so commitCreate reads the latest creating value
  // without depending on potentially stale closure state.
  const creatingRef = React.useRef<Creating>(null);
  React.useEffect(() => { creatingRef.current = creating; }, [creating]);
  const newEntryRef = React.useRef("");
  React.useEffect(() => { newEntryRef.current = newEntryName; }, [newEntryName]);

  async function commitCreate() {
    if (cancelRef.current) { cancelRef.current = false; return; }
    const c = creatingRef.current;
    const name = newEntryRef.current.trim();
    setCreating(null);
    setNewEntryName("");
    if (!c || !name) return;

    const fullPath = c.inDir ? `${c.inDir}/${name}` : name;
    try {
      if (c.kind === "dir") {
        await api.createSkillDir(c.skill, fullPath);
        toast.success(`已创建文件夹：${fullPath}`);
        ensureExpanded(c.skill, fullPath);
      } else {
        await api.writeSkillFile(c.skill, fullPath, "");
        toast.success(`已创建文件：${fullPath}`);
        setSelected({ skill: c.skill, path: fullPath });
        ensureExpanded(c.skill, c.inDir);
      }
      load();
    } catch (e) {
      toast.error("创建失败：" + (e as Error).message);
    }
  }

  function cancelCreate() {
    cancelRef.current = true;
    setCreating(null);
    setNewEntryName("");
  }

  async function deletePath(skill: string, path: string) {
    try {
      await api.deleteSkillPath(skill, path);
      toast.success(`已删除：${path}`);
      if (selected?.skill === skill && selected.path === path) setSelected(null);
      load();
    } catch (e) {
      toast.error("删除失败：" + (e as Error).message);
    }
  }

  async function deleteSkill(name: string) {
    try {
      await api.deleteSkill(name);
      toast.success(`已删除 Skill：${name}`);
      if (selected?.skill === name) setSelected(null);
      load();
    } catch (e) {
      toast.error("删除失败：" + (e as Error).message);
    }
  }

  async function runPendingDelete() {
    const p = pendingDelete;
    if (!p) return;
    setDeleting(true);
    try {
      if (p.kind === "skill") await deleteSkill(p.skill);
      else await deletePath(p.skill, p.path);
    } finally {
      setDeleting(false);
      setPendingDelete(null);
    }
  }

  async function saveFile() {
    if (!selected || selected.path === null || fileLoading || fileError || fileFor !== `${selected.skill}/${selected.path}` || saving) return;
    const key = `${selected.skill}/${selected.path}`;
    setSaving(true);
    try {
      await api.writeSkillFile(selected.skill, selected.path, fileContent);
      toast.success(`已保存：${key}`);
      if (selectedRef.current?.path !== null && `${selectedRef.current?.skill}/${selectedRef.current?.path}` === key) setDirty(false);
    } catch (e) {
      toast.error("保存失败：" + (e as Error).message);
    } finally { setSaving(false); }
  }

  async function toggleSkillMcp(skillName: string, mcpName: string, mcpOn: boolean) {
    if (mcpBusy.current || mcpLoading || mcpError || listLoading || listError) return;
    const original = detailMcps;
    const next = mcpOn ? [...original, mcpName] : original.filter((name) => name !== mcpName);
    mcpBusy.current = true;
    setMcpSaving(true);
    try {
      await api.updateSkillMeta(skillName, { mcps: next });
      if (missingMounted.current && selectedRef.current?.skill === skillName && selectedRef.current.path === null) setDetailMcps(next);
      toast.success(`${mcpOn ? "关联" : "取消关联"}「${mcpName}」`);
      load();
    } catch (error) {
      toast.error(`MCP 关联保存失败：${(error as Error).message}。保留上次显示，请刷新确认实际关联。`);
    } finally {
      mcpBusy.current = false;
      if (missingMounted.current) setMcpSaving(false);
    }
  }

  async function toggleVisibility(skillName: string, agentId: string, agentName: string) {
    const actor = agents.find((item) => item.id === agentId);
    if (!actor || !agentBindingSupport(actor, "skill").allowed || agentsError || agentsLoading || listLoading || listError || visibilityStatus[skillName] !== "ready" || visibilityBusy.current.has(skillName)) return;
    const on = (visibility[skillName] ?? []).includes(agentId);
    visibilityBusy.current.add(skillName);
    setVisibilityStatus((current) => ({ ...current, [skillName]: "loading" }));
    try {
      await api.toggleSkillVisibility(agentId, skillName, !on);
      toast.success(`${on ? "取消" : "授予"}「${agentName}」可见`);
      await refreshVisibility(skillName);
    } catch (error) {
      if (missingMounted.current) {
        setVisibilityErrors((current) => ({ ...current, [skillName]: `授权保存未完成：${(error as Error).message}。保留上次显示，请刷新确认。` }));
        setVisibilityStatus((current) => ({ ...current, [skillName]: "error" }));
      }
      toast.error("授权更新失败，请刷新确认");
    } finally {
      visibilityBusy.current.delete(skillName);
    }
  }

  async function createNewSkill() {
    if (!newName.trim()) { toast.error("请填写 name"); return; }
    if (!newDesc.trim()) { toast.error("description 为必填项"); return; }
    if (newVisibility.some((id) => { const actor = agents.find((item) => item.id === id); return !actor || !agentBindingSupport(actor, "skill").allowed; })) { toast.error("所选 Agent 不支持 Skill，请先调整可见性选择。"); return; }
    setCreatingSkill(true);
    let createdName = "";
    try {
      const name = newName.trim();
      await api.createSkill({
        name, description: newDesc.trim(),
        license: newLicense.trim() || undefined,
        compatibility: newCompat.trim() || undefined,
        mcps: newMcps.length ? newMcps : undefined,
        instructions: newInst.trim() || undefined,
      });
      createdName = name;
      const results = await Promise.allSettled(newVisibility.map((id) => api.toggleSkillVisibility(id, name, true)));
      if (results.some((result) => result.status === "rejected")) toast.error("Skill 已创建，但部分初始授权未保存；请在详情中刷新并核对授权。");
      else toast.success("已创建 Skill");
      setNewOpen(false);
      setNewName(""); setNewDesc(""); setNewLicense(""); setNewCompat(""); setNewInst("");
      setNewMcps([]); setNewVisibility([]);
      load();
    } catch (e) {
      toast.error(createdName ? `Skill「${createdName}」已创建，后续配置未完成：${(e as Error).message}` : "创建失败：" + (e as Error).message);
      if (createdName) load();
    } finally { setCreatingSkill(false); }
  }

  // ── Inline input JSX helper (NOT a React component — avoids remount on re-render) ──
  // Defined as a plain function returning JSX so React never sees a new component type.
  function inlineInputJSX(indent: number) {
    return (
      <div
        key="__inline_create__"
        className="flex items-center gap-1 py-0.5 pr-2"
        style={{ paddingLeft: indent }}
      >
        {creating?.kind === "dir"
          ? <FolderIcon className="size-3.5 shrink-0 text-amber-500" />
          : <FileTextIcon className="size-3.5 shrink-0 text-muted-foreground" />
        }
        <Input
          ref={inlineRef}
          className="h-6 flex-1 px-1 py-0 font-mono text-xs"
          placeholder={creating?.kind === "dir" ? "folder-name" : "filename.py"}
          value={newEntryName}
          onChange={(e) => setNewEntryName(e.target.value)}
          onKeyDown={(e) => {
            if (e.key === "Enter") { cancelRef.current = false; void commitCreate(); }
            if (e.key === "Escape") cancelCreate();
          }}
          onBlur={() => { void commitCreate(); }}
        />
      </div>
    );
  }

  // ── Recursive tree renderer ───────────────────────────────────────────────
  function renderTree(nodes: TreeNode[], skill: string, depth: number): React.ReactNode {
    // depth+1: the skill root sits at 8px (px-2); its children indent one step
    // further in so the tree reads as nested under the skill folder.
    const baseIndent = 8 + (depth + 1) * 14;
    return nodes.map((node) => {
      if (node.type === "dir") {
        const key = `${skill}:${node.path}`;
        const open = expanded.has(key);
        return (
          <div key={node.path}>
            <div
              className="group relative flex cursor-pointer select-none items-center gap-1 rounded py-0.5 pr-1 text-sm hover:bg-muted"
              style={{ paddingLeft: baseIndent }}
              onClick={() => toggleExpanded(key)}
            >
              <ChevronRightIcon className={cn("size-3.5 shrink-0 transition-transform", open && "rotate-90")} />
              {open
                ? <FolderOpenIcon className="size-3.5 shrink-0 text-amber-500" />
                : <FolderIcon className="size-3.5 shrink-0 text-amber-500" />
              }
              <span className="min-w-0 flex-1 truncate" title={node.path}>{node.name}</span>
              {/* Absolute so a long name can never push the actions out of view */}
              <span className="absolute inset-y-0 right-1 hidden items-center gap-0.5 rounded bg-muted pl-1 group-hover:flex">
                <Button size="icon" variant="ghost" className="size-5" title="新建文件"
                  onClick={(e) => { e.stopPropagation(); startCreate(skill, node.path, "file"); }}>
                  <FilePlusIcon className="size-3 text-muted-foreground" />
                </Button>
                <Button size="icon" variant="ghost" className="size-5" title="新建文件夹"
                  onClick={(e) => { e.stopPropagation(); startCreate(skill, node.path, "dir"); }}>
                  <FolderPlusIcon className="size-3 text-muted-foreground" />
                </Button>
                <Button size="icon" variant="ghost" className="size-5" title="删除文件夹"
                  onClick={(e) => { e.stopPropagation(); setPendingDelete({ kind: "dir", skill, path: node.path }); }}>
                  <Trash2Icon className="size-3 text-destructive" />
                </Button>
              </span>
            </div>
            {open && (
              <>
                {renderTree(node.children, skill, depth + 1)}
                {creating?.skill === skill && creating.inDir === node.path &&
                  inlineInputJSX(baseIndent + 14)}
              </>
            )}
          </div>
        );
      }

      // file node
      const isSelected = selected?.skill === skill && selected.path === node.path;
      return (
        <div
          key={node.path}
          className={cn(
            "group relative flex cursor-pointer select-none items-center gap-1 rounded py-0.5 pr-1 text-sm",
            isSelected ? "bg-accent text-accent-foreground" : "hover:bg-muted",
          )}
          style={{ paddingLeft: baseIndent + 16 }}
          onClick={() => setSelected({ skill, path: node.path })}
        >
          <FileTextIcon className="size-3.5 shrink-0 text-muted-foreground" />
          <span className="min-w-0 flex-1 truncate font-mono text-xs" title={node.path}>{node.name}</span>
          <span className={cn(
            "absolute inset-y-0 right-1 hidden items-center rounded pl-1 group-hover:flex",
            isSelected ? "bg-accent" : "bg-muted",
          )}>
            <Button size="icon" variant="ghost" className="size-5"
              title="删除文件"
              onClick={(e) => { e.stopPropagation(); setPendingDelete({ kind: "file", skill, path: node.path }); }}>
              <Trash2Icon className="size-3 text-destructive" />
            </Button>
          </span>
        </div>
      );
    });
  }

  const selectedSkill = selected
    ? skills.find((s) => s.name === selected.skill) ?? null
    : null;

  return (
    <div data-content-padding="false" className="flex flex-1 flex-col overflow-hidden">
      <div className="flex flex-wrap items-center gap-3 border-b px-4 py-2.5 lg:px-6">
        <div className="flex flex-col gap-0.5">
          <h1 className="text-sm font-semibold leading-tight">Skill</h1>
          <p className="text-muted-foreground text-xs">技能库 · agentskills.io 规范 · 按 Agent 授权可见</p>
        </div>
        <Button type="button" size="sm" variant="outline" className="ml-auto" onClick={load} disabled={listLoading} aria-label="刷新 Skill 模块">
          <RefreshCwIcon className={cn("size-3.5", listLoading && "animate-spin")} />刷新
        </Button>
        {/* 历史未解析调用：可能是未分配给当时的 Agent，也可能是目录不存在。 */}
        {missing.length > 0 && (
          <Popover>
            <PopoverTrigger asChild>
              <Button size="sm" variant="outline" className="ml-auto">
                <AlertTriangleIcon className="size-3.5 text-amber-500" />
                {missingSkillNoticeSummary(missing).skills} 个 Skill · {missingSkillNoticeSummary(missing).calls} 次历史未解析
              </Button>
            </PopoverTrigger>
            <PopoverContent align="end" className="w-80">
              <div className="mb-2 flex items-start justify-between gap-2">
                <p className="text-xs text-muted-foreground">
                  Agent 曾点名调用，但当时未在其可见 Skill 注册表中解析成功，可能是未分配或文件缺失。当前文件存在不等于 Agent 已获授权。
                </p>
                <Button
                  type="button"
                  size="sm"
                  variant="ghost"
                  className="shrink-0 text-xs"
                  disabled={clearingMissing}
                  onClick={() => void clearMissing()}
                >
                  {clearingMissing ? "处理中…" : "清除提示"}
                </Button>
              </div>
              <div className="space-y-1">
                {missing.map((m) => (
                  <div key={m.skill} className="flex items-center gap-2 text-sm">
                    <code className="min-w-0 flex-1 truncate font-mono text-xs" title={m.skill}>{m.skill}</code>
                    <span className="shrink-0 text-[10px] text-muted-foreground">{missingSkillLibraryLabel(m.library_present)}</span>
                    <span className="shrink-0 text-xs tabular-nums text-muted-foreground">{m.calls} 次</span>
                    <span className="shrink-0 text-xs text-muted-foreground">{fmtTime(m.last_used)}</span>
                  </div>
                ))}
              </div>
              {missingError ? <p role="alert" className="mt-2 break-words text-xs text-destructive">{missingError}</p> : null}
              {missingError ? <Button type="button" variant="ghost" size="sm" disabled={clearingMissing} onClick={() => void refreshMissing()}>重新加载提示</Button> : null}
            </PopoverContent>
          </Popover>
        )}
        {missing.length === 0 && missingError ? (
          <div role="alert" className="ml-auto flex items-center gap-2 text-xs text-destructive">
            <span>{missingError}</span>
            <Button type="button" variant="ghost" size="sm" onClick={() => void refreshMissing()}>重试</Button>
          </div>
        ) : null}
      </div>
      {[listError, usageSummaryError, registryError, agentsError, mcpError].some(Boolean) ? (
        <div role="alert" data-testid="skills-module-errors" className="space-y-1 border-b border-amber-300 bg-amber-50 px-4 py-2 text-xs text-amber-900 dark:border-amber-900 dark:bg-amber-950/30 dark:text-amber-200">
          <p className="font-medium">部分数据读取失败，保留上次内容；空白和未知不等于零或未授权。</p>
          {[listError, usageSummaryError, registryError, agentsError, mcpError].filter(Boolean).map((message) => <p key={message} className="break-words">{message}</p>)}
        </div>
      ) : null}
      <div className="flex flex-1 overflow-hidden">
        {/* ── 左侧文件树 ── */}
        <div className="flex w-64 shrink-0 flex-col border-r">
          <div className="flex flex-col gap-2 border-b p-2">
            <Button size="sm" variant="outline" className="w-full" onClick={() => setNewOpen(true)}>
              <PlusIcon className="size-3.5" />新建 Skill
            </Button>
            <input
              ref={uploadRef}
              type="file"
              accept=".zip,application/zip"
              className="hidden"
              onChange={onUploadPick}
            />
            <Button
              size="sm"
              variant="outline"
              className="w-full"
              disabled={uploading}
              onClick={() => uploadRef.current?.click()}
              title="上传包含 SKILL.md 的 .zip 压缩包"
            >
              <UploadIcon className="size-3.5" />
              {uploading ? "上传中…" : "上传压缩包"}
            </Button>
          </div>
          {/* Radix viewport wraps children in a display:table div that grows with
              content — force it to block so long names truncate instead of
              widening the rows past the sidebar. */}
          <ScrollArea className="flex-1 [&>[data-slot=scroll-area-viewport]>div]:!block">
            <div className="p-1">
              {skills.map((s) => {
                const isOpen = expanded.has(s.name);
                const tree = buildTree(s.files);
                const isSkillSelected = selected?.skill === s.name && selected.path === null;
                const calls = skillExplicitLoads(s, usageAvailable);
                return (
                  <div key={s.name}>
                    {/* skill 根节点 */}
                    <div
                      data-testid={`skill-select-${s.name}`}
                      className={cn(
                        "group relative flex cursor-pointer select-none items-center gap-1 rounded px-2 py-1 text-sm",
                        isSkillSelected ? "bg-accent text-accent-foreground" : "hover:bg-muted",
                      )}
                      onClick={() => {
                        toggleExpanded(s.name);
                        setSelected({ skill: s.name, path: null });
                      }}
                    >
                      <ChevronRightIcon className={cn("size-3.5 shrink-0 transition-transform", isOpen && "rotate-90")} />
                      {isOpen
                        ? <FolderOpenIcon className="size-3.5 shrink-0 text-blue-500" />
                        : <FolderIcon className="size-3.5 shrink-0 text-blue-500" />
                      }
                      <span className="min-w-0 flex-1 truncate font-semibold" title={s.name}>{s.name}</span>
                      {s.loadable === false ? <AlertTriangleIcon className="size-3.5 shrink-0 text-amber-600" aria-label="Skill 加载问题" /> : null}
                      {calls === null ? <span className="text-[10px] text-muted-foreground" title="显式加载统计不可用">未知</span> : calls > 0 && (
                        <span
                          className="shrink-0 rounded bg-muted px-1 text-[10px] tabular-nums text-muted-foreground"
                          title={`显式加载记录 ${calls} 次 · 最近 ${fmtTime(s.last_used)}`}
                        >
                          {calls}
                        </span>
                      )}
                      <span className={cn(
                        "absolute inset-y-0 right-1 hidden items-center gap-0.5 rounded pl-1 group-hover:flex",
                        isSkillSelected ? "bg-accent" : "bg-muted",
                      )}>
                        <Button size="icon" variant="ghost" className="size-5" title="新建文件"
                          onClick={(e) => { e.stopPropagation(); startCreate(s.name, "", "file"); }}>
                          <FilePlusIcon className="size-3 text-muted-foreground" />
                        </Button>
                        <Button size="icon" variant="ghost" className="size-5" title="新建文件夹"
                          onClick={(e) => { e.stopPropagation(); startCreate(s.name, "", "dir"); }}>
                          <FolderPlusIcon className="size-3 text-muted-foreground" />
                        </Button>
                        <Button size="icon" variant="ghost" className="size-5" title="删除 Skill"
                          onClick={(e) => { e.stopPropagation(); setPendingDelete({ kind: "skill", skill: s.name }); }}>
                          <Trash2Icon className="size-3 text-destructive" />
                        </Button>
                      </span>
                    </div>

                    {/* 展开：递归文件树 */}
                    {isOpen && (
                      <>
                        {renderTree(tree, s.name, 0)}
                        {creating?.skill === s.name && creating.inDir === "" &&
                          inlineInputJSX(22)}
                      </>
                    )}
                  </div>
                );
              })}

              {skills.length === 0 && <p className="p-3 text-xs text-muted-foreground">{listLoading ? "读取 Skill 目录中…" : listError ? "目录读取失败，请刷新重试。" : "暂无 Skill，点击「新建」开始"}</p>}
            </div>
          </ScrollArea>
        </div>

        {/* ── 右侧面板 ── */}
        <div className="flex flex-1 flex-col overflow-auto p-4">
          {!selected && (
            <SkillsOverview
              skills={skills}
              missing={missing}
              loading={listLoading}
              listError={listError}
              usageAvailable={usageAvailable}
              missingAvailable={!missingLoading && !missingError && !clearingMissing}
              onSelect={(name) => setSelected({ skill: name, path: null })}
            />
          )}

          {selected && selected.path === null && !selectedSkill && !listLoading ? <p className="text-sm text-muted-foreground">所选目录不在当前列表中，请重新选择 Skill；读取失败时请先刷新确认。</p> : null}
          {selected && selected.path === null && selectedSkill && (
            <div data-testid={`skill-detail-${selectedSkill.name}`} className="max-w-5xl space-y-5">
              <div>
                <h2 className="font-mono text-base font-semibold">{selectedSkill.name}</h2>
                {selectedSkill.declared_name && selectedSkill.declared_name !== selectedSkill.name ? <p className="mt-1 text-xs text-muted-foreground">调用名称：<code>{selectedSkill.declared_name}</code>；授权与统计按目录名称归属。</p> : null}
                {selectedSkill.loadable === false ? <p role="alert" className="mt-2 rounded-md border border-amber-300 p-2 text-xs text-amber-800 dark:text-amber-200">加载问题：{selectedSkill.load_error || "SKILL.md 或元数据需要检查"}。请先修正文件；此目录不计为可加载 Skill。</p> : selectedSkill.loadable === undefined ? <p className="mt-1 text-xs text-muted-foreground">当前服务未提供此目录的加载诊断，目录存在不等于运行时已加载。</p> : <Badge variant="outline" className="mt-2 text-xs font-normal">文件与元数据可加载</Badge>}
                {selectedSkill.description && (
                  <p className="mt-1 text-sm text-muted-foreground">{selectedSkill.description}</p>
                )}
                <div className="mt-2 flex flex-wrap gap-2">
                  {selectedSkill.license && (
                    <Badge variant="outline" className="text-xs font-normal">License: {selectedSkill.license}</Badge>
                  )}
                  {selectedSkill.compatibility && (
                    <Badge variant="secondary" className="text-xs font-normal">{selectedSkill.compatibility}</Badge>
                  )}
                </div>
              </div>

              {/* 左右分栏：配置（MCP/可见性）在左为主，调用统计在右为辅。
                  lg 以下放不下时用 flex-row-reverse 回落到单列——统计因 DOM 顺序在前，
                  窄屏时自然落到配置上方（与改版前的上下顺序一致）。 */}
              <div className="flex flex-col gap-6 lg:flex-row-reverse lg:items-start">
                {/* ── 右侧：调用统计 ── */}
                <div className="space-y-2 lg:w-80 lg:shrink-0">
                  <Label className="text-xs text-muted-foreground">显式 Skill 加载统计</Label>
                  <p className="text-[11px] text-muted-foreground">{SKILL_USAGE_SCOPE_NOTE}</p>
                  {skillExplicitLoads(selectedSkill, usageAvailable) === null ? <p role="alert" className="text-xs text-destructive">{selectedSkill.usage_error || usageSummaryError || "显式加载统计不可用，次数和最近时间保持未知。"}</p> : null}
                  <div className="grid grid-cols-3 gap-2">
                    <div className="rounded-md border p-2">
                      <p className="text-lg font-semibold tabular-nums">{skillExplicitLoads(selectedSkill, usageAvailable) ?? "—"}</p>
                      <p className="text-xs text-muted-foreground">{skillExplicitLoads(selectedSkill, usageAvailable) === null ? "统计不可用" : "显式加载记录"}</p>
                    </div>
                    <div className="rounded-md border p-2">
                      <p className="text-lg font-semibold tabular-nums">{skillExplicitLoads(selectedSkill, usageAvailable) === null ? "—" : (selectedSkill.tasks ?? "—")}</p>
                      <p className="text-xs text-muted-foreground">覆盖任务数</p>
                    </div>
                    <div className="rounded-md border p-2">
                      <p className="truncate text-sm font-medium" title={fmtTime(selectedSkill.last_used)}>
                        {skillExplicitLoads(selectedSkill, usageAvailable) === null ? "统计未知" : fmtTime(selectedSkill.last_used)}
                      </p>
                      <p className="text-xs text-muted-foreground">最近显式加载</p>
                    </div>
                  </div>
                  {skillExplicitLoads(selectedSkill, usageAvailable) !== null && selectedSkill.usage_agents.length > 0 && (
                    <div className="flex flex-wrap items-center gap-1">
                      <span className="text-xs text-muted-foreground">调用方：</span>
                      {selectedSkill.usage_agents.map((k) => (
                        <Badge key={k} variant="secondary" className="text-xs font-normal">{k}</Badge>
                      ))}
                    </div>
                  )}
                  {usageError ? <div role="alert" data-testid="skill-usage-error" className="space-y-1 text-xs text-destructive"><p>{usageError}</p><p>已有明细保留；这不表示没有历史记录。</p><Button type="button" size="sm" variant="outline" onClick={() => setRefreshRevision((current) => current + 1)}>重试加载明细</Button></div> : null}
                  {usageLoading ? <p className="text-xs text-muted-foreground">读取近期热数据明细…</p> : null}
                  {usageFor === selectedSkill.name && usageCalls.length > 0 ? (
                    <div className="rounded-md border">
                      <div className="border-b px-2 py-1 text-xs text-muted-foreground">近期热表 {usageCalls.length} 条显式加载记录</div>
                      <div className="max-h-56 overflow-y-auto">
                        {usageCalls.map((c, i) => (
                          <div key={`${c.ts}-${i}`} className="flex items-center gap-2 border-b px-2 py-1 text-xs last:border-b-0">
                            <span className="tabular-nums text-muted-foreground">{fmtTime(c.ts)}</span>
                            <Badge variant="outline" className="font-normal">{c.agent_key || "—"}</Badge>
                            <span className="ml-auto text-muted-foreground">
                              {c.task_id > 0 ? `任务 #${c.task_id}` : c.session_id ? "对话会话" : "—"}
                            </span>
                          </div>
                        ))}
                      </div>
                    </div>
                  ) : !usageLoading && !usageError && usageFor === selectedSkill.name ? (
                    <p className="text-xs text-muted-foreground">近期热数据中暂无显式加载记录；历史记录可能已归档，汇总总数仍包含归档。</p>
                  ) : null}
                </div>

                {/* ── 左侧：关联 MCP + 可见性 ── */}
                <div className="space-y-5 lg:min-w-0 lg:flex-1">
                  <div className="space-y-2">
                    <Label className="text-xs text-muted-foreground">
                      关联 MCP
                      <span className="ml-1 font-normal">（执行依赖仍受 Agent 授权与服务开关限制）</span>
                    </Label>
                    {mcpLoading ? <p className="text-xs text-muted-foreground">读取 MCP 列表中…</p> : mcpError ? <p role="alert" className="text-xs text-destructive">{mcpError}；关联编辑已暂停，请刷新确认。</p> : mcpOptions.length === 0 ? (
                      <p className="text-xs text-muted-foreground">暂无 MCP，可在「MCP」页添加。</p>
                    ) : (
                      <div className="flex flex-wrap gap-x-4 gap-y-2">
                        {mcpOptions.map((m) => (
                          <label key={m.id} className="flex cursor-pointer items-center gap-2 text-sm">
                            <Checkbox
                              checked={detailMcps.includes(m.name)}
                              disabled={mcpSaving || mcpLoading || !!mcpError || listLoading || !!listError || selectedSkill.loadable === false}
                              onCheckedChange={(on) => toggleSkillMcp(selectedSkill.name, m.name, !!on)}
                            />
                            {m.name}{!m.enabled ? <span className="text-xs text-muted-foreground">（服务已停用）</span> : null}
                          </label>
                        ))}
                      </div>
                    )}
                  </div>

                  <div className="space-y-2">
                    <Label className="text-xs text-muted-foreground">可见性（按 Agent 授权）</Label>
                    <div className="space-y-2">
                      {agentsLoading ? <p className="text-xs text-muted-foreground">读取 Agent 列表中…</p> : null}
                      {visibilityStatus[selectedSkill.name] === "loading" ? <p className="text-xs text-muted-foreground">读取或更新授权中，暂不接受编辑…</p> : null}
                      {visibilityErrors[selectedSkill.name] ? <div role="alert" data-testid="skill-visibility-error" className="space-y-1 text-xs text-destructive"><p>{visibilityErrors[selectedSkill.name]}</p><p>保留上次显示，授权状态未确认时不接受编辑。</p><Button type="button" size="sm" variant="outline" onClick={() => void refreshVisibility(selectedSkill.name)}>重试读取授权</Button></div> : null}
                      {agentsError ? <p role="alert" className="text-xs text-destructive">{agentsError}；授权编辑已暂停。</p> : null}
                      {agents.map((actor) => {
                        const support = agentBindingSupport(actor, "skill");
                        const checked = (visibility[selectedSkill.name] ?? []).includes(actor.id);
                        return <label key={actor.key} className="flex items-start gap-2 text-sm">
                          <Checkbox aria-label={`${actor.name} Skill 可见性`} className="mt-0.5" checked={checked}
                            disabled={!support.allowed || agentsLoading || !!agentsError || listLoading || !!listError || visibilityStatus[selectedSkill.name] !== "ready"}
                            onCheckedChange={() => void toggleVisibility(selectedSkill.name, actor.id, actor.name)} />
                          <span>{actor.name}{support.reason ? <span className="mt-0.5 block text-xs text-muted-foreground">{support.reason}{checked && !support.allowed ? "；原有绑定保留显示，当前不生效。" : ""}</span> : null}</span>
                        </label>;
                      })}
                      {!agentsLoading && !agentsError && agents.length === 0 ? <span className="text-xs text-muted-foreground">（暂无 Agent）</span> : null}
                    </div>
                  </div>
                </div>
              </div>
            </div>
          )}

          {selected && selected.path !== null && (
            <div className="flex h-full flex-col gap-2">
              <div className="flex items-center gap-2 text-xs text-muted-foreground">
                <span className="font-medium text-foreground">{selected.skill}</span>
                <span>/</span>
                <span className="font-mono">{selected.path}</span>
                <Button size="sm" className="ml-auto" onClick={saveFile} disabled={!dirty || saving || fileLoading || !!fileError || fileFor !== `${selected.skill}/${selected.path}`}>
                  {saving ? "保存中…" : "保存"}
                </Button>
              </div>
              {fileError ? <p role="alert" className="text-xs text-destructive">{fileError}；文件编辑与保存已暂停，重新选择文件后重试。</p> : null}
              {fileLoading ? (
                <p className="text-xs text-muted-foreground">加载中…</p>
              ) : (
                <Textarea
                  disabled={!!fileError || fileFor !== `${selected.skill}/${selected.path}`}
                  className="flex-1 resize-none font-mono text-xs"
                  value={fileContent}
                  onChange={(e) => { setFileContent(e.target.value); setDirty(true); }}
                />
              )}
            </div>
          )}
        </div>
      </div>

      {/* ── 删除二次确认 ── */}
      <AlertDialog open={!!pendingDelete} onOpenChange={(o) => { if (!o) setPendingDelete(null); }}>
        <AlertDialogContent>
          <AlertDialogHeader>
            <AlertDialogTitle>
              {pendingDelete?.kind === "skill" && `删除 Skill「${pendingDelete.skill}」？`}
              {pendingDelete?.kind === "dir" && `删除文件夹「${pendingDelete.path}」？`}
              {pendingDelete?.kind === "file" && `删除文件「${pendingDelete.path}」？`}
            </AlertDialogTitle>
            <AlertDialogDescription>
              {pendingDelete?.kind === "skill"
                ? "将删除该 Skill 的全部文件、MCP 关联与可见性配置。此操作不可撤销。"
                : pendingDelete?.kind === "dir"
                  ? "将一并删除该文件夹下的所有文件。此操作不可撤销。"
                  : "此操作不可撤销。"}
            </AlertDialogDescription>
          </AlertDialogHeader>
          <AlertDialogFooter>
            <AlertDialogCancel disabled={deleting}>取消</AlertDialogCancel>
            <AlertDialogAction
              disabled={deleting}
              onClick={(e) => { e.preventDefault(); void runPendingDelete(); }}
            >
              {deleting ? "删除中…" : "删除"}
            </AlertDialogAction>
          </AlertDialogFooter>
        </AlertDialogContent>
      </AlertDialog>

      {/* ── 新建 Skill 对话框 ── */}
      <Sheet open={newOpen} onOpenChange={setNewOpen}>
        <SheetContent side="right" className="flex w-full flex-col gap-0 data-[side=right]:sm:max-w-xl">
          <SheetHeader className="px-4 pt-4">
            <SheetTitle>新建 Skill</SheetTitle>
          </SheetHeader>
          <Tabs defaultValue="basic" className="flex min-h-0 flex-1 flex-col">
            <TabsList className="mx-4 mt-3 shrink-0 justify-start">
              <TabsTrigger value="basic">基本信息</TabsTrigger>
              <TabsTrigger value="mcp">
                关联 MCP
                {newMcps.length > 0 && (
                  <span className="ml-1.5 rounded-full bg-primary px-1.5 py-0.5 text-[10px] leading-none text-primary-foreground">
                    {newMcps.length}
                  </span>
                )}
              </TabsTrigger>
              <TabsTrigger value="visibility">
                可见性
                {newVisibility.length > 0 && (
                  <span className="ml-1.5 rounded-full bg-primary px-1.5 py-0.5 text-[10px] leading-none text-primary-foreground">
                    {newVisibility.length}
                  </span>
                )}
              </TabsTrigger>
            </TabsList>

            {/* 基本信息 */}
            <TabsContent value="basic" className="flex min-h-0 flex-1 flex-col gap-4 overflow-y-auto px-4 pb-4 pt-4 data-[state=inactive]:hidden">
              <div className="grid gap-1.5">
                <Label htmlFor="sk-name">名称 <span className="text-destructive">*</span></Label>
                <Input id="sk-name" placeholder="sqli-deepdive" value={newName} onChange={(e) => setNewName(e.target.value)} />
                <p className="text-muted-foreground text-xs">小写字母 / 数字 / 连字符，1–64 字符</p>
              </div>
              <div className="grid gap-1.5">
                <Label htmlFor="sk-desc">描述 <span className="text-destructive">*</span></Label>
                <Textarea id="sk-desc" rows={2} className="resize-none"
                  placeholder="这个 skill 做什么、何时使用。"
                  value={newDesc} onChange={(e) => setNewDesc(e.target.value)} />
              </div>
              <div className="grid grid-cols-2 gap-3">
                <div className="grid gap-1.5">
                  <Label className="text-muted-foreground text-xs">license</Label>
                  <Input placeholder="MIT / Proprietary" value={newLicense} onChange={(e) => setNewLicense(e.target.value)} />
                </div>
                <div className="grid gap-1.5">
                  <Label className="text-muted-foreground text-xs">compatibility</Label>
                  <Input placeholder="需要 sqlmap、python3" value={newCompat} onChange={(e) => setNewCompat(e.target.value)} />
                </div>
              </div>
              <div className="flex min-h-0 flex-1 flex-col gap-1.5">
                <Label htmlFor="sk-inst">正文 <span className="text-muted-foreground text-xs font-normal">（留空自动生成骨架）</span></Label>
                <Textarea id="sk-inst"
                  className="min-h-40 flex-1 resize-none font-mono text-sm leading-relaxed"
                  placeholder={"## 执行方法\n\n1. 先探测错误\n2. 区分盲注类型\n\n脚本放 scripts/ 目录。"}
                  value={newInst} onChange={(e) => setNewInst(e.target.value)} />
              </div>
            </TabsContent>

            {/* 关联 MCP */}
            <TabsContent value="mcp" className="overflow-y-auto px-4 pb-4 pt-4 data-[state=inactive]:hidden">
              <p className="mb-3 text-xs text-muted-foreground">为执行角色声明所需 MCP；加载后仍受 Agent 权限、服务开关与审批约束。目标拆解只读取 Skill 指导，不开放 MCP 执行依赖。</p>
              {mcpLoading ? <p className="text-xs text-muted-foreground">读取 MCP 列表中…</p> : mcpError ? <p role="alert" className="text-xs text-destructive">{mcpError}；列表未确认，暂不接受关联编辑。</p> : mcpOptions.length === 0 ? (
                <p className="text-xs text-muted-foreground">暂无 MCP，可在「MCP」页添加。</p>
              ) : (
                <div className="flex flex-col gap-2">
                  {mcpOptions.map((m) => (
                    <label key={m.id} className="flex cursor-pointer items-center gap-2 text-sm">
                      <Checkbox
                        checked={newMcps.includes(m.name)}
                        disabled={mcpLoading || !!mcpError}
                        onCheckedChange={(on) =>
                          setNewMcps((cur) => on ? [...cur, m.name] : cur.filter((n) => n !== m.name))
                        }
                      />
                      {m.name}
                    </label>
                  ))}
                </div>
              )}
            </TabsContent>

            {/* 可见性 */}
            <TabsContent value="visibility" className="overflow-y-auto px-4 pb-4 pt-4 data-[state=inactive]:hidden">
              <p className="mb-3 text-xs text-muted-foreground">选中的 Agent 创建后即可见此 Skill。</p>
              {agentsLoading ? <p className="text-xs text-muted-foreground">读取 Agent 列表中…</p> : agentsError ? <p role="alert" className="text-xs text-destructive">{agentsError}；列表未确认，暂不接受授权编辑。</p> : agents.length === 0 ? (
                <p className="text-xs text-muted-foreground">（暂无 Agent）</p>
              ) : (
                <div className="flex flex-col gap-2">
                  {agents.map((actor) => {
                    const support = agentBindingSupport(actor, "skill");
                    return <label key={actor.key} className="flex items-start gap-2 text-sm"><Checkbox className="mt-0.5" checked={newVisibility.includes(actor.id)} disabled={!support.allowed || agentsLoading || !!agentsError}
                      onCheckedChange={(on) => setNewVisibility((current) => on ? [...current, actor.id] : current.filter((id) => id !== actor.id))} />
                      <span>{actor.name}{support.reason ? <span className="mt-0.5 block text-xs text-muted-foreground">{support.reason}</span> : null}</span></label>;
                  })}
                </div>
              )}
            </TabsContent>
          </Tabs>

          <SheetFooter className="flex-row justify-end gap-2 border-t px-4 py-3">
            <Button variant="outline" onClick={() => setNewOpen(false)}>取消</Button>
            <Button onClick={createNewSkill} disabled={creatingSkill}>{creatingSkill ? "创建中…" : "创建"}</Button>
          </SheetFooter>
        </SheetContent>
      </Sheet>
    </div>
  );
}
