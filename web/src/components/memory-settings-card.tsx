"use client";

import * as React from "react";
import Link from "next/link";

import { BrainIcon, RefreshCwIcon } from "lucide-react";
import { toast } from "sonner";

import { Button } from "@/components/ui/button";
import { Card, CardContent, CardDescription, CardFooter, CardHeader, CardTitle } from "@/components/ui/card";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from "@/components/ui/select";
import { Switch } from "@/components/ui/switch";
import { api } from "@/lib/api";
import { memoryAPI } from "@/lib/memory-api";
import { memoryConflict, memoryTime, type MemorySettings } from "@/lib/memory-types";
import type { LLMProfile } from "@/lib/types";

type Form = {
  enabled: boolean;
  auto_extract: boolean;
  global_share: boolean;
  profile_id: string;
  max_entries: string;
  max_tokens: string;
  max_jobs_per_hour: string;
  retention_days: string;
};

function toForm(settings: MemorySettings): Form {
  return {
    enabled: settings.enabled,
    auto_extract: settings.auto_extract,
    global_share: settings.global_share,
    profile_id: String(settings.profile_id),
    max_entries: String(settings.max_entries),
    max_tokens: String(settings.max_tokens),
    max_jobs_per_hour: String(settings.max_jobs_per_hour),
    retention_days: String(settings.retention_days),
  };
}

function integer(value: string, min: number, max: number, label: string): number {
  if (!/^\d+$/.test(value)) throw new Error(`${label}须为整数`);
  const n = Number(value);
  if (!Number.isSafeInteger(n) || n < min || n > max) throw new Error(`${label}须在 ${min}–${max} 之间`);
  return n;
}

export function MemorySettingsCard({
  className,
  onChange,
}: {
  className?: string;
  onChange?: (settings: MemorySettings) => void;
}) {
  const [saved, setSaved] = React.useState<MemorySettings | null>(null);
  const [form, setForm] = React.useState<Form | null>(null);
  const [profiles, setProfiles] = React.useState<LLMProfile[]>([]);
  const [profilesError, setProfilesError] = React.useState("");
  const [loading, setLoading] = React.useState(true);
  const [saving, setSaving] = React.useState(false);
  const [conflict, setConflict] = React.useState(false);
  const [error, setError] = React.useState("");
  const generation = React.useRef(0);
  const mounted = React.useRef(false);
  const savingRef = React.useRef(false);
  const changeRef = React.useRef(onChange);
  changeRef.current = onChange;

  const load = React.useCallback(async () => {
    const current = ++generation.current;
    setLoading(true);
    setError("");
    try {
      const settings = await memoryAPI.settings();
      if (!mounted.current || generation.current !== current) return;
      setSaved(settings);
      setForm(toForm(settings));
      setConflict(false);
      changeRef.current?.(settings);
    } catch (cause) {
      if (!mounted.current || generation.current !== current) return;
      setSaved(null);
      setError(`记忆设置读取失败：${(cause as Error).message}`);
    } finally {
      if (mounted.current && generation.current === current) setLoading(false);
    }
  }, []);

  React.useEffect(() => {
    mounted.current = true;
    void load();
    void api.llmProfiles().then((items) => {
      if (!mounted.current) return;
      setProfiles(items.filter((item) => Number.isSafeInteger(Number(item.id)) && Number(item.id) > 0));
      setProfilesError("");
    }).catch((cause) => {
      if (mounted.current) setProfilesError(`模型列表读取失败：${(cause as Error).message}`);
    });
    return () => { mounted.current = false; generation.current += 1; };
  }, [load]);

  const update = (patch: Partial<Form>) => setForm((previous) => previous ? { ...previous, ...patch } : previous);
  const editable = !!saved && !!form && !loading && !saving && !conflict;
  const dirty = !!saved && !!form && JSON.stringify(form) !== JSON.stringify(toForm(saved));

  const save = async () => {
    if (!saved || !form || !editable || !dirty || savingRef.current) return;
    let next: MemorySettings;
    try {
      next = {
        ...saved,
        enabled: form.enabled,
        auto_extract: form.auto_extract,
        global_share: form.global_share,
        profile_id: integer(form.profile_id, 0, Number.MAX_SAFE_INTEGER, "模型配置"),
        max_entries: integer(form.max_entries, 1, 10, "每次召回条数"),
        max_tokens: integer(form.max_tokens, 1024, 2000, "注入 Token 预算"),
        max_jobs_per_hour: integer(form.max_jobs_per_hour, 1, 60, "每小时提炼预算"),
        retention_days: integer(form.retention_days, 1, 3650, "自动记忆有效天数"),
      };
    } catch (cause) {
      setError((cause as Error).message);
      return;
    }
    savingRef.current = true;
    setSaving(true);
    setError("");
    const current = ++generation.current;
    try {
      const result = await memoryAPI.saveSettings(next);
      if (!mounted.current || generation.current !== current) return;
      if (JSON.stringify(toForm(result)) !== JSON.stringify(toForm(next))) {
        throw new Error("服务未确认本次设置，未报告保存成功，请重新读取后核对");
      }
      setSaved(result);
      setForm(toForm(result));
      changeRef.current?.(result);
      toast.success("记忆设置已保存，后续学习与召回使用新配置");
    } catch (cause) {
      if (!mounted.current || generation.current !== current) return;
      const collided = memoryConflict(cause);
      setConflict(collided);
      setError(collided
        ? "记忆设置已发生变化，本次未覆盖。请重新读取最新设置后再修改。"
        : `保存失败：${(cause as Error).message}`);
    } finally {
      savingRef.current = false;
      if (mounted.current && generation.current === current) setSaving(false);
    }
  };

  const selectedExists = form?.profile_id === "0" || profiles.some((item) => item.id === form?.profile_id);
  return (
    <Card className={className}>
      <CardHeader>
        <CardTitle className="flex items-center gap-2 text-base"><BrainIcon className="size-4" />自动共享记忆</CardTitle>
        <CardDescription>
          自动提炼、整理并按相关性召回，无需逐条审批。全局共享不是全量注入，也不扩大任何任务或工具权限。
          提炼输入和召回片段会发送给所用模型；不保存明文密钥、Cookie 或原始流量。
        </CardDescription>
      </CardHeader>
      <form onSubmit={(event) => { event.preventDefault(); void save(); }}>
        <CardContent className="space-y-5">
          {loading ? <p role="status" className="text-muted-foreground text-sm">正在读取记忆设置…</p> : null}
          {form ? (
            <>
              <div className="space-y-3">
                {([
                  ["enabled", "启用记忆", "关闭后停止新的学习与召回，原业务记录保持不变。"],
                  ["auto_extract", "自动提炼", "从新增或变更的来源提炼有用经验，无需人工审批。"],
                  ["global_share", "所有任务共享", "开启后所有任务可按相关性检索经验，保留来源、目标与适用条件。"],
                ] as const).map(([key, label, description]) => (
                  <div key={key} className="flex items-center justify-between gap-4">
                    <div><Label htmlFor={`memory-${key}`}>{label}</Label><p className="text-muted-foreground mt-1 text-xs">{description}</p></div>
                    <Switch id={`memory-${key}`} checked={form[key]} disabled={!editable || (key !== "enabled" && !form.enabled)} onCheckedChange={(value) => update({ [key]: value })} />
                  </div>
                ))}
              </div>
              <div className="space-y-2">
                <Label htmlFor="memory-profile">自动提炼所用模型</Label>
                <Select value={form.profile_id} onValueChange={(value) => update({ profile_id: value })} disabled={!editable || !form.enabled}>
                  <SelectTrigger id="memory-profile" className="w-full"><SelectValue /></SelectTrigger>
                  <SelectContent>
                    <SelectItem value="0">当前全局配置</SelectItem>
                    {!selectedExists ? <SelectItem value={form.profile_id}>失效配置 #{form.profile_id}</SelectItem> : null}
                    {profiles.map((profile) => <SelectItem key={profile.id} value={profile.id}>{profile.name || profile.model} · {profile.model}</SelectItem>)}
                  </SelectContent>
                </Select>
                {profilesError ? <p role="alert" className="text-destructive text-xs">{profilesError}；已保存的模型绑定保持原值。</p> : null}
                {!selectedExists ? <p role="alert" className="text-destructive text-xs">所选模型配置已失效，请重新选择；系统不会静默切换到其它模型。</p> : null}
                {saved?.model_available === false ? <p role="alert" className="text-destructive text-xs">自动提炼不可用：{saved.model_error || "尚未配置可用模型，请前往 LLM 设置"}。<Link className="ml-1 underline" href="/system/llm">配置模型</Link></p> : null}
              </div>
              <div className="grid gap-4 sm:grid-cols-2">
                {([
                  ["max_entries", "每次最多召回条数", 1, 10],
                  ["max_tokens", "注入Token预算（UTF-8字节保守计数，含来源元数据）", 1024, 2000],
                  ["max_jobs_per_hour", "每小时自动提炼任务上限", 1, 60],
                  ["retention_days", "自动记忆有效天数", 1, 3650],
                ] as const).map(([key, label, min, max]) => (
                  <div key={key} className="space-y-2"><Label htmlFor={`memory-${key}`}>{label}</Label><Input id={`memory-${key}`} type="number" min={min} max={max} step={1} value={form[key]} disabled={!editable || !form.enabled} onChange={(event) => update({ [key]: event.target.value })} /></div>
                ))}
              </div>
              <p className="text-muted-foreground text-xs">预算按 UTF-8 字节保守计数，并非模型精确 tokenizer。预算不足以放入完整正文时，仅提供标明 index_only 的短索引，不截断为完整证据；详情在记忆中心查看。</p>
              <p className="text-muted-foreground text-xs">自动学习起点：{memoryTime(saved?.learn_after)}。只处理此后新增或变更的来源；历史资料可在记忆中心主动回顾。</p>
            </>
          ) : null}
          {error ? <p role="alert" className="text-destructive break-words text-sm">{error}</p> : null}
        </CardContent>
        <CardFooter className="mt-4 flex flex-wrap gap-2">
          <Button type="submit" disabled={!editable || !dirty}>{saving ? "保存中…" : "保存记忆设置"}</Button>
          <Button type="button" variant="outline" disabled={loading || saving} onClick={() => void load()}><RefreshCwIcon className="size-4" />{conflict ? "重新读取最新设置" : "重新读取"}</Button>
          <Button type="button" variant="ghost" asChild><Link href="/function/memories">管理记忆</Link></Button>
        </CardFooter>
      </form>
    </Card>
  );
}
