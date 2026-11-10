"use client";
import * as React from "react";
import { toast } from "sonner";
import { api } from "@/lib/api";
import { knowledgeAPI, type KnowledgeDoc, type KnowledgeConfig, type KnowledgeResult } from "@/lib/knowledge-api";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { Textarea } from "@/components/ui/textarea";
import { Card, CardHeader, CardTitle, CardContent } from "@/components/ui/card";

export default function KnowledgePage() {
  const [task, setTask] = React.useState(0),
    [tasks, setTasks] = React.useState<{ id: string; name: string }[]>([]);
  const [docs, setDocs] = React.useState<KnowledgeDoc[]>([]),
    [config, setConfig] = React.useState<KnowledgeConfig | null>(null);
  const [pdf, setPdf] = React.useState(false),
    [error, setError] = React.useState(""),
    [busy, setBusy] = React.useState(false);
  const [edit, setEdit] = React.useState<Partial<KnowledgeDoc> | null>(null),
    [query, setQuery] = React.useState(""),
    [result, setResult] = React.useState<KnowledgeResult | null>(null);
  const [key, setKey] = React.useState("");
  const searchAbort = React.useRef<AbortController | null>(null);
  const generation = React.useRef(0);
  const refresh = React.useCallback(async () => {
    const g = generation.current;
    const v = await knowledgeAPI.list(task);
    if (g === generation.current) setDocs(v.items);
  }, [task]);
  React.useEffect(() => {
    api
      .tasks()
      .then((v) => setTasks(v.tasks.map((t) => ({ id: String(t.id), name: t.name || t.description }))))
      .catch((e) => setError(e.message));
    knowledgeAPI
      .config()
      .then((v) => {
        setConfig(v.config);
        setPdf(v.pdf_available);
      })
      .catch((e) => setError(e.message));
  }, []);
  React.useEffect(() => {
    generation.current++;
    searchAbort.current?.abort();
    setResult(null);
    setEdit(null);
    const controller = new AbortController();
    setDocs([]);
    setError("");
    knowledgeAPI
      .list(task, controller.signal)
      .then((v) => setDocs(v.items))
      .catch((e) => {
        if (!controller.signal.aborted) setError(e.message);
      });
    return () => {
      controller.abort();
      searchAbort.current?.abort();
    };
  }, [task]);
  async function action(fn: () => Promise<unknown>) {
    setBusy(true);
    setError("");
    try {
      await fn();
      await refresh();
    } catch (e) {
      setError((e as Error).message);
    } finally {
      setBusy(false);
    }
  }
  async function search() {
    searchAbort.current?.abort();
    const c = new AbortController();
    searchAbort.current = c;
    setResult(null);
    setError("");
    try {
      const value = await knowledgeAPI.search(task, query, c.signal);
      if (!c.signal.aborted) setResult(value);
    } catch (e) {
      if (!c.signal.aborted) setError((e as Error).message);
    }
  }
  return (
    <div className="space-y-5 p-5">
      <div>
        <h1 className="text-2xl font-semibold">文档知识库</h1>
        <p className="text-sm text-muted-foreground">
          导入方法文档和任务背景，让 Agent 按需查阅。执行中积累的经验仍保存在经验库。
        </p>
      </div>
      {error && (
        <div role="alert" className="rounded border border-destructive p-3">
          {error}
        </div>
      )}
      <div className="flex flex-wrap items-center gap-3">
        <label>
          资料范围{" "}
          <select
            aria-label="资料范围"
            className="rounded border bg-background p-2"
            value={task}
            disabled={busy}
            onChange={(e) => setTask(Number(e.target.value))}
          >
            <option value={0}>全局资料</option>
            {tasks.map((t) => (
              <option key={t.id} value={t.id}>
                任务 #{t.id} · {t.name}
              </option>
            ))}
          </select>
        </label>
        <Button
          disabled={busy}
          onClick={() => setEdit({ title: "", category: "", content: "", task_id: task, file_name: "" })}
        >
          新建资料
        </Button>
        <label className="cursor-pointer rounded border px-3 py-2 text-sm">
          导入文档
          <input
            aria-label="导入文档"
            className="ml-2 max-w-52"
            type="file"
            accept=".txt,.md,.markdown,.csv,.json,.yaml,.yml,.docx,.pdf"
            disabled={busy}
            onChange={(e) => {
              const f = e.target.files?.[0];
              if (f)
                void action(async () => {
                  await knowledgeAPI.upload(f, task);
                  toast.success("资料已导入，可用关键词检索");
                });
              e.target.value = "";
            }}
          />
        </label>
        <Button variant="outline" disabled={busy} onClick={() => void action(refresh)}>
          刷新
        </Button>
      </div>
      <p className="text-xs text-muted-foreground">
        选定任务时同时检索全局资料和该任务资料。上传最多 8 MiB，提取文字最多 256 KiB，最多 128 个分块；总资料文字上限 16
        MiB。原始上传文件不保留。{!pdf && " 此实例尚未安装 PDF 解析器，可先导入文本或 DOCX。"}
      </p>
      <div className="grid gap-4 xl:grid-cols-2">
        <Card>
          <CardHeader>
            <CardTitle>资料列表</CardTitle>
          </CardHeader>
          <CardContent className="space-y-3">
            {docs.length === 0 && <p className="text-sm text-muted-foreground">此范围没有资料。</p>}
            {docs.map((d) => (
              <div key={d.id} className="space-y-2 rounded border p-3">
                <p className="font-medium">{d.title}</p>
                <p className="text-xs text-muted-foreground">
                  {d.task_id ? `任务 #${d.task_id}` : "全局"} · {d.category || "未分类"} · {d.bytes} 字节 · {d.chunks}{" "}
                  块 · {d.indexed ? "语义索引可用" : "关键词可用 / 语义未索引"}
                </p>
                <div className="flex gap-2">
                  <Button
                    size="sm"
                    variant="outline"
                    disabled={busy}
                    onClick={() => void action(async () => setEdit(await knowledgeAPI.get(d.id)))}
                  >
                    查看 / 编辑
                  </Button>
                  <Button
                    size="sm"
                    variant="outline"
                    disabled={busy || !config?.enabled || !config.vector_available}
                    onClick={() =>
                      void action(async () => {
                        if (config) await knowledgeAPI.index(d, config);
                        toast.success("索引已建立");
                      })
                    }
                  >
                    建立语义索引
                  </Button>
                  <Button
                    size="sm"
                    variant="ghost"
                    disabled={busy}
                    onClick={() => {
                      if (window.confirm(`删除资料「${d.title}」及其索引？`)) void action(() => knowledgeAPI.remove(d));
                    }}
                  >
                    删除
                  </Button>
                </div>
              </div>
            ))}
          </CardContent>
        </Card>
        <Card>
          <CardHeader>
            <CardTitle>{edit ? "编辑资料" : "检索资料"}</CardTitle>
          </CardHeader>
          <CardContent className="space-y-3">
            {edit ? (
              <>
                <label className="block">
                  标题
                  <Input
                    aria-label="资料标题"
                    value={edit.title || ""}
                    maxLength={120}
                    onChange={(e) => setEdit({ ...edit, title: e.target.value })}
                  />
                </label>
                <label className="block">
                  分类
                  <Input
                    aria-label="资料分类"
                    value={edit.category || ""}
                    onChange={(e) => setEdit({ ...edit, category: e.target.value })}
                  />
                </label>
                <label className="block">
                  内容
                  <Textarea
                    aria-label="资料内容"
                    className="min-h-72"
                    value={edit.content || ""}
                    onChange={(e) => setEdit({ ...edit, content: e.target.value })}
                  />
                </label>
                <p className="text-xs text-muted-foreground">
                  保存后旧语义索引失效，可重新建立；资料不会改变任务授权或工具审批。
                </p>
                <div className="flex gap-2">
                  <Button
                    disabled={busy}
                    onClick={() =>
                      void action(async () => {
                        const saved = await knowledgeAPI.save({
                          id: edit.id,
                          version: edit.version,
                          title: edit.title,
                          category: edit.category,
                          content: edit.content,
                          task_id: edit.task_id,
                          file_name: edit.file_name || "",
                        });
                        setEdit(saved);
                        toast.success("资料已保存");
                      })
                    }
                  >
                    保存资料
                  </Button>
                  <Button variant="outline" disabled={busy} onClick={() => setEdit(null)}>
                    返回检索
                  </Button>
                </div>
              </>
            ) : (
              <>
                <Input
                  aria-label="检索主题"
                  placeholder="输入主题或关键词"
                  value={query}
                  onChange={(e) => setQuery(e.target.value)}
                />
                <Button disabled={!query.trim()} onClick={() => void search()}>
                  检索
                </Button>
                {result && (
                  <>
                    <p role="status" className="text-sm">
                      {result.mode === "semantic" ? "语义检索" : "关键词检索"} · {result.notice}
                    </p>
                    {result.hits.length === 0 && <p>没有相关资料。</p>}
                    {result.hits.map((h) => (
                      <article key={`${h.doc_id}-${h.chunk}`} className="rounded border p-3">
                        <p className="font-medium">{h.title}</p>
                        <p className="text-xs text-muted-foreground">
                          文档 #{h.doc_id} · 版本 {h.version} · 分块 {h.chunk + 1} ·{" "}
                          {h.task_id ? `任务 #${h.task_id}` : "全局"}
                        </p>
                        <pre className="mt-2 whitespace-pre-wrap break-words text-sm">{h.content}</pre>
                      </article>
                    ))}
                  </>
                )}
              </>
            )}
          </CardContent>
        </Card>
      </div>
      {config && (
        <Card>
          <CardHeader>
            <CardTitle>语义检索配置</CardTitle>
          </CardHeader>
          <CardContent className="space-y-3">
            <p className="text-sm text-muted-foreground">
              启用后，建立索引会把资料文字发送到下方配置的模型服务，检索会发送查询文字。关键词检索无需模型。修改配置后需重新建立语义索引。
              {!config.vector_available && " 数据库尚无 pgvector：由维护者安装扩展并重启后可用。"}
            </p>
            <label className="flex gap-2">
              <input
                type="checkbox"
                checked={config.enabled}
                onChange={(e) => setConfig({ ...config, enabled: e.target.checked })}
              />
              启用语义检索
            </label>
            <div className="grid gap-3 md:grid-cols-2">
              <label>
                模型服务地址
                <Input
                  aria-label="向量服务地址"
                  placeholder="https://provider.example/v1"
                  value={config.base_url}
                  onChange={(e) => setConfig({ ...config, base_url: e.target.value })}
                />
              </label>
              <label>
                模型名称
                <Input
                  aria-label="向量模型"
                  value={config.model}
                  onChange={(e) => setConfig({ ...config, model: e.target.value })}
                />
              </label>
              <label>
                向量维度
                <Input
                  aria-label="向量维度"
                  type="number"
                  min={1}
                  max={2048}
                  value={config.dimension}
                  onChange={(e) => setConfig({ ...config, dimension: Number(e.target.value) })}
                />
              </label>
              <label>
                API Key
                <Input
                  aria-label="向量 API Key"
                  type="password"
                  autoComplete="off"
                  placeholder={config.has_key ? "已配置；留空保留" : "本地服务可留空"}
                  value={key}
                  onChange={(e) => setKey(e.target.value)}
                />
              </label>
            </div>
            <Button
              disabled={busy}
              onClick={() =>
                void action(async () => {
                  const saved = await knowledgeAPI.saveConfig(config, key || undefined);
                  setConfig(saved);
                  setKey("");
                  toast.success("检索配置已保存");
                })
              }
            >
              保存检索配置
            </Button>
          </CardContent>
        </Card>
      )}
    </div>
  );
}
