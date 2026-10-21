"use client";

import * as React from "react";

import {
  CheckCircle2Icon,
  FileTextIcon,
  FolderPlusIcon,
  Loader2Icon,
  PencilIcon,
  PlusIcon,
  RefreshCwIcon,
  Trash2Icon,
  UploadIcon,
} from "lucide-react";
import { toast } from "sonner";

import { Button } from "@/components/ui/button";
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from "@/components/ui/dialog";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from "@/components/ui/select";
import { Switch } from "@/components/ui/switch";
import { Textarea } from "@/components/ui/textarea";
import { api } from "@/lib/api";
import type { CodexInstructionsSource, LLMInstructionFileInfo } from "@/lib/types";

const MAX_FILE_BYTES = 1024 * 1024;
const DEFAULT_CODEX_CONFIG = "~/.codex/config.toml";

function validName(name: string) {
  return (
    /^[^/\\]+\.(md|txt)$/i.test(name) &&
    [...name].every((char) => char.charCodeAt(0) >= 32 && char.charCodeAt(0) !== 127)
  );
}

function fileSize(size: number) {
  return `${size.toLocaleString()} B`;
}

export function ModelInstructionsFiles({
  open,
  enabled,
  activeFile,
  registeredFiles,
  codexConfigFile,
  savedCodexConfigFile,
  onChange,
}: {
  open: boolean;
  enabled: boolean;
  activeFile: string;
  registeredFiles: string[];
  codexConfigFile: string;
  savedCodexConfigFile: string;
  onChange: (active: string, files: string[], codexConfig: string) => void;
}) {
  const [directory, setDirectory] = React.useState("");
  const [files, setFiles] = React.useState<LLMInstructionFileInfo[]>([]);
  const [loading, setLoading] = React.useState(false);
  const [loadError, setLoadError] = React.useState("");
  const [codexSource, setCodexSource] = React.useState<CodexInstructionsSource | null>(null);
  const [codexLoading, setCodexLoading] = React.useState(false);
  const [codexError, setCodexError] = React.useState("");
  const [codexConfigPath, setCodexConfigPath] = React.useState(DEFAULT_CODEX_CONFIG);
  const [checkedCodexPath, setCheckedCodexPath] = React.useState("");
  const [busy, setBusy] = React.useState(false);
  const [editorOpen, setEditorOpen] = React.useState(false);
  const [editingName, setEditingName] = React.useState<string | null>(null);
  const [name, setName] = React.useState("");
  const [content, setContent] = React.useState("");
  const [addOpen, setAddOpen] = React.useState(false);
  const [existingName, setExistingName] = React.useState("");
  const [deleteName, setDeleteName] = React.useState("");
  const importInput = React.useRef<HTMLInputElement>(null);
  const inspectRequest = React.useRef(0);

  const loadFiles = React.useCallback(async () => {
    setLoading(true);
    setLoadError("");
    try {
      const result = await api.llmInstructionFiles();
      setDirectory(result.directory);
      setFiles(result.files ?? []);
    } catch (e) {
      setLoadError((e as Error).message);
    } finally {
      setLoading(false);
    }
  }, []);

  const loadCodexSource = React.useCallback(async (path: string) => {
    const request = ++inspectRequest.current;
    setCodexLoading(true);
    setCodexError("");
    setCheckedCodexPath(path.trim());
    try {
      const result = await api.inspectCodexConfig(path.trim());
      if (request === inspectRequest.current) setCodexSource(result);
    } catch (e) {
      if (request === inspectRequest.current) {
        setCodexSource(null);
        setCodexError((e as Error).message);
      }
    } finally {
      if (request === inspectRequest.current) setCodexLoading(false);
    }
  }, []);

  React.useEffect(() => {
    if (open) {
      void loadFiles();
    } else {
      setEditorOpen(false);
      setAddOpen(false);
      setDeleteName("");
    }
  }, [open, loadFiles]);

  React.useEffect(() => {
    if (!open) {
      inspectRequest.current += 1;
      setCodexSource(null);
      setCodexError("");
      setCodexLoading(false);
      setCheckedCodexPath("");
      return;
    }
    const path = savedCodexConfigFile || DEFAULT_CODEX_CONFIG;
    setCodexConfigPath(path);
    void loadCodexSource(path);
  }, [open, savedCodexConfigFile, loadCodexSource]);

  const names = [...new Set([...registeredFiles, ...(activeFile ? [activeFile] : [])])];
  const availableFiles = files.filter((file) => !names.includes(file.name));
  const contentBytes = new TextEncoder().encode(content).length;
  const fromCodex = Boolean(codexConfigFile.trim());
  const currentCodexCheck = checkedCodexPath === codexConfigPath.trim();
  const codexAvailable = currentCodexCheck && codexSource !== null && !codexError;
  let activeSummary = "未启用，使用默认基础指令";
  if (enabled && fromCodex) activeSummary = "已启用：从 Codex config.toml 加载";
  else if (enabled && activeFile) activeSummary = `已启用：${activeFile}`;

  function registerFile(fileName: string) {
    onChange(activeFile, [...new Set([...names, fileName])], codexConfigFile);
  }

  function createFile() {
    setEditingName(null);
    setName("");
    setContent("");
    setEditorOpen(true);
  }

  async function editFile(fileName: string) {
    setBusy(true);
    try {
      const result = await api.getLLMInstructionFile(fileName);
      setEditingName(fileName);
      setName(fileName);
      setContent(result.content);
      setEditorOpen(true);
    } catch (e) {
      toast.error(`打开文件失败：${(e as Error).message}`);
    } finally {
      setBusy(false);
    }
  }

  async function importFile(file: File) {
    if (!validName(file.name)) {
      toast.error("请选择 .md 或 .txt 文件");
      return;
    }
    if (file.size > MAX_FILE_BYTES) {
      toast.error("文件不能超过 1 MiB");
      return;
    }
    setBusy(true);
    try {
      const text = new TextDecoder("utf-8", { fatal: true }).decode(await file.arrayBuffer());
      setName(file.name);
      setContent(text);
    } catch {
      toast.error("无法读取文件，请使用 UTF-8 编码");
    } finally {
      setBusy(false);
    }
  }

  async function saveFile() {
    const fileName = name.trim();
    if (!validName(fileName)) {
      toast.error("文件名必须以 .md 或 .txt 结尾，不能包含路径");
      return;
    }
    if (contentBytes > MAX_FILE_BYTES) {
      toast.error("文件不能超过 1 MiB");
      return;
    }
    if (busy) return;
    setBusy(true);
    try {
      const result = editingName
        ? await api.updateLLMInstructionFile(editingName, content)
        : await api.createLLMInstructionFile(fileName, content);
      setFiles((current) =>
        [...current.filter((file) => file.name !== result.file.name), result.file].sort((a, b) =>
          a.name.localeCompare(b.name),
        ),
      );
      if (!editingName) registerFile(result.file.name);
      setEditorOpen(false);
      toast.success(editingName ? "文件已保存" : "文件已创建，请启用后保存模型配置");
    } catch (e) {
      toast.error(`保存文件失败：${(e as Error).message}`);
    } finally {
      setBusy(false);
    }
  }

  async function deleteFile() {
    if (!deleteName || busy) return;
    setBusy(true);
    try {
      await api.deleteLLMInstructionFile(deleteName);
      setFiles((current) => current.filter((file) => file.name !== deleteName));
      onChange(
        activeFile,
        names.filter((fileName) => fileName !== deleteName),
        codexConfigFile,
      );
      setDeleteName("");
      toast.success("文件已删除");
    } catch (e) {
      toast.error(`删除文件失败：${(e as Error).message}`);
    } finally {
      setBusy(false);
    }
  }

  return (
    <section className="grid gap-3 rounded-lg border p-3" aria-labelledby="model-instructions-title">
      <div className="flex flex-wrap items-center justify-between gap-2">
        <h3 id="model-instructions-title" className="font-medium text-sm">
          模型指令文件
        </h3>
        <div className="flex gap-1">
          <Button
            type="button"
            size="sm"
            variant="ghost"
            disabled={busy || loading || codexLoading}
            onClick={() => {
              void loadFiles();
              if (codexConfigPath.trim()) void loadCodexSource(codexConfigPath);
            }}
            aria-label="刷新模型指令文件"
          >
            <RefreshCwIcon className={loading || codexLoading ? "animate-spin" : ""} />
          </Button>
          <Button type="button" size="sm" variant="outline" onClick={createFile} disabled={busy || loading}>
            <PlusIcon /> 新建文件
          </Button>
        </div>
      </div>
      <p className="text-muted-foreground text-xs">
        启用后替换 Codex 模式的默认基础指令。Agent Prompt 独立保留，仍用于任务角色与流程。
      </p>
      {!enabled && (
        <p className="text-muted-foreground text-xs">
          使用 OpenAI Responses 格式并选择 Codex User-Agent 后可启用；其他模式下仍可管理文件。
        </p>
      )}
      <div className="rounded-md border bg-muted/30 px-3 py-2 text-muted-foreground text-xs">
        共 {names.length} 个文件 · {activeSummary}
      </div>
      <div className="grid gap-2 rounded-lg border p-3">
        <div className="flex items-center justify-between gap-3">
          <Label htmlFor="model-instructions-from-codex" className="text-sm">
            从 Codex config.toml 加载指令
          </Label>
          <Switch
            id="model-instructions-from-codex"
            checked={enabled && fromCodex}
            disabled={!enabled || busy || codexLoading || (!codexAvailable && !fromCodex)}
            onCheckedChange={(checked) =>
              onChange(checked ? "" : activeFile, names, checked ? codexConfigPath.trim() : "")
            }
          />
        </div>
        <p className="text-muted-foreground text-xs">
          从 ARTEX 所在机器的 TOML 文件中读取 model_instructions_file 与 developer_instructions
          两项指令。修改源文件后，重新保存模型配置或重启 ARTEX 生效。
        </p>
        <div className="grid gap-2">
          <Label htmlFor="model-instructions-codex-config" className="text-xs">
            Codex 配置文件路径
          </Label>
          <div className="flex gap-2">
            <Input
              id="model-instructions-codex-config"
              className="font-mono text-xs"
              value={codexConfigPath}
              placeholder={DEFAULT_CODEX_CONFIG}
              disabled={busy || codexLoading}
              onChange={(event) => {
                setCodexConfigPath(event.target.value);
                if (fromCodex) onChange("", names, event.target.value);
              }}
            />
            <Button
              type="button"
              size="sm"
              variant="outline"
              disabled={busy || codexLoading || !codexConfigPath.trim()}
              onClick={() => void loadCodexSource(codexConfigPath)}
            >
              {codexLoading && <Loader2Icon className="animate-spin" />} 检查配置
            </Button>
          </div>
        </div>
        {currentCodexCheck && codexSource?.config_path && (
          <p className="break-all text-muted-foreground text-xs">配置：{codexSource.config_path}</p>
        )}
        {currentCodexCheck && codexSource?.model_instructions_file && (
          <p className="break-all text-muted-foreground text-xs">指令：{codexSource.model_instructions_file}</p>
        )}
        {codexLoading && (
          <p className="flex items-center gap-1 text-muted-foreground text-xs">
            <Loader2Icon className="size-3 animate-spin" /> 正在读取 Codex 配置…
          </p>
        )}
        {!codexLoading && codexAvailable && codexSource && (
          <div className="grid gap-1 text-emerald-600 text-xs dark:text-emerald-400">
            <p className="flex items-center gap-1">
              <CheckCircle2Icon className="size-3" /> 指令配置检查通过
            </p>
            <p>
              基础指令 {fileSize(codexSource.model_instructions_bytes)} · 开发者指令{" "}
              {fileSize(codexSource.developer_instructions_bytes)}
            </p>
            {!codexSource.has_model_instructions && (
              <p className="text-muted-foreground">未指定基础指令文件，使用 ARTEX 当前默认基础指令。</p>
            )}
          </div>
        )}
        {!codexLoading && !codexAvailable && (
          <p role="status" className="text-muted-foreground text-xs">
            {currentCodexCheck ? codexError || "配置检查未通过" : "路径已修改，请检查配置"}
          </p>
        )}
      </div>
      {loadError && (
        <p role="alert" className="text-destructive text-xs">
          文件列表加载失败：{loadError}
        </p>
      )}
      {loading && files.length === 0 && (
        <p className="flex items-center gap-2 text-muted-foreground text-xs">
          <Loader2Icon className="size-3 animate-spin" /> 加载文件信息…
        </p>
      )}
      {names.map((fileName) => {
        const file = files.find((entry) => entry.name === fileName);
        const active = !fromCodex && activeFile === fileName;
        const available = file?.exists && !file.error;
        const path = file?.path ?? (directory ? `${directory}/${fileName}` : fileName);
        return (
          <div key={fileName} className="flex items-center gap-3 rounded-lg border p-3">
            <Switch
              checked={enabled && active}
              disabled={!enabled || busy || loading || (!available && !active)}
              onCheckedChange={(checked) => onChange(checked ? fileName : "", names, "")}
              aria-label={`启用模型指令文件 ${fileName}`}
            />
            <FileTextIcon className="size-4 shrink-0 text-muted-foreground" />
            <div className="grid min-w-0 flex-1 gap-0.5">
              <span className="break-all font-medium text-sm">{fileName}</span>
              <span className="break-all text-muted-foreground text-xs">{path}</span>
              {available ? (
                <span className="flex items-center gap-1 text-emerald-600 text-xs dark:text-emerald-400">
                  <CheckCircle2Icon className="size-3" /> 文件可用 · {fileSize(file.size)}
                </span>
              ) : (
                <span className="text-muted-foreground text-xs">
                  {loading ? "正在检查文件…" : (file?.error ?? (file ? "文件不可用" : "未找到文件"))}
                </span>
              )}
            </div>
            <div className="flex shrink-0 gap-1">
              <Button
                type="button"
                size="icon-sm"
                variant="ghost"
                disabled={busy || loading || !available}
                onClick={() => void editFile(fileName)}
                aria-label={`编辑 ${fileName}`}
              >
                <PencilIcon />
              </Button>
              <Button
                type="button"
                size="icon-sm"
                variant="ghost"
                disabled={busy || loading || active}
                onClick={() => setDeleteName(fileName)}
                aria-label={`删除 ${fileName}`}
                title={active ? "请先停用文件并保存模型配置" : "删除文件"}
              >
                <Trash2Icon />
              </Button>
            </div>
          </div>
        );
      })}
      <Button
        type="button"
        size="sm"
        variant="outline"
        className="justify-self-start"
        disabled={busy || loading || availableFiles.length === 0}
        onClick={() => {
          setExistingName("");
          setAddOpen(true);
        }}
      >
        <FolderPlusIcon /> 添加已有文件
      </Button>
      <p className="text-muted-foreground text-xs">
        Codex 配置与手动文件单选启用；开关和文件列表在保存模型配置后生效。文件正文独立保存，复用此文件的配置会同步更新。
      </p>

      <Dialog open={editorOpen} onOpenChange={(value) => !busy && setEditorOpen(value)}>
        <DialogContent className="sm:max-w-2xl">
          <DialogHeader>
            <DialogTitle>{editingName ? `编辑：${editingName}` : "新建模型指令文件"}</DialogTitle>
            <DialogDescription>
              文件内容用于替换 Codex 基础指令；不会覆盖 Agent Prompt。支持 UTF-8 的 .md / .txt 文件，最多 1 MiB。
            </DialogDescription>
          </DialogHeader>
          <div className="grid gap-2">
            <Label htmlFor="model-instructions-filename">文件名</Label>
            <div className="flex gap-2">
              <Input
                id="model-instructions-filename"
                className="font-mono"
                placeholder="instruction.md"
                value={name}
                disabled={busy || editingName !== null}
                onChange={(event) => setName(event.target.value)}
              />
              {!editingName && (
                <>
                  <input
                    ref={importInput}
                    type="file"
                    accept=".md,.txt,text/plain,text/markdown"
                    className="hidden"
                    aria-label="导入模型指令文件"
                    onChange={(event) => {
                      const file = event.target.files?.[0];
                      event.target.value = "";
                      if (file) void importFile(file);
                    }}
                  />
                  <Button type="button" variant="outline" disabled={busy} onClick={() => importInput.current?.click()}>
                    <UploadIcon /> 导入
                  </Button>
                </>
              )}
            </div>
          </div>
          <div className="grid gap-2">
            <Label htmlFor="model-instructions-content">指令内容</Label>
            <Textarea
              id="model-instructions-content"
              className="min-h-72 font-mono text-xs"
              value={content}
              disabled={busy}
              onChange={(event) => setContent(event.target.value)}
              placeholder="填写此模型的基础指令"
            />
            <p className="text-muted-foreground text-xs">{fileSize(contentBytes)} / 1 MiB</p>
          </div>
          <DialogFooter>
            <Button type="button" variant="outline" disabled={busy} onClick={() => setEditorOpen(false)}>
              取消
            </Button>
            <Button
              type="button"
              disabled={busy || !name.trim() || contentBytes > MAX_FILE_BYTES}
              onClick={() => void saveFile()}
            >
              {busy && <Loader2Icon className="animate-spin" />} 保存文件
            </Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>

      <Dialog open={addOpen} onOpenChange={setAddOpen}>
        <DialogContent>
          <DialogHeader>
            <DialogTitle>添加已有文件</DialogTitle>
            <DialogDescription>将文件仓库中的文件加入当前模型配置；不会自动启用。</DialogDescription>
          </DialogHeader>
          <Select value={existingName} onValueChange={setExistingName}>
            <SelectTrigger aria-label="选择已有模型指令文件">
              <SelectValue placeholder="选择文件" />
            </SelectTrigger>
            <SelectContent>
              {availableFiles.map((file) => (
                <SelectItem key={file.name} value={file.name} disabled={!file.exists || Boolean(file.error)}>
                  {file.name}
                </SelectItem>
              ))}
            </SelectContent>
          </Select>
          <DialogFooter>
            <Button type="button" variant="outline" onClick={() => setAddOpen(false)}>
              取消
            </Button>
            <Button
              type="button"
              disabled={!existingName}
              onClick={() => {
                registerFile(existingName);
                setAddOpen(false);
              }}
            >
              添加
            </Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>

      <Dialog open={Boolean(deleteName)} onOpenChange={(value) => !busy && !value && setDeleteName("")}>
        <DialogContent>
          <DialogHeader>
            <DialogTitle>删除模型指令文件</DialogTitle>
            <DialogDescription>
              删除 {deleteName} 的文件正文。其他模型配置中已添加此文件的记录也会失效；已启用的文件需先停用并保存配置。
            </DialogDescription>
          </DialogHeader>
          <DialogFooter>
            <Button type="button" variant="outline" disabled={busy} onClick={() => setDeleteName("")}>
              取消
            </Button>
            <Button type="button" variant="destructive" disabled={busy} onClick={() => void deleteFile()}>
              {busy && <Loader2Icon className="animate-spin" />} 删除文件
            </Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>
    </section>
  );
}
