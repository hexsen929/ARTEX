"use client";

import * as React from "react";

import { ShieldCheckIcon } from "lucide-react";
import { toast } from "sonner";

import { Alert, AlertDescription } from "@/components/ui/alert";
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
import { Card, CardContent, CardDescription, CardFooter, CardHeader, CardTitle } from "@/components/ui/card";
import { Label } from "@/components/ui/label";
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from "@/components/ui/select";
import { api } from "@/lib/api";
import type { RoEEnforcement } from "@/lib/types";

import { isRoEEnforcement, readRoEEnforcement } from "./settings-response";

const modeLabels: Record<RoEEnforcement, string> = {
  strict: "strict · 范围外按入口审批或拒绝",
  warn: "warn · 记录告警后放行",
  off: "off · 关闭此项范围核验",
};

// Keep this draft independent from the page's full-snapshot apply(): saving a
// different setting must not overwrite an unsaved, security-sensitive choice.
export function RoEEnforcementCard({ className }: { className?: string }) {
  const [saved, setSaved] = React.useState<RoEEnforcement | null>(null);
  const [draft, setDraft] = React.useState<RoEEnforcement | null>(null);
  const [loading, setLoading] = React.useState(true);
  const [saving, setSaving] = React.useState(false);
  const [error, setError] = React.useState("");
  const [confirmOpen, setConfirmOpen] = React.useState(false);
  const mounted = React.useRef(false);
  const generation = React.useRef(0);
  const savingRef = React.useRef(false);

  const load = React.useCallback(async () => {
    if (savingRef.current) return;
    const request = ++generation.current;
    setLoading(true);
    setError("");
    try {
      const mode = readRoEEnforcement(await api.getRuntimeControl("roe_enforcement"));
      if (!mounted.current || generation.current !== request) return;
      setSaved(mode);
      setDraft(mode);
    } catch (e) {
      if (!mounted.current || generation.current !== request) return;
      setError(`读取失败：${(e as Error).message}。请重新读取；也可明确选择模式后，仅修复此项设置。`);
    } finally {
      if (mounted.current && generation.current === request) setLoading(false);
    }
  }, []);

  React.useEffect(() => {
    mounted.current = true;
    void load();
    return () => {
      mounted.current = false;
      generation.current += 1;
    };
  }, [load]);

  const persist = async (mode: RoEEnforcement) => {
    if (loading || savingRef.current) return;
    savingRef.current = true;
    const request = ++generation.current;
    setSaving(true);
    setError("");
    setConfirmOpen(false);
    try {
      // The dedicated control endpoint does not depend on unrelated malformed
      // peers, and its single value body cannot carry another form's defaults.
      const confirmed = readRoEEnforcement(await api.saveRuntimeControl("roe_enforcement", mode));
      if (confirmed !== mode) throw new Error("服务返回模式与本次选择不一致，需重新读取确认");
      if (!mounted.current || generation.current !== request) return;
      setSaved(confirmed);
      setDraft(confirmed);
      toast.success("RoE 模式已保存；后续目标交互读取新值，无需重启");
    } catch (e) {
      if (!mounted.current || generation.current !== request) return;
      // A network failure can happen after the server persisted the request.
      // Retain the last confirmed value, but do not call it the current value.
      setError(`保存结果未确认：${(e as Error).message}。保留上次确认值，请重新读取核对。`);
    } finally {
      savingRef.current = false;
      if (mounted.current && generation.current === request) setSaving(false);
    }
  };

  const editable = !loading && !saving;
  const currentKnown = saved !== null && !error && !loading;
  const dirty = draft !== null && (!currentKnown || draft !== saved);
  const save = () => {
    if (!editable || draft === null || !dirty) return;
    if (!currentKnown || draft === "off") {
      setConfirmOpen(true);
      return;
    }
    void persist(draft);
  };

  return (
    <Card className={className}>
      <CardHeader>
        <CardTitle className="flex items-center gap-2 text-base">
          <ShieldCheckIcon className="size-4" />
          授权范围核验（RoE）
        </CardTitle>
        <CardDescription id="roe-enforcement-description">
          将任务工具识别出的网络目标与该任务已登记范围比较。它独立于 OPSEC、提示词约束和自动审批；
          修改模式不会增加任务授权范围，也不会豁免其他拦截规则或 deny。
        </CardDescription>
      </CardHeader>
      <CardContent className="space-y-3">
        <p className="text-muted-foreground text-sm" aria-live="polite">
          {loading ? "正在读取 RoE 模式…" : saved === null ? "当前模式未知；未选择任何默认值" : `最近确认的模式：${modeLabels[saved]}`}
        </p>
        {error ? <Alert variant="destructive"><AlertDescription>{error}</AlertDescription></Alert> : null}
        <div className="space-y-2">
          <Label htmlFor="roe-enforcement">授权范围检查模式</Label>
          <Select value={draft ?? ""} disabled={!editable} onValueChange={(value) => {
            if (isRoEEnforcement(value)) setDraft(value);
          }}>
            <SelectTrigger id="roe-enforcement" aria-describedby="roe-enforcement-description" className="w-full">
              <SelectValue placeholder="读取后选择；未知值不会自动改成 warn" />
            </SelectTrigger>
            <SelectContent>
              <SelectItem value="strict">{modeLabels.strict}</SelectItem>
              <SelectItem value="warn">{modeLabels.warn}</SelectItem>
              <SelectItem value="off">{modeLabels.off}</SelectItem>
            </SelectContent>
          </Select>
        </div>
        <ul className="text-muted-foreground list-disc space-y-1 pl-4 text-xs">
          <li><b>strict</b>：范围外操作按各工具入口进入既有审批或直接拒绝；审批不可用、拒绝或超时则阻止该操作。仍受已有审批与 deny 规则约束，本开关不会修改自动审批设置。</li>
          <li><b>warn</b>：范围外目标写入告警并继续执行；仅在服务端未保存该设置时作为默认值。</li>
          <li><b>off</b>：关闭这一项范围核验；不会关闭其他拦截、审批或敏感信息保护。</li>
        </ul>
        <p className="text-muted-foreground text-xs">
          本地文件名误识别应修正目标提取逻辑，不应为消除告警而把文件名加入任务范围。
        </p>
      </CardContent>
      <CardFooter className="flex flex-wrap gap-2">
        <Button type="button" onClick={save} disabled={!editable || !dirty}>{saving ? "正在保存…" : "保存 RoE 模式"}</Button>
        <Button type="button" variant="outline" disabled={loading || saving} onClick={() => void load()}>重新读取 RoE 模式</Button>
        {dirty && currentKnown ? <span className="text-muted-foreground text-xs">选择尚未保存，当前运行模式未因此改变。</span> : null}
      </CardFooter>
      <AlertDialog open={confirmOpen} onOpenChange={setConfirmOpen}>
        <AlertDialogContent>
          <AlertDialogHeader>
            <AlertDialogTitle>{draft === "off" ? "确认关闭 RoE 范围核验？" : "确认显式修复 RoE 模式？"}</AlertDialogTitle>
            <AlertDialogDescription>
              {!currentKnown ? "当前服务端模式未能确认；此操作只覆盖你明确选择的 RoE 字段，不写入其他表单默认值。" : "关闭后，范围外目标将不再由 RoE 这一层提示或拦截。"}
              任务登记范围、其他拦截规则及人工审批配置保持不变。
            </AlertDialogDescription>
          </AlertDialogHeader>
          <AlertDialogFooter>
            <AlertDialogCancel>取消</AlertDialogCancel>
            <AlertDialogAction onClick={() => { if (draft !== null) void persist(draft); }}>
              {draft === "off" ? "确认关闭并保存" : "确认仅修复 RoE"}
            </AlertDialogAction>
          </AlertDialogFooter>
        </AlertDialogContent>
      </AlertDialog>
    </Card>
  );
}
