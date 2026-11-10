"use client";
import * as React from "react";
import { toast } from "sonner";
import { api, http } from "@/lib/api";
import { reportAPI, blankTemplate, type ReportTemplate } from "@/lib/report-api";
import type { Finding } from "@/lib/types";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { Textarea } from "@/components/ui/textarea";
import { Card, CardHeader, CardTitle, CardContent } from "@/components/ui/card";

export default function ReportsPage() {
  const [templates, setTemplates] = React.useState<ReportTemplate[]>([]),
    [edit, setEdit] = React.useState<ReportTemplate>({ ...blankTemplate });
  const [templateID, setTemplateID] = React.useState(0),
    [tasks, setTasks] = React.useState<{ id: string; name: string }[]>([]),
    [task, setTask] = React.useState("");
  const [findings, setFindings] = React.useState<Finding[]>([]),
    [selected, setSelected] = React.useState<string[]>([]),
    [include, setInclude] = React.useState(false);
  const [error, setError] = React.useState(""),
    [busy, setBusy] = React.useState(false),
    [total, setTotal] = React.useState(0);
  const loadTemplates = async () => {
    const v = await reportAPI.templates();
    setTemplates(v.items);
  };
  React.useEffect(() => {
    loadTemplates().catch((e) => setError(e.message));
    api
      .tasks()
      .then((v) => setTasks(v.tasks.map((t) => ({ id: String(t.id), name: t.name || t.description }))))
      .catch((e) => setError(e.message));
  }, []);
  React.useEffect(() => {
    setFindings([]);
    setSelected([]);
    setTotal(0);
    if (!task) return;
    const c = new AbortController();
    http<{ items: Finding[]; total: number }>(
      `/exploration/findings?task_id=${encodeURIComponent(task)}&limit=100&page=1${include ? "" : "&verification=confirmed"}`,
      { signal: c.signal },
    )
      .then((v) => {
        setFindings(v.items);
        setTotal(v.total);
      })
      .catch((e) => {
        if (!c.signal.aborted) setError(e.message);
      });
    return () => c.abort();
  }, [task, include]);
  async function action(fn: () => Promise<unknown>) {
    setBusy(true);
    setError("");
    try {
      await fn();
    } catch (e) {
      setError((e as Error).message);
    } finally {
      setBusy(false);
    }
  }
  const template = templates.find((t) => t.id === templateID);
  const field = (name: keyof ReportTemplate, label: string) => (
    <label key={name} className="block">
      {label}
      <Input
        aria-label={label}
        value={String(edit[name])}
        onChange={(e) => setEdit({ ...edit, [name]: e.target.value })}
      />
    </label>
  );
  return (
    <div className="space-y-5 p-5">
      <div>
        <h1 className="text-2xl font-semibold">Word 报告</h1>
        <p className="text-sm text-muted-foreground">
          用保存的漏洞、复现步骤和证据截图生成 DOCX。生成报告不会重新测试目标，也不会改变漏洞验证状态。
        </p>
      </div>
      {error && (
        <div role="alert" className="rounded border border-destructive p-3">
          {error}
        </div>
      )}
      <div className="grid gap-5 xl:grid-cols-2">
        <Card>
          <CardHeader>
            <CardTitle>报告模板</CardTitle>
          </CardHeader>
          <CardContent className="space-y-3">
            <div className="flex flex-wrap gap-2">
              {templates.map((t) => (
                <Button variant="outline" key={t.id} disabled={busy} onClick={() => setEdit({ ...t })}>
                  {t.name}
                </Button>
              ))}
              <Button variant="secondary" disabled={busy} onClick={() => setEdit({ ...blankTemplate })}>
                新建模板
              </Button>
            </div>
            <div className="grid gap-3 md:grid-cols-2">
              {field("name", "模板名称")}
              {field("title", "报告标题")}
              {field("company", "出具单位")}
              {field("client", "客户名称")}
              {field("project", "项目名称")}
              {field("footer", "页脚文字")}
            </div>
            <label className="block">
              测试人员
              <Textarea
                aria-label="测试人员"
                value={edit.personnel}
                onChange={(e) => setEdit({ ...edit, personnel: e.target.value })}
              />
            </label>
            <label className="block">
              模板语言{" "}
              <select
                aria-label="模板语言"
                className="rounded border bg-background p-2"
                value={edit.locale}
                onChange={(e) => setEdit({ ...edit, locale: e.target.value })}
              >
                <option value="zh-CN">简体中文</option>
                <option value="zh-TW">繁體中文</option>
                <option value="en-US">English</option>
              </select>
            </label>
            <p className="text-xs text-muted-foreground">
              语言用于固定标题；报告正文、请求和响应保持原文，不自动翻译。Word 打开后可更新目录域。
            </p>
            <label className="block text-sm">
              Logo（PNG/JPEG，最多 256 KiB）
              <input
                aria-label="报告 Logo"
                type="file"
                accept="image/png,image/jpeg"
                disabled={busy}
                onChange={(e) => {
                  const f = e.target.files?.[0];
                  if (!f) return;
                  if (f.size > 262144) {
                    setError("Logo 超过 256 KiB");
                    return;
                  }
                  const reader = new FileReader();
                  reader.onload = () => setEdit((old) => ({ ...old, logo: String(reader.result).split(",")[1] }));
                  reader.readAsDataURL(f);
                  e.target.value = "";
                }}
              />
            </label>
            {edit.logo && (
              <div className="flex items-center gap-3">
                <img
                  className="max-h-20 max-w-52 object-contain"
                  alt="模板 Logo 预览"
                  src={`data:image/png;base64,${edit.logo}`}
                />
                <Button variant="ghost" onClick={() => setEdit({ ...edit, logo: "" })}>
                  移除 Logo
                </Button>
              </div>
            )}
            <div className="flex gap-2">
              <Button
                disabled={busy || !edit.name.trim()}
                onClick={() =>
                  void action(async () => {
                    const saved = await reportAPI.save(edit);
                    setEdit(saved);
                    setTemplateID(saved.id);
                    await loadTemplates();
                    toast.success("模板已保存");
                  })
                }
              >
                保存模板
              </Button>
              {edit.id > 0 && (
                <Button
                  variant="outline"
                  disabled={busy}
                  onClick={() => {
                    if (window.confirm(`删除模板「${edit.name}」？已下载的报告不受影响。`))
                      void action(async () => {
                        await reportAPI.remove(edit);
                        setEdit({ ...blankTemplate });
                        setTemplateID(0);
                        await loadTemplates();
                      });
                  }}
                >
                  删除模板
                </Button>
              )}
            </div>
          </CardContent>
        </Card>
        <Card>
          <CardHeader>
            <CardTitle>选择报告内容</CardTitle>
          </CardHeader>
          <CardContent className="space-y-3">
            <label className="block">
              使用模板{" "}
              <select
                aria-label="使用模板"
                className="w-full rounded border bg-background p-2"
                value={templateID}
                disabled={busy}
                onChange={(e) => setTemplateID(Number(e.target.value))}
              >
                <option value={0}>请选择已保存模板</option>
                {templates.map((t) => (
                  <option key={t.id} value={t.id}>
                    {t.name}
                  </option>
                ))}
              </select>
            </label>
            <label className="block">
              来源任务{" "}
              <select
                aria-label="报告来源任务"
                className="w-full rounded border bg-background p-2"
                value={task}
                disabled={busy}
                onChange={(e) => setTask(e.target.value)}
              >
                <option value="">请选择任务</option>
                {tasks.map((t) => (
                  <option key={t.id} value={t.id}>
                    #{t.id} · {t.name}
                  </option>
                ))}
              </select>
            </label>
            <label className="flex gap-2 text-sm">
              <input type="checkbox" checked={include} disabled={busy} onChange={(e) => setInclude(e.target.checked)} />
              包含待验证、受阻等其他状态（报告会标明）
            </label>
            <p className="text-xs text-muted-foreground">
              默认只列已验证记录。显示 {findings.length} / {total} 条，每份报告最多 100 条。
            </p>
            <Button
              variant="outline"
              disabled={busy || !findings.length}
              onClick={() => setSelected(findings.map((f) => f.finding_id || f.id))}
            >
              选择本页全部
            </Button>
            <div className="max-h-96 space-y-2 overflow-auto">
              {findings.map((f) => {
                const id = f.finding_id || f.id;
                return (
                  <label key={id} className="flex gap-3 rounded border p-3">
                    <input
                      type="checkbox"
                      checked={selected.includes(id)}
                      disabled={busy}
                      onChange={(e) =>
                        setSelected((old) => (e.target.checked ? [...old, id] : old.filter((v) => v !== id)))
                      }
                    />
                    <span>
                      {f.name || f.vulnclass}
                      <small className="block text-muted-foreground">
                        #{id} · {f.severity} · {f.verification_status} · 证据版本 {f.evidence_version}
                      </small>
                    </span>
                  </label>
                );
              })}
            </div>
            <Button
              disabled={busy || !template || !selected.length}
              onClick={() =>
                void action(async () => {
                  if (!template) return;
                  await reportAPI.generate(
                    template,
                    findings
                      .filter((f) => selected.includes(f.finding_id || f.id))
                      .map((f) => { if (typeof f.evidence_version !== "number") throw new Error("漏洞证据版本缺失，请刷新列表"); return { id: Number(f.finding_id || f.id), evidence_version: f.evidence_version }; }),
                    include,
                  );
                  toast.success("报告已生成并下载");
                })
              }
            >
              {busy ? "处理中…" : `生成并下载 Word（${selected.length} 条）`}
            </Button>
          </CardContent>
        </Card>
      </div>
    </div>
  );
}
