"use client";
import * as React from "react";
import { http } from "@/lib/api";
import type { Finding, StructuredReproduction } from "@/lib/types";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { Textarea } from "@/components/ui/textarea";
const empty = (): StructuredReproduction => ({
  entry_point: "",
  prerequisites: [],
  provenance: "",
  steps: [{ title: "", request: "", response: "", expected: "", proves: "", traffic_ids: [] }],
});
export function ReproductionEditor({
  finding,
  contextTask,
  onSaved,
}: {
  finding: Finding;
  contextTask?: string;
  onSaved: (f: Finding) => void;
}) {
  const [open, setOpen] = React.useState(false),
    [version, setVersion] = React.useState<number | undefined>(undefined),
    [value, setValue] = React.useState<StructuredReproduction>(empty),
    [busy, setBusy] = React.useState(false),
    [error, setError] = React.useState("");
  React.useEffect(() => {
    setOpen(false);
    setError("");
  }, [finding.id, contextTask]);
  if (finding.inherited) return null;
  if (!open)
    return (
      <Button
        variant="outline"
        size="sm"
        onClick={() => {
          setValue(structuredClone(finding.reproduction || empty()));
          setVersion(finding.evidence_version);
          setError("");
          setOpen(true);
        }}
      >
        编辑结构化步骤
      </Button>
    );
  const update = (i: number, key: string, text: string) =>
    setValue((old) => ({
      ...old,
      steps: old.steps.map((s, n) =>
        n === i
          ? {
              ...s,
              [key]:
                key === "traffic_ids"
                  ? text
                      .split(",")
                      .map((s) => s.trim())
                      .filter(Boolean)
                  : text,
            }
          : s,
      ),
    }));
  return (
    <div className="space-y-3 rounded border p-3">
      <p className="text-sm">只整理已执行的动作和已有证据；保存不会确认漏洞。流量 ID 必须已绑定到该漏洞。</p>
      {error && <p role="alert">{error}</p>}
      <label className="block">
        入口
        <Input
          aria-label="复现入口"
          value={value.entry_point}
          onChange={(e) => setValue({ ...value, entry_point: e.target.value })}
        />
      </label>
      <label className="block">
        前置条件（每行一条）
        <Textarea
          aria-label="复现前置条件"
          value={(value.prerequisites || []).join("\n")}
          onChange={(e) => setValue({ ...value, prerequisites: e.target.value.split("\n").filter(Boolean) })}
        />
      </label>
      {value.steps.map((s, i) => (
        <div key={i} className="space-y-2 rounded bg-muted/30 p-3">
          <h4>步骤 {i + 1}</h4>
          {(
            [
              ["title", "动作说明"],
              ["request", "请求 / 命令"],
              ["response", "响应 / 输出"],
              ["expected", "预期现象"],
              ["proves", "证明内容和局限"],
              ["traffic_ids", "已绑定流量 ID（逗号分隔）"],
            ] as const
          ).map(([key, label]) => (
            <label key={key} className="block text-sm">
              {label}
              <Textarea
                aria-label={`步骤 ${i + 1} ${label}`}
                value={key === "traffic_ids" ? (s.traffic_ids || []).join(",") : s[key]}
                onChange={(e) => update(i, key, e.target.value)}
              />
            </label>
          ))}
          <Button
            size="sm"
            variant="ghost"
            disabled={busy || value.steps.length <= 1}
            onClick={() => setValue({ ...value, steps: value.steps.filter((_, n) => n !== i) })}
          >
            移除此步骤
          </Button>
        </div>
      ))}
      <Button
        variant="outline"
        disabled={busy || value.steps.length >= 32}
        onClick={() => setValue({ ...value, steps: [...value.steps, empty().steps[0]] })}
      >
        添加步骤
      </Button>
      <label className="block">
        来源说明
        <Textarea
          aria-label="复现来源说明"
          value={value.provenance}
          onChange={(e) => setValue({ ...value, provenance: e.target.value })}
        />
      </label>
      <div className="flex gap-2">
        <Button
          disabled={busy}
          onClick={async () => {
            setBusy(true);
            setError("");
            try {
              if (typeof version !== "number") throw new Error("证据版本缺失，请刷新漏洞详情");
              const saved = await http<Finding>(
                `/exploration/findings/${finding.finding_id || finding.id}/reproduction${contextTask ? `?context_task=${encodeURIComponent(contextTask)}` : ""}`,
                {
                  method: "PUT",
                  body: JSON.stringify({ evidence_version: version, reproduction: value }),
                },
              );
              onSaved(saved);
              setOpen(false);
            } catch (e) {
              setError((e as Error).message);
            } finally {
              setBusy(false);
            }
          }}
        >
          保存步骤
        </Button>
        <Button variant="ghost" disabled={busy} onClick={() => setOpen(false)}>
          取消
        </Button>
      </div>
    </div>
  );
}
