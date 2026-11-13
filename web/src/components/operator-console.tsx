"use client";

import * as React from "react";
import Link from "next/link";
import { toast } from "sonner";
import { Button } from "@/components/ui/button";
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card";
import { Input } from "@/components/ui/input";
import { Textarea } from "@/components/ui/textarea";
import { operatorAPI as api, type OperatorRow as Row } from "@/lib/operator-api";

type Module = "kb" | "c2" | "webshell" | "vps" | "armor" | "skills" | "cleanup" | "surfaces";
const modules: { key: Module; title: string; url: string }[] = [
  { key: "kb", title: "POC 武器库", url: "/system/kb" },
  { key: "c2", title: "C2 控制台", url: "/c2" },
  { key: "webshell", title: "WebShell 管理", url: "/webshell" },
  { key: "vps", title: "VPS 管理", url: "/system/vps" },
  { key: "armor", title: "智能体预设", url: "/system/armor" },
  { key: "skills", title: "技能全文检索", url: "/system/skill-search" },
  { key: "cleanup", title: "垃圾清理", url: "/system/cleanup" },
  { key: "surfaces", title: "漏洞面分类", url: "/system/finding-surfaces" },
];
function text(value: unknown) {
  return value === null || value === undefined
    ? "—"
    : typeof value === "object"
      ? JSON.stringify(value)
      : String(value);
}
function errorText(error: unknown) {
  return error instanceof Error ? error.message : String(error);
}
function useData<T>(path: string | null) {
  const [data, setData] = React.useState<T | null>(null),
    [error, setError] = React.useState("");
  const [version, update] = React.useState(0),
    [pending, setPending] = React.useState(false);
  const reload = React.useCallback(() => update((v) => v + 1), []);
  React.useEffect(() => {
    const c = new AbortController();
    setError("");
    setData(null);
    if (!path) {
      setPending(false);
      return () => c.abort();
    }
    setPending(true);
    api
      .get<T>(path, c.signal)
      .then((v) => {
        if (!c.signal.aborted) setData(v);
      })
      .catch((e) => {
        if (!c.signal.aborted) setError(errorText(e));
      })
      .finally(() => {
        if (!c.signal.aborted) setPending(false);
      });
    return () => c.abort();
  }, [path, version]);
  return { data, error, pending, reload };
}
function useAction(reload?: () => void) {
  const [busy, setBusy] = React.useState(false),
    [error, setError] = React.useState("");
  async function run(fn: () => Promise<unknown>, message = "操作完成") {
    if (busy) return;
    setBusy(true);
    setError("");
    try {
      await fn();
      reload?.();
      if (message) toast.success(message);
    } catch (e) {
      setError(errorText(e));
    } finally {
      setBusy(false);
    }
  }
  return { busy, error, run };
}
function ErrorBox({ message }: { message?: string }) {
  return message ? (
    <p role="alert" className="rounded-md border border-destructive/30 bg-destructive/5 p-3 text-sm text-destructive">
      {message}
    </p>
  ) : null;
}
function Grid({
  rows,
  columns,
  select,
  selected,
}: {
  rows: Row[];
  columns: { key: string; title: string }[];
  select?: (row: Row) => void;
  selected?: unknown;
}) {
  return (
    <div className="overflow-x-auto rounded-md border">
      <table className="w-full text-sm">
        <thead className="bg-muted/50">
          <tr>
            {columns.map((c) => (
              <th key={c.key} className="p-3 text-left font-medium">
                {c.title}
              </th>
            ))}
          </tr>
        </thead>
        <tbody>
          {rows.map((r, i) => (
            <tr
              key={text(r.id ?? r.code ?? i)}
              className={`border-t ${select ? "cursor-pointer hover:bg-muted/40" : ""} ${selected === r.id ? "bg-muted" : ""}`}
              onClick={() => select?.(r)}
            >
              {columns.map((c) => (
                <td key={c.key} className="max-w-[28rem] break-words p-3">
                  {text(r[c.key]).slice(0, 260)}
                </td>
              ))}
            </tr>
          ))}
        </tbody>
      </table>
      {rows.length === 0 && <p className="p-8 text-center text-muted-foreground">没有记录</p>}
    </div>
  );
}
type Field = {
  key: string;
  label: string;
  type?: "text" | "number" | "password" | "textarea" | "checkbox";
  options?: string[];
  required?: boolean;
};
function Editor({
  title,
  initial,
  fields,
  save,
  close,
  busy,
}: {
  title: string;
  initial: Row;
  fields: Field[];
  save: (value: Row) => void;
  close: () => void;
  busy: boolean;
}) {
  const [value, setValue] = React.useState<Row>(initial);
  React.useEffect(() => setValue(initial), [initial]);
  return (
    <Card>
      <CardHeader>
        <CardTitle>{title}</CardTitle>
      </CardHeader>
      <CardContent>
        <form
          className="grid gap-4 md:grid-cols-2"
          onSubmit={(e) => {
            e.preventDefault();
            save(value);
          }}
        >
          {fields.map((f) => (
            <label key={f.key} className={`grid gap-2 text-sm ${f.type === "textarea" ? "md:col-span-2" : ""}`}>
              <span>{f.label}</span>
              {f.type === "checkbox" ? (
                <input
                  type="checkbox"
                  checked={!!value[f.key]}
                  onChange={(e) => setValue({ ...value, [f.key]: e.target.checked })}
                />
              ) : f.options ? (
                <select
                  className="h-9 rounded-md border bg-background px-3"
                  value={String(value[f.key] ?? "")}
                  onChange={(e) => setValue({ ...value, [f.key]: e.target.value })}
                >
                  {f.options.map((o) => (
                    <option key={o} value={o}>
                      {o}
                    </option>
                  ))}
                </select>
              ) : f.type === "textarea" ? (
                <Textarea
                  rows={8}
                  value={String(value[f.key] ?? "")}
                  required={f.required}
                  onChange={(e) => setValue({ ...value, [f.key]: e.target.value })}
                />
              ) : (
                <Input
                  type={f.type ?? "text"}
                  autoComplete={f.type === "password" ? "new-password" : "off"}
                  value={String(value[f.key] ?? "")}
                  required={f.required}
                  onChange={(e) =>
                    setValue({ ...value, [f.key]: f.type === "number" ? Number(e.target.value) : e.target.value })
                  }
                />
              )}
            </label>
          ))}
          <div className="flex gap-2 md:col-span-2">
            <Button type="submit" disabled={busy}>
              {busy ? "处理中…" : "保存"}
            </Button>
            <Button type="button" variant="outline" onClick={close}>
              关闭
            </Button>
          </div>
        </form>
      </CardContent>
    </Card>
  );
}
const categories = [
  "rce",
  "deserialization",
  "file-upload",
  "sqli",
  "unauthorized",
  "auth-bypass",
  "weak-password",
  "ssrf",
  "xxe",
  "path-traversal",
  "file-read",
  "info-leak",
  "privesc",
  "tunnel",
  "methodology",
  "recon",
  "frontend-reverse",
  "app-audit",
  "env",
  "waf",
  "protocol",
];
const pocFields: Field[] = [
  { key: "title", label: "标题", required: true },
  { key: "code", label: "稳定标识（留空自动生成）" },
  { key: "category", label: "分类", options: categories },
  { key: "kind", label: "类型", options: ["poc", "exp", "script", "template", "payload"] },
  { key: "component", label: "产品 / 组件" },
  { key: "versions", label: "适用版本（未知请留空）" },
  { key: "cve", label: "CVE" },
  { key: "language", label: "语言" },
  { key: "polarity", label: "情报类型", options: ["context", "positive", "negative"] },
  { key: "verified", label: "已验证（必须附证据）", type: "checkbox" },
  { key: "verified_note", label: "验证证据与条件", type: "textarea" },
  { key: "description", label: "描述 / 前提条件", type: "textarea" },
  { key: "content", label: "正文 / POC（仅保存，不执行）", type: "textarea" },
  { key: "usage", label: "使用说明", type: "textarea" },
  { key: "tags", label: "标签" },
];
function POCLibrary() {
  const [q, setQ] = React.useState(""),
    [search, setSearch] = React.useState(""),
    [category, setCategory] = React.useState("");
  const results = useData<{ ammo: Row[]; playbook: Row[]; negative: Row[]; hint: string }>(
    `/kb/search?limit=100&q=${encodeURIComponent(search)}&category=${encodeURIComponent(category)}`,
  );
  const status = useData<Row>("/kb/status");
  const config = useData<{ preflight: boolean; backfill: boolean; after: string }>("/kb/config");
  const [expert, setExpert] = React.useState("code_auditor");
  const [edit, setEdit] = React.useState<Row | null>(null),
    [task, setTask] = React.useState("");
  const [candidates, setCandidates] = React.useState<Row[]>([]);
  const action = useAction(() => {
    results.reload();
    status.reload();
    config.reload();
  });
  const rows = [...(results.data?.ammo ?? []), ...(results.data?.playbook ?? []), ...(results.data?.negative ?? [])];
  return (
    <div className="space-y-4">
      <div className="grid grid-cols-2 gap-3 lg:grid-cols-4">
        {[
          ["总条目", "total"],
          ["发布者标记已验证", "verified"],
          ["负面情报", "negative"],
          ["累计复用（含导入历史）", "reused"],
        ].map(([label, key]) => (
          <Card key={key}>
            <CardContent className="p-4">
              <p className="text-xs text-muted-foreground">{label}</p>
              <p className="text-2xl font-semibold">{text(status.data?.[key])}</p>
            </CardContent>
          </Card>
        ))}
      </div>
      {config.data && (
        <div className="flex flex-wrap gap-4 rounded-md border p-3 text-sm">
          <label className="flex items-center gap-2">
            <input
              type="checkbox"
              checked={config.data.preflight}
              disabled={action.busy}
              onChange={(e) =>
                action.run(() => api.save("/kb/config", { ...config.data, preflight: e.target.checked }, "PUT"))
              }
            />
            任务开局自动预查
          </label>
          <label className="flex items-center gap-2">
            <input
              type="checkbox"
              checked={config.data.backfill}
              disabled={action.busy}
              onChange={(e) =>
                action.run(() => api.save("/kb/config", { ...config.data, backfill: e.target.checked }, "PUT"))
              }
            />
            已验证的新发现自动回填
          </label>
          <span className="text-xs text-muted-foreground">回填保留证据与适用条件，不覆盖人工条目。</span>
        </div>
      )}
      <form
        className="flex flex-wrap gap-2"
        onSubmit={(e) => {
          e.preventDefault();
          setSearch(q);
        }}
      >
        <Input
          className="max-w-sm"
          placeholder="产品、CVE、方法关键词"
          value={q}
          onChange={(e) => setQ(e.target.value)}
        />
        <select
          aria-label="分类"
          className="rounded-md border bg-background px-3"
          value={category}
          onChange={(e) => setCategory(e.target.value)}
        >
          <option value="">全部分类</option>
          {categories.map((c) => (
            <option key={c}>{c}</option>
          ))}
        </select>
        <Button>检索</Button>
        <Button
          type="button"
          variant="outline"
          onClick={() => setEdit({ category: "methodology", kind: "poc", polarity: "context", verified: false })}
        >
          新建条目
        </Button>
        <Button
          type="button"
          variant="outline"
          disabled={action.busy}
          onClick={() => action.run(() => api.save("/kb/import", {}))}
        >
          导入已配置的 knowledge.db
        </Button>
      </form>
      <ErrorBox message={results.error || status.error || action.error} />
      <p className="text-xs text-muted-foreground">
        资料库与自动经验库保持独立。已验证标签不代表在当前目标复测通过；负面结论只适用于原始条件。
      </p>
      {results.pending ? (
        <p>正在检索…</p>
      ) : (
        <Grid
          rows={rows}
          selected={edit?.id}
          columns={[
            { key: "title", title: "标题" },
            { key: "category", title: "分类" },
            { key: "polarity", title: "情报类型" },
            { key: "verified", title: "验证标签" },
            { key: "hit_count", title: "复用" },
          ]}
          select={(r) =>
            action.run(async () => {
              const v = await api.get<Row>(`/kb/entry?code=${encodeURIComponent(String(r.code))}`);
              setEdit(v);
            }, "")
          }
        />
      )}
      {edit && (
        <Editor
          title="知识条目"
          initial={edit}
          fields={pocFields}
          busy={action.busy}
          close={() => setEdit(null)}
          save={(v) =>
            action.run(async () => {
              await api.save("/kb/entry", v);
              setEdit(null);
            })
          }
        />
      )}
      {!!edit?.code && (
        <Button
          variant="destructive"
          disabled={action.busy}
          onClick={() => {
            if (window.confirm("删除当前条目？"))
              action.run(async () => {
                await api.remove(`/kb/entry?code=${encodeURIComponent(String(edit.code))}`);
                setEdit(null);
              });
          }}
        >
          删除条目
        </Button>
      )}
      <Card>
        <CardHeader>
          <CardTitle>从任务发现整理入库</CardTitle>
        </CardHeader>
        <CardContent className="space-y-3">
          <div className="flex gap-2">
            <Input
              className="max-w-xs"
              type="number"
              aria-label="任务 ID"
              placeholder="任务 ID"
              value={task}
              onChange={(e) => setTask(e.target.value)}
            />
            <Button
              variant="outline"
              disabled={!task || action.busy}
              onClick={() =>
                action.run(async () => {
                  const v = await api.get<{ items: Row[] }>(`/tasks/${task}/kb-candidates`);
                  setCandidates(v.items);
                }, "")
              }
            >
              加载候选
            </Button>
          </div>
          <div className="flex flex-wrap gap-2">
            <select
              aria-label="任务专家"
              className="rounded-md border bg-background px-3"
              value={expert}
              onChange={(e) => setExpert(e.target.value)}
            >
              {["code_auditor", "reverser", "kb_curator", "exploit", "cloud", "avevasion", "payout_analyst"].map(
                (key) => (
                  <option key={key}>{key}</option>
                ),
              )}
            </select>
            <Button
              variant="outline"
              disabled={!task || action.busy}
              onClick={() =>
                action.run(async () => {
                  const v = await api.save<{ conversation_id: number }>(`/tasks/${task}/experts/${expert}/chat`, {});
                  window.location.href = `/chat?c=${v.conversation_id}`;
                }, "")
              }
            >
              打开此任务的专家对话
            </Button>
          </div>
          <Grid
            rows={candidates}
            columns={[
              { key: "finding_id", title: "发现 ID" },
              { key: "name", title: "名称" },
              { key: "summary", title: "摘要" },
            ]}
            select={(r) =>
              setEdit({
                title: r.name ?? r.summary ?? "",
                description: r.summary ?? "",
                content: r.evidence ?? "",
                category: "methodology",
                kind: "poc",
                polarity: "context",
                verified: false,
                source_finding_id: r.finding_id,
              })
            }
          />
        </CardContent>
      </Card>
    </div>
  );
}
function Presets() {
  const list = useData<{ items: Row[] }>("/armor/presets"),
    settings = useData<{
      armor: { enabled: boolean; active_id: number; text: string; roles: Record<string, boolean> };
    }>("/operator/settings");
  const [edit, setEdit] = React.useState<Row | null>(null);
  const action = useAction(() => {
    list.reload();
    settings.reload();
  });
  const config = settings.data?.armor;
  return (
    <div className="space-y-4">
      <p className="text-sm text-muted-foreground">
        管理可按角色选择的附加提示词。应用预设不删除已有提示词、任务约束或拦截规则。
      </p>
      <div className="flex gap-2">
        <Button onClick={() => setEdit({ name: "", text: "" })}>新建预设</Button>
        <Button
          variant="outline"
          disabled={!config || action.busy}
          onClick={() =>
            action.run(() => api.save("/operator/settings", { armor: { ...config, enabled: !config?.enabled } }, "PUT"))
          }
        >
          {config?.enabled ? "停用附加预设" : "启用所选预设"}
        </Button>
      </div>
      <p className="text-sm">
        当前状态：{config?.enabled ? "已启用" : "已停用"} · 预设 ID：{config?.active_id ?? "—"}
      </p>
      <ErrorBox message={action.error || list.error || settings.error} />
      <Grid
        rows={list.data?.items ?? []}
        columns={[
          { key: "name", title: "名称" },
          { key: "size", title: "字节" },
          { key: "updated_at", title: "更新时间" },
        ]}
        select={(r) => action.run(async () => setEdit(await api.get<Row>(`/armor/presets/${r.id}`)), "")}
      />
      {config && (
        <Card>
          <CardContent className="flex flex-wrap gap-4 p-4">
            {["planner", "worker", "mainagent", "pentest", "auto", "exploit", "cloud", "reverser", "code_auditor"].map(
              (role) => (
                <label key={role} className="flex gap-2 text-sm">
                  <input
                    type="checkbox"
                    checked={!!config.roles?.[role]}
                    disabled={action.busy}
                    onChange={(e) =>
                      action.run(() =>
                        api.save(
                          "/operator/settings",
                          { armor: { ...config, roles: { ...config.roles, [role]: e.target.checked } } },
                          "PUT",
                        ),
                      )
                    }
                  />
                  {role}
                </label>
              ),
            )}
          </CardContent>
        </Card>
      )}
      {edit && (
        <>
          <Editor
            title="编辑预设"
            initial={edit}
            fields={[
              { key: "name", label: "名称", required: true },
              { key: "text", label: "附加提示词", type: "textarea" },
            ]}
            busy={action.busy}
            close={() => setEdit(null)}
            save={(v) =>
              action.run(async () => {
                await api.save(edit.id ? `/armor/presets/${edit.id}` : "/armor/presets", v, edit.id ? "PUT" : "POST");
                setEdit(null);
              })
            }
          />
          {!!edit.id && (
            <div className="flex gap-2">
              <Button
                disabled={action.busy}
                onClick={() => action.run(() => api.save(`/armor/presets/${edit.id}/apply`, {}))}
              >
                应用此预设
              </Button>
              <Button
                variant="destructive"
                disabled={action.busy}
                onClick={() => {
                  if (window.confirm("删除预设？"))
                    action.run(async () => {
                      await api.remove(`/armor/presets/${edit.id}`);
                      setEdit(null);
                    });
                }}
              >
                删除
              </Button>
            </div>
          )}
        </>
      )}
    </div>
  );
}
const vpsFields: Field[] = [
  { key: "label", label: "名称" },
  { key: "kind", label: "类型", options: ["ssh", "socks5"] },
  { key: "host", label: "主机", required: true },
  { key: "port", label: "端口", type: "number" },
  { key: "user", label: "SSH 用户" },
  { key: "host_key_sha256", label: "SSH 主机密钥指纹（SHA256:…）" },
  { key: "ssh_password", label: "SSH 密码（留空保留）", type: "password" },
  { key: "ssh_key", label: "SSH 私钥 PEM（留空保留）", type: "textarea" },
  { key: "socks_user", label: "SOCKS 用户" },
  { key: "socks_password", label: "SOCKS 密码（留空保留）", type: "password" },
  { key: "enabled", label: "启用", type: "checkbox" },
  { key: "prefer_callback", label: "首选回连主机（不覆盖现有固定配置）", type: "checkbox" },
  { key: "prefer_egress", label: "首选出口候选", type: "checkbox" },
  { key: "note", label: "备注", type: "textarea" },
];
function VPS() {
  const list = useData<{ vps: Row[]; callback_host: string }>("/vps"),
    [edit, setEdit] = React.useState<Row | null>(null),
    [command, setCommand] = React.useState(""),
    [output, setOutput] = React.useState<Row | null>(null);
  const action = useAction(list.reload);
  return (
    <div className="space-y-4">
      <p className="text-sm text-muted-foreground">
        凭据加密保存，不返回明文。SSH 首次检查会报告主机指纹；核对后填写指纹再连接。
      </p>
      <Button onClick={() => setEdit({ kind: "ssh", port: 22, user: "root", enabled: true })}>登记 VPS</Button>
      <ErrorBox message={action.error || list.error} />
      <Grid
        rows={list.data?.vps ?? []}
        selected={edit?.id}
        select={(r) => {
          setEdit(r);
          setOutput(null);
        }}
        columns={[
          { key: "label", title: "名称" },
          { key: "host", title: "主机" },
          { key: "kind", title: "类型" },
          { key: "enabled", title: "启用" },
          { key: "has_key", title: "有私钥" },
          { key: "has_password", title: "有密码" },
          { key: "last_check_msg", title: "检查结果" },
        ]}
      />
      {edit && (
        <>
          <Editor
            title="VPS 配置"
            initial={edit}
            fields={vpsFields}
            close={() => setEdit(null)}
            busy={action.busy}
            save={(v) =>
              action.run(async () => {
                const b = { ...v };
                for (const k of ["ssh_key", "ssh_password", "socks_password"]) if (!b[k]) delete b[k];
                await api.save(edit.id ? `/vps/${edit.id}` : "/vps", b, edit.id ? "PUT" : "POST");
                setEdit(null);
              })
            }
          />
          {!!edit.id && (
            <Card>
              <CardContent className="space-y-3 p-4">
                <div className="flex gap-2">
                  <Button
                    variant="outline"
                    disabled={action.busy}
                    onClick={() =>
                      action.run(async () => setOutput(await api.save<Row>(`/vps/${edit.id}/check`, {})), "")
                    }
                  >
                    检查 SSH
                  </Button>
                  <Button
                    variant="outline"
                    disabled={action.busy}
                    onClick={() => {
                      if (window.confirm("把全局回连主机改为此 VPS？现有固定环境配置仍优先。"))
                        action.run(() => api.save(`/vps/${edit.id}/apply`, { callback: true }));
                    }}
                  >
                    用于回连
                  </Button>
                  <Button
                    variant="outline"
                    disabled={action.busy || edit.kind !== "socks5"}
                    onClick={() => {
                      if (window.confirm("将全局出口切换到此 SOCKS5 端点？"))
                        action.run(() => api.save(`/vps/${edit.id}/apply`, { egress: true }));
                    }}
                  >
                    用于出口
                  </Button>
                  <Button
                    variant="destructive"
                    disabled={action.busy}
                    onClick={() => {
                      if (window.confirm("删除此 VPS 配置？"))
                        action.run(async () => {
                          await api.remove(`/vps/${edit.id}`);
                          setEdit(null);
                        });
                    }}
                  >
                    删除
                  </Button>
                </div>
                <div className="flex gap-2">
                  <Input
                    placeholder="在此 VPS 上执行的命令"
                    value={command}
                    onChange={(e) => setCommand(e.target.value)}
                  />
                  <Button
                    disabled={!command || action.busy}
                    onClick={() =>
                      action.run(async () => setOutput(await api.save<Row>(`/vps/${edit.id}/exec`, { command })), "")
                    }
                  >
                    执行
                  </Button>
                </div>
                {output && (
                  <pre className="overflow-auto whitespace-pre-wrap rounded bg-muted p-3 text-xs">
                    {JSON.stringify(output, null, 2)}
                  </pre>
                )}
              </CardContent>
            </Card>
          )}
        </>
      )}
    </div>
  );
}
const shellFields: Field[] = [
  { key: "name", label: "名称" },
  { key: "url", label: "完整 URL", required: true },
  { key: "lang", label: "协议", options: ["auto", "php", "phpcmd", "phpxor", "jsp", "aspx", "phpenc", "jspenc"] },
  { key: "password", label: "连接参数 / 密码字段名", type: "password" },
  { key: "secret", label: "加密会话密钥", type: "password" },
  { key: "method", label: "方法", options: ["POST", "GET"] },
  { key: "cmd_param", label: "命令参数名（覆盖默认）" },
  { key: "encoding", label: "响应编码", options: ["auto", "utf-8", "gbk", "gb18030"] },
  { key: "target_os", label: "操作系统", options: ["", "linux", "windows"] },
  { key: "task_id", label: "绑定任务 ID（不移除创建者原有权限）", type: "number" },
  { key: "probe", label: "保存前实际探测", type: "checkbox" },
  { key: "remark", label: "备注", type: "textarea" },
];
function Webshells() {
  const list = useData<{ items: Row[] }>("/webshell"),
    [edit, setEdit] = React.useState<Row | null>(null),
    [command, setCommand] = React.useState(""),
    [output, setOutput] = React.useState<Row | null>(null),
    [history, setHistory] = React.useState<Row[]>([]);
  const action = useAction(list.reload);
  return (
    <div className="space-y-4">
      <Button onClick={() => setEdit({ lang: "auto", method: "POST", encoding: "auto", probe: true })}>
        登记 WebShell
      </Button>
      <ErrorBox message={list.error || action.error} />
      <Grid
        rows={list.data?.items ?? []}
        selected={edit?.id}
        columns={[
          { key: "id", title: "ID" },
          { key: "name", title: "名称" },
          { key: "url", title: "URL" },
          { key: "lang", title: "协议" },
          { key: "status", title: "状态" },
          { key: "created_by_task", title: "所属任务" },
        ]}
        select={(r) => {
          setEdit({ ...r, task_id: r.created_by_task, probe: true });
          setOutput(null);
          setHistory([]);
        }}
      />
      {edit && (
        <>
          <Editor
            title="会话配置"
            initial={edit}
            fields={shellFields}
            busy={action.busy}
            close={() => setEdit(null)}
            save={(v) =>
              action.run(async () => {
                await api.save(edit.id ? `/webshell/${edit.id}` : "/webshell", v, edit.id ? "PUT" : "POST");
                setEdit(null);
              })
            }
          />
          {!!edit.id && (
            <Card>
              <CardContent className="space-y-3 p-4">
                <div className="flex flex-wrap gap-2">
                  <Button
                    variant="outline"
                    disabled={action.busy}
                    onClick={() =>
                      action.run(async () => setOutput(await api.save<Row>(`/webshell/${edit.id}/test`, {})), "")
                    }
                  >
                    实际探活
                  </Button>
                  <Button
                    variant="outline"
                    disabled={action.busy}
                    onClick={() =>
                      action.run(async () => {
                        const answer = Number(edit.created_by_task)
                          ? String(edit.created_by_task)
                          : window.prompt("绑定任务 ID；填 0 打开仅控制此手工会话的独立对话", String(edit.task_id ?? 0));
                        if (answer === null || answer.trim() === "") return;
                        const selected = Number(answer);
                        if (!Number.isSafeInteger(selected) || selected < 0) throw new Error("任务 ID 无效");
                        const v = await api.save<{ conversation_id: number }>(`/webshell/${edit.id}/chat`, {
                          task_id: selected,
                        });
                        window.location.href = `/chat?c=${v.conversation_id}`;
                      }, "")
                    }
                  >
                    打开智能体对话
                  </Button>
                  <Button
                    variant="outline"
                    disabled={action.busy}
                    onClick={() =>
                      action.run(
                        async () => setOutput(await api.save<Row>(`/webshell/${edit.id}/register-asset`, {})),
                        "",
                      )
                    }
                  >
                    登记资产
                  </Button>
                  <Button
                    variant="outline"
                    disabled={action.busy}
                    onClick={() =>
                      action.run(
                        async () => setHistory((await api.get<{ items: Row[] }>(`/webshell/${edit.id}/history`)).items),
                        "",
                      )
                    }
                  >
                    执行历史
                  </Button>
                  <Button
                    variant="destructive"
                    disabled={action.busy}
                    onClick={() => {
                      if (window.confirm("删除此会话？"))
                        action.run(async () => {
                          await api.remove(`/webshell/${edit.id}`);
                          setEdit(null);
                        });
                    }}
                  >
                    删除
                  </Button>
                </div>
                <form
                  className="flex gap-2"
                  onSubmit={(e) => {
                    e.preventDefault();
                    action.run(
                      async () => setOutput(await api.save<Row>(`/webshell/${edit.id}/exec`, { command, timeout: 30 })),
                      "",
                    );
                  }}
                >
                  <Input
                    placeholder="远程命令（仅发送一次）"
                    value={command}
                    onChange={(e) => setCommand(e.target.value)}
                  />
                  <Button disabled={!command || action.busy}>执行</Button>
                </form>
                {output && (
                  <pre className="overflow-auto whitespace-pre-wrap rounded bg-muted p-3 text-xs">
                    {JSON.stringify(output, null, 2)}
                  </pre>
                )}
                <Grid
                  rows={history}
                  columns={[
                    { key: "created_at", title: "时间" },
                    { key: "command", title: "命令" },
                    { key: "ok", title: "成功" },
                    { key: "stdout", title: "回显" },
                  ]}
                />
              </CardContent>
            </Card>
          )}
        </>
      )}
    </div>
  );
}
function SkillSearch() {
  const [query, setQuery] = React.useState(""),
    [term, setTerm] = React.useState("");
  const hits = useData<{ hits: Row[] }>(`/skills/search?q=${encodeURIComponent(term)}&limit=30`),
    categories = useData<{ items: Row[] }>("/operator/skill-categories");
  const action = useAction(() => {
    hits.reload();
    categories.reload();
  });
  const [selected, setSelected] = React.useState<Row | null>(null);
  return (
    <div className="space-y-4">
      <form
        className="flex gap-2"
        onSubmit={(e) => {
          e.preventDefault();
          setTerm(query);
        }}
      >
        <Input placeholder="检索 SKILL.md 和引用文档全文" value={query} onChange={(e) => setQuery(e.target.value)} />
        <Button>检索</Button>
        <Button
          type="button"
          variant="outline"
          disabled={action.busy}
          onClick={() => action.run(() => api.save("/skills/reindex", {}))}
        >
          重建索引
        </Button>
      </form>
      <ErrorBox message={hits.error || categories.error || action.error} />
      <Grid
        rows={hits.data?.hits ?? []}
        columns={[
          { key: "skill", title: "技能" },
          { key: "path", title: "路径" },
          { key: "score", title: "相关度" },
          { key: "snippet", title: "命中片段" },
        ]}
        select={setSelected}
      />
      {selected && (
        <pre className="overflow-auto whitespace-pre-wrap rounded border p-4 text-sm">{text(selected.snippet)}</pre>
      )}
      <h2 className="text-lg font-medium">技能分类（点击修改）</h2>
      <Grid
        rows={categories.data?.items ?? []}
        columns={[
          { key: "skill", title: "技能" },
          { key: "category", title: "分类" },
          { key: "source", title: "来源" },
        ]}
        select={(r) => {
          const category = window.prompt("新分类", String(r.category));
          if (category?.trim())
            action.run(() =>
              api.save(`/skills/${encodeURIComponent(String(r.skill))}/category`, { category }, "PATCH"),
            );
        }}
      />
    </div>
  );
}
function Cleanup() {
  const scan = useData<{ items: Row[]; scan_token: string; total_count: number; total_bytes: number }>(
    "/maintenance/cleanup",
  );
  const [selected, setSelected] = React.useState<Record<string, boolean>>({}),
    action = useAction(scan.reload);
  return (
    <div className="space-y-4">
      <p className="text-sm text-muted-foreground">
        仅清理已确认无所属记录的孤儿文件，或超过 24
        小时的暂存；符号链接、仍有打开文件或扫描后发生变化的项目不清理。现有任务、模型配置、知识库不在清理范围。
      </p>
      <div className="flex gap-2">
        <Button variant="outline" onClick={scan.reload} disabled={scan.pending}>
          重新扫描
        </Button>
        <Button
          variant="destructive"
          disabled={!scan.data || action.busy || !Object.values(selected).some(Boolean)}
          onClick={() => {
            if (window.confirm("永久删除所选类别中经过复核的残留文件？"))
              action.run(() =>
                api.save("/maintenance/cleanup", {
                  keys: Object.keys(selected).filter((k) => selected[k]),
                  scan_token: scan.data?.scan_token,
                }),
              );
          }}
        >
          清理所选残留
        </Button>
      </div>
      <ErrorBox message={scan.error || action.error} />
      <p className="text-sm">
        扫描到 {scan.data?.total_count ?? 0} 项 · {((scan.data?.total_bytes ?? 0) / 1048576).toFixed(2)} MiB
      </p>
      <div className="grid gap-3">
        {scan.data?.items.map((r) => (
          <label key={String(r.key)} className="flex items-start gap-3 rounded-md border p-4">
            <input
              type="checkbox"
              className="mt-1"
              checked={!!selected[String(r.key)]}
              disabled={Number(r.count) === 0}
              onChange={(e) => setSelected({ ...selected, [String(r.key)]: e.target.checked })}
            />
            <div>
              <p className="font-medium">
                {text(r.label)} · {text(r.count)} 项 · {(Number(r.bytes) / 1048576).toFixed(2)} MiB
              </p>
              <p className="text-xs text-muted-foreground">{text(r.hint)}</p>
            </div>
          </label>
        ))}
      </div>
    </div>
  );
}
const listenerFields: Field[] = [
  { key: "name", label: "名称", required: true },
  { key: "kind", label: "传输", options: ["http_beacon", "tcp_reverse"] },
  { key: "bind_host", label: "监听地址", required: true },
  { key: "bind_port", label: "监听端口", type: "number" },
  { key: "path", label: "HTTP 路径" },
  { key: "envelope", label: "加密模式", options: ["auto", "gcm", "cbc-hmac"] },
  { key: "start", label: "创建后启动", type: "checkbox" },
  { key: "remark", label: "备注", type: "textarea" },
];
function C2() {
  const listeners = useData<{ items: Row[] }>("/c2/listeners"),
    sessions = useData<{ items: Row[] }>("/c2/sessions");
  const [listener, setListener] = React.useState<Row | null>(null),
    [session, setSession] = React.useState<Row | null>(null),
    [create, setCreate] = React.useState<Row | null>(null);
  const [payload, setPayload] = React.useState<Row | null>(null),
    [host, setHost] = React.useState(""),
    [os, setOS] = React.useState("linux"),
    [task, setTask] = React.useState("");
  const [command, setCommand] = React.useState(""),
    [type, setType] = React.useState("shell"),
    [path, setPath] = React.useState(""),
    [file, setFile] = React.useState<File | null>(null);
  const tasks = useData<{ items: Row[] }>(`/c2/tasks?session_id=${session?.id ?? 0}`),
    events = useData<{ items: Row[] }>(`/c2/events?session_id=${session?.id ?? 0}`),
    files = useData<{ items: Row[] }>(session ? `/c2/sessions/${session.id}/files` : null);
  const action = useAction(() => {
    listeners.reload();
    sessions.reload();
    tasks.reload();
    events.reload();
    files.reload();
  });
  async function queue() {
    if (!session) return;
    let data = "";
    if (type === "upload") {
      if (!file || file.size > 2 * 1024 * 1024) throw new Error("请选择不超过 2 MiB 的文件");
      const bytes = new Uint8Array(await file.arrayBuffer());
      let binary = "";
      for (const b of bytes) binary += String.fromCharCode(b);
      data = btoa(binary);
    }
    await api.save(`/c2/sessions/${session.id}/tasks`, {
      type,
      command: type === "shell" ? command : "",
      arg: type === "sleep" ? command : "",
      path,
      data,
    });
  }
  return (
    <div className="space-y-4">
      <div className="flex gap-2">
        <Button
          onClick={() =>
            setCreate({
              name: "",
              kind: "http_beacon",
              bind_host: "127.0.0.1",
              bind_port: 18891,
              path: "/updates/check",
              envelope: "auto",
              start: false,
            })
          }
        >
          新建监听器
        </Button>
        <Button
          variant="outline"
          onClick={() => {
            listeners.reload();
            sessions.reload();
            tasks.reload();
            events.reload();
            files.reload();
          }}
        >
          刷新状态
        </Button>
      </div>
      <ErrorBox message={action.error || listeners.error || sessions.error || tasks.error} />
      <p className="text-xs text-muted-foreground">
        支持 HTTP / TCP、GCM / CBC-HMAC。GCM 的 Windows 客户端需要 PowerShell
        7。已投递命令不自动重发；生成脚本不会自行启动客户端。
      </p>
      {create && (
        <Editor
          title="监听器"
          initial={create}
          fields={listenerFields}
          busy={action.busy}
          close={() => setCreate(null)}
          save={(v) =>
            action.run(async () => {
              const r = await api.save<Row>("/c2/listeners", v);
              if (v.start && !r.started) throw new Error(String(r.error));
              setCreate(null);
            })
          }
        />
      )}
      <Grid
        rows={listeners.data?.items ?? []}
        selected={listener?.id}
        columns={[
          { key: "id", title: "ID" },
          { key: "name", title: "监听器" },
          { key: "kind", title: "类型" },
          { key: "bind_host", title: "绑定" },
          { key: "bind_port", title: "端口" },
          { key: "envelope", title: "加密" },
          { key: "status", title: "状态" },
          { key: "last_error", title: "错误" },
        ]}
        select={(r) => {
          setListener(r);
          setPayload(null);
        }}
      />
      {listener && (
        <Card>
          <CardContent className="space-y-3 p-4">
            <div className="flex flex-wrap gap-2">
              <Button
                disabled={action.busy}
                onClick={() => action.run(() => api.save(`/c2/listeners/${listener.id}/start`, {}))}
              >
                启动 #{text(listener.id)}
              </Button>
              <Button
                variant="outline"
                disabled={action.busy}
                onClick={() => action.run(() => api.save(`/c2/listeners/${listener.id}/stop`, {}))}
              >
                停止
              </Button>
              <Button
                variant="destructive"
                disabled={action.busy}
                onClick={() => {
                  if (window.confirm("停止并删除监听器？"))
                    action.run(async () => {
                      await api.remove(`/c2/listeners/${listener.id}`);
                      setListener(null);
                      setPayload(null);
                    });
                }}
              >
                删除
              </Button>
            </div>
            <div className="flex gap-2">
              <Input
                placeholder="可达的回连主机（可留空使用绑定地址）"
                value={host}
                onChange={(e) => setHost(e.target.value)}
              />
              <select
                className="rounded-md border bg-background px-3"
                aria-label="客户端系统"
                value={os}
                onChange={(e) => setOS(e.target.value)}
              >
                {["linux", "darwin", "windows"].map((v) => (
                  <option key={v}>{v}</option>
                ))}
              </select>
              <Button
                variant="outline"
                disabled={action.busy}
                onClick={() =>
                  action.run(
                    async () =>
                      setPayload(
                        await api.get<Row>(
                          `/c2/listeners/${listener.id}/payload?os=${os}&host=${encodeURIComponent(host)}`,
                        ),
                      ),
                    "",
                  )
                }
              >
                生成脚本
              </Button>
            </div>
            {payload && (
              <>
                <p className="text-xs text-muted-foreground">脚本包含本监听器认证材料，不上传到其他服务。</p>
                <Textarea
                  aria-label="客户端脚本"
                  className="font-mono text-xs"
                  rows={10}
                  readOnly
                  value={String(payload.script)}
                />
                <Button
                  variant="outline"
                  onClick={async () => {
                    try {
                      await navigator.clipboard.writeText(String(payload.script));
                      toast.success("已复制脚本");
                    } catch {
                      toast.error("请手动复制脚本");
                    }
                  }}
                >
                  复制脚本
                </Button>
              </>
            )}
          </CardContent>
        </Card>
      )}
      <h2 className="text-lg font-semibold">控制会话</h2>
      <Grid
        rows={sessions.data?.items ?? []}
        selected={session?.id}
        select={setSession}
        columns={[
          { key: "id", title: "ID" },
          { key: "hostname", title: "主机" },
          { key: "username", title: "用户" },
          { key: "os", title: "系统" },
          { key: "status", title: "状态" },
          { key: "last_check_in", title: "最近上线" },
        ]}
      />
      {session && (
        <Card>
          <CardHeader>
            <CardTitle>会话 #{text(session.id)}</CardTitle>
          </CardHeader>
          <CardContent className="space-y-3">
            <div className="flex gap-2">
              <Input
                className="max-w-xs"
                type="number"
                placeholder="所属任务 ID"
                value={task}
                onChange={(e) => setTask(e.target.value)}
              />
              <Button
                variant="outline"
                disabled={!task || action.busy}
                onClick={() =>
                  action.run(async () => {
                    const v = await api.save<{ conversation_id: number }>(`/c2/sessions/${session.id}/chat`, {
                      task_id: Number(task),
                    });
                    window.location.href = `/chat?c=${v.conversation_id}`;
                  }, "")
                }
              >
                打开任务绑定对话
              </Button>
            </div>
            <form
              className="flex flex-wrap gap-2"
              onSubmit={(e) => {
                e.preventDefault();
                if (type !== "exit" || window.confirm("向客户端发送退出任务？"))
                  action.run(queue, "任务已排队，尚未确认执行");
              }}
            >
              <select
                className="rounded-md border bg-background px-3"
                aria-label="任务类型"
                value={type}
                onChange={(e) => setType(e.target.value)}
              >
                {["shell", "sleep", "upload", "download", "exit"].map((v) => (
                  <option key={v}>{v}</option>
                ))}
              </select>
              {["shell", "sleep"].includes(type) && (
                <Input
                  className="max-w-md"
                  placeholder={type === "sleep" ? "间隔秒数（1–3600）" : "命令"}
                  value={command}
                  onChange={(e) => setCommand(e.target.value)}
                />
              )}
              {["upload", "download"].includes(type) && (
                <Input
                  className="max-w-md"
                  placeholder="远程完整路径"
                  value={path}
                  onChange={(e) => setPath(e.target.value)}
                />
              )}
              {type === "upload" && <Input type="file" onChange={(e) => setFile(e.target.files?.[0] ?? null)} />}
              <Button disabled={action.busy}>排队任务</Button>
            </form>
            <Button
              variant="destructive"
              disabled={action.busy}
              onClick={() => {
                if (window.confirm("删除此会话及其任务/文件台账？客户端再次上线将生成新会话。"))
                  action.run(() => api.remove(`/c2/sessions/${session.id}`));
              }}
            >
              删除会话
            </Button>
          </CardContent>
        </Card>
      )}
      <h2 className="text-lg font-semibold">任务与审批</h2>
      <Grid
        rows={tasks.data?.items ?? []}
        columns={[
          { key: "id", title: "任务" },
          { key: "session_id", title: "会话" },
          { key: "task_type", title: "类型" },
          { key: "status", title: "状态" },
          { key: "result_text", title: "结果" },
          { key: "error", title: "错误" },
        ]}
        select={(r) => {
          if (r.status === "awaiting_approval") {
            if (window.confirm(`批准任务 #${r.id}：${JSON.stringify(r.payload)}？`))
              action.run(() => api.save(`/c2/tasks/${r.id}/approve`, {}));
          } else if (r.status === "queued" && window.confirm(`取消尚未投递的任务 #${r.id}？`))
            action.run(() => api.save(`/c2/tasks/${r.id}/cancel`, {}));
        }}
      />
      {session && (
        <>
          <h2 className="text-lg font-semibold">下载文件（点击获取）</h2>
          <ErrorBox message={files.error} />
          <Grid
            rows={files.data?.items ?? []}
            columns={[
              { key: "id", title: "文件" },
              { key: "remote_path", title: "远程路径" },
              { key: "size_bytes", title: "字节" },
              { key: "sha256", title: "SHA256" },
            ]}
            select={(r) => action.run(() => api.download(Number(r.id)), "")}
          />
        </>
      )}
      <h2 className="text-lg font-semibold">事件记录</h2>
      <ErrorBox message={events.error} />
      <Grid
        rows={events.data?.items ?? []}
        columns={[
          { key: "created_at", title: "时间" },
          { key: "category", title: "类别" },
          { key: "message", title: "事件" },
        ]}
      />
    </div>
  );
}
function FindingSurfaces() {
  const data = useData<{
    categories: { key: string; label: string }[];
    counts: Record<string, number>;
    overrides: Record<string, string>;
    automatic_enabled: boolean;
  }>("/findings/surfaces");
  const [selected, setSelected] = React.useState("");
  const [className, setClassName] = React.useState("");
  const [mapping, setMapping] = React.useState("other");
  const records = useData<{ items: Row[] }>(
    selected ? `/exploration/findings?page=1&limit=100&surface=${encodeURIComponent(selected)}` : null,
  );
  const action = useAction(() => {
    data.reload();
    records.reload();
  });
  return (
    <div className="space-y-4">
      <p className="text-sm text-muted-foreground">
        按二开版的11类关键词顺序归类；未知类型由当前模型在启动5秒后及每10分钟补分。这里只发送类型字符串，不改动漏洞验证结论。
      </p>
      <ErrorBox message={data.error || records.error || action.error} />
      <div className="flex flex-wrap gap-2">
        <Button
          variant="outline"
          disabled={action.busy}
          onClick={() => action.run(() => api.save("/findings/surfaces/classify", {}))}
        >
          归类未知类型
        </Button>
        <Button
          variant="outline"
          disabled={!data.data || action.busy}
          onClick={() =>
            action.run(() =>
              api.save("/findings/surfaces", { automatic_enabled: !data.data?.automatic_enabled }, "PUT"),
            )
          }
        >
          {data.data?.automatic_enabled ? "关闭自动补分" : "开启自动补分"}
        </Button>
      </div>
      <div className="grid gap-3 md:grid-cols-3 lg:grid-cols-4">
        {data.data?.categories.map((c) => (
          <button
            key={c.key}
            onClick={() => setSelected(c.key)}
            className={`rounded-md border p-4 text-left ${selected === c.key ? "bg-muted" : "hover:bg-muted/40"}`}
          >
            <p className="text-sm">{c.label}</p>
            <p className="text-2xl font-semibold">{data.data?.counts[c.key] ?? 0}</p>
          </button>
        ))}
      </div>
      <form
        className="flex flex-wrap gap-2"
        onSubmit={(e) => {
          e.preventDefault();
          action.run(() => api.save("/findings/surfaces", { mapping: { [className]: mapping } }, "PUT"));
        }}
      >
        <Input
          className="max-w-sm"
          aria-label="漏洞类型原字符串"
          placeholder="漏洞类型原字符串"
          value={className}
          onChange={(e) => setClassName(e.target.value)}
        />
        <select
          aria-label="漏洞面归类"
          className="rounded-md border bg-background px-3"
          value={mapping}
          onChange={(e) => setMapping(e.target.value)}
        >
          {data.data?.categories.map((c) => (
            <option key={c.key} value={c.key}>
              {c.label}
            </option>
          ))}
        </select>
        <Button disabled={!className.trim() || action.busy}>保存归类</Button>
      </form>
      {selected && (
        <Grid
          rows={records.data?.items ?? []}
          columns={[
            { key: "name", title: "发现名称" },
            { key: "vulnclass", title: "类型" },
            { key: "surface", title: "漏洞面" },
            { key: "severity", title: "级别" },
            { key: "verification_status", title: "验证状态" },
          ]}
        />
      )}
      <h2 className="text-lg font-semibold">已保存的补充分组</h2>
      <Grid
        rows={Object.entries(data.data?.overrides ?? {}).map(([vulnclass, surface]) => ({ vulnclass, surface }))}
        columns={[
          { key: "vulnclass", title: "原类型" },
          { key: "surface", title: "分组" },
        ]}
      />
    </div>
  );
}
export function OperatorConsole({ module }: { module: Module }) {
  const title = modules.find((v) => v.key === module)?.title;
  return (
    <div className="space-y-5 p-4 md:p-6">
      <div>
        <h1 className="text-2xl font-semibold">{title}</h1>
        <p className="mt-1 text-sm text-muted-foreground">
          操作模块 · 保留当前 ARTEX 的资料库、经验库、代理与执行恢复机制
        </p>
      </div>
      <nav aria-label="操作模块" className="flex flex-wrap gap-2">
        {modules.map((v) => (
          <Link
            key={v.key}
            href={v.url}
            className={`rounded-md border px-3 py-2 text-sm ${v.key === module ? "bg-primary text-primary-foreground" : "hover:bg-muted"}`}
          >
            {v.title}
          </Link>
        ))}
      </nav>
      {module === "kb" ? (
        <POCLibrary />
      ) : module === "c2" ? (
        <C2 />
      ) : module === "webshell" ? (
        <Webshells />
      ) : module === "vps" ? (
        <VPS />
      ) : module === "armor" ? (
        <Presets />
      ) : module === "skills" ? (
        <SkillSearch />
      ) : module === "surfaces" ? (
        <FindingSurfaces />
      ) : (
        <Cleanup />
      )}
    </div>
  );
}
