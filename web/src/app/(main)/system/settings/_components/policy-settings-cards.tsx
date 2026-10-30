"use client";

import * as React from "react";

import Link from "next/link";
import { ClipboardCheckIcon, DatabaseIcon } from "lucide-react";
import { toast } from "sonner";

import { Alert, AlertDescription } from "@/components/ui/alert";
import {
  AlertDialog, AlertDialogAction, AlertDialogCancel, AlertDialogContent,
  AlertDialogDescription, AlertDialogFooter, AlertDialogHeader, AlertDialogTitle,
} from "@/components/ui/alert-dialog";
import { Button } from "@/components/ui/button";
import { Card, CardContent, CardDescription, CardFooter, CardHeader, CardTitle } from "@/components/ui/card";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from "@/components/ui/select";
import { api } from "@/lib/api";
import type { FindingVerifyMode, RuntimeControlKey } from "@/lib/types";

import {
  isFindingVerifyMode, MAX_LLM_RECORDS_RETENTION_DAYS, parseLLMRecordsRetentionDraft,
  readFindingVerifyMode, readLLMRecordsRetentionDays,
} from "./settings-response";

// Each independent form reads and writes only its own known value. A complete
// settings response from a different form never resets an unsaved draft here.
function usePolicySetting<T extends string | number>(key: RuntimeControlKey, reader: (value: unknown) => T, onConfirmed: (value: T) => void) {
  const [confirmed, setConfirmed] = React.useState<{ value: T } | null>(null);
  const [loading, setLoading] = React.useState(true);
  const [saving, setSaving] = React.useState(false);
  const [error, setError] = React.useState("");
  const mounted = React.useRef(false);
  const generation = React.useRef(0);
  const savingRef = React.useRef(false);
  const load = React.useCallback(async () => {
    if (savingRef.current) return;
    const request = ++generation.current;
    setLoading(true); setError("");
    try {
      const value = reader(await api.getRuntimeControl(key));
      if (!mounted.current || request !== generation.current) return;
      setConfirmed({ value });
      onConfirmed(value);
    } catch (e) {
      if (!mounted.current || request !== generation.current) return;
      setError(`读取失败：${(e as Error).message}。未知值不会使用默认值覆盖；请重试，或明确选择并确认修复此项。`);
    } finally {
      if (mounted.current && request === generation.current) setLoading(false);
    }
  }, [key, reader, onConfirmed]);
  React.useEffect(() => {
    mounted.current = true;
    void load();
    return () => { mounted.current = false; generation.current += 1; };
  }, [load]);
  const persist = async (expected: T) => {
    if (loading || savingRef.current) return;
    savingRef.current = true;
    const request = ++generation.current;
    setSaving(true); setError("");
    try {
      const value = reader(await api.saveRuntimeControl(key, expected));
      if (!Object.is(value, expected)) throw new Error("服务返回值与本次选择不一致，需重新读取确认");
      if (!mounted.current || request !== generation.current) return;
      setConfirmed({ value });
      onConfirmed(value);
      toast.success("设置已保存；保存请求不会直接发起验证或清理历史记录");
    } catch (e) {
      if (!mounted.current || request !== generation.current) return;
      setError(`保存结果未确认：${(e as Error).message}。保留上次确认值，请重新读取核对。`);
    } finally {
      savingRef.current = false;
      if (mounted.current && request === generation.current) setSaving(false);
    }
  };
  return { confirmed, loading, saving, error, load, persist, known: confirmed !== null && !loading && !error };
}

export function LLMRecordsRetentionCard({ className }: { className?: string }) {
  const [draft, setDraft] = React.useState("");
  const acceptValue = React.useCallback((value: number) => setDraft(String(value)), []);
  const policy = usePolicySetting("llm_records_retention_days", readLLMRecordsRetentionDays, acceptValue);
  const [confirmOpen, setConfirmOpen] = React.useState(false);
  const selected = parseLLMRecordsRetentionDraft(draft);
  const saved = policy.confirmed?.value ?? null;
  const dirty = selected !== null && (!policy.known || selected !== saved);
  const save = () => {
    if (selected === null || !dirty || policy.loading || policy.saving) return;
    // A positive horizon may remove existing rows at the next scheduled run.
    // Do not silently turn indefinite retention into future deletion.
    if (!policy.known || (selected > 0 && (saved === 0 || (saved !== null && selected < saved)))) {
      setConfirmOpen(true);
      return;
    }
    void policy.persist(selected);
  };
  return (
    <Card className={className}>
      <CardHeader>
        <CardTitle className="flex items-center gap-2 text-base"><DatabaseIcon className="size-4" />LLM 调用记录保留</CardTitle>
        <CardDescription id="llm-record-retention-description">
          仅控制已录制完整调用记录（llm_records）的保留期，不删除 Token 用量账本、任务或发现证据。
          服务端未设置时沿用 30 天；0 表示不自动清理。录制是否开启在调用记录页面单独控制。
        </CardDescription>
      </CardHeader>
      <CardContent className="space-y-3">
        <p className="text-muted-foreground text-sm" aria-live="polite">
          {policy.loading ? "正在读取记录保留期…" : saved === null ? "当前保留期未知；未填入默认值" : `最近确认的保留期：${saved === 0 ? "0 天（不自动清理）" : `${saved} 天`}`}
        </p>
        {policy.error ? <Alert variant="destructive"><AlertDescription>{policy.error}</AlertDescription></Alert> : null}
        <div className="space-y-2">
          <Label htmlFor="llm-records-retention-days">LLM 调用记录保留天数</Label>
          <Input id="llm-records-retention-days" type="number" min={0} max={MAX_LLM_RECORDS_RETENTION_DAYS} step={1}
            value={draft} disabled={policy.loading || policy.saving} onChange={(e) => setDraft(e.target.value)}
            placeholder="读取后填写；0 表示不自动清理" aria-describedby="llm-record-retention-description" />
          <p className="text-muted-foreground text-xs">允许 0–{MAX_LLM_RECORDS_RETENTION_DAYS} 的整数。保存不会立即删除记录；服务正常启动和后续每 24 小时的清理会读取该设置。</p>
        </div>
      </CardContent>
      <CardFooter className="flex flex-wrap gap-2">
        <Button type="button" disabled={policy.loading || policy.saving || !dirty} onClick={save}>{policy.saving ? "正在保存…" : "保存记录保留期"}</Button>
        <Button type="button" variant="outline" disabled={policy.loading || policy.saving} onClick={() => void policy.load()}>重新读取保留期</Button>
        <Link className="text-xs underline underline-offset-4" href="/function/llm-records">查看调用记录</Link>
      </CardFooter>
      <AlertDialog open={confirmOpen} onOpenChange={setConfirmOpen}>
        <AlertDialogContent>
          <AlertDialogHeader>
            <AlertDialogTitle>确认记录保留策略变更？</AlertDialogTitle>
            <AlertDialogDescription>
              {!policy.known ? "当前保留期未能确认，此操作仅修复你选择的保留天数字段。" : "缩短保留期或从不清理改为有限期，可能在后续清理时删除较早的完整调用记录。"}
              保存本身不立即删除，但服务正常启动及后续定时清理会应用新值；已删除记录不会因再延长保留期而恢复。
            </AlertDialogDescription>
          </AlertDialogHeader>
          <AlertDialogFooter>
            <AlertDialogCancel>取消</AlertDialogCancel>
            <AlertDialogAction onClick={() => {
              if (selected !== null) { setConfirmOpen(false); void policy.persist(selected); }
            }}>确认仅保存保留期</AlertDialogAction>
          </AlertDialogFooter>
        </AlertDialogContent>
      </AlertDialog>
    </Card>
  );
}

const verifyLabels: Record<FindingVerifyMode, string> = {
  auto: "auto · 按既有条件自动派发",
  off: "off · 不派发新的自动验证",
};

export function FindingVerifyModeCard({ className }: { className?: string }) {
  const [draft, setDraft] = React.useState<FindingVerifyMode | null>(null);
  const acceptValue = React.useCallback((value: FindingVerifyMode) => setDraft(value), []);
  const policy = usePolicySetting("finding_verify_mode", readFindingVerifyMode, acceptValue);
  const [confirmOpen, setConfirmOpen] = React.useState(false);
  const saved = policy.confirmed?.value ?? null;
  const dirty = draft !== null && (!policy.known || draft !== saved);
  return (
    <Card className={className}>
      <CardHeader>
        <CardTitle className="flex items-center gap-2 text-base"><ClipboardCheckIcon className="size-4" />发现自动验证</CardTitle>
        <CardDescription id="finding-verify-mode-description">
          控制后续新事件的自动验证派发。auto（未设置时的既有默认）仍需验证 Agent、新发现触发器及父任务运行状态满足条件，
          且继续遵守任务范围、拦截与审批；这个开关不会自动启用 Agent 或补建触发器。
        </CardDescription>
      </CardHeader>
      <CardContent className="space-y-3">
        <p className="text-muted-foreground text-sm" aria-live="polite">
          {policy.loading ? "正在读取自动验证模式…" : saved === null ? "当前自动验证模式未知；未选择默认值" : `最近确认的自动验证模式：${verifyLabels[saved]}`}
        </p>
        {policy.error ? <Alert variant="destructive"><AlertDescription>{policy.error}</AlertDescription></Alert> : null}
        <Label htmlFor="finding-verify-mode">发现自动验证模式</Label>
        <Select value={draft ?? ""} disabled={policy.loading || policy.saving} onValueChange={(value) => { if (isFindingVerifyMode(value)) setDraft(value); }}>
          <SelectTrigger id="finding-verify-mode" className="w-full" aria-describedby="finding-verify-mode-description"><SelectValue placeholder="读取后选择；未知值不自动改为 auto" /></SelectTrigger>
          <SelectContent>
            <SelectItem value="auto">{verifyLabels.auto}</SelectItem>
            <SelectItem value="off">{verifyLabels.off}</SelectItem>
          </SelectContent>
        </Select>
        <p className="text-muted-foreground text-xs">
          off 只阻止新的自动派发，不取消正在运行的验证，不影响明确发起的手动单条验证；重新开启不会补跑跳过的历史事件。
          这不是自动按内外网阶段切换的设置。
        </p>
      </CardContent>
      <CardFooter className="flex flex-wrap gap-2">
        <Button type="button" disabled={policy.loading || policy.saving || !dirty} onClick={() => setConfirmOpen(true)}>{policy.saving ? "正在保存…" : "保存自动验证模式"}</Button>
        <Button type="button" variant="outline" disabled={policy.loading || policy.saving} onClick={() => void policy.load()}>重新读取验证模式</Button>
        <Link className="text-xs underline underline-offset-4" href="/system/agents">检查 Agent 与触发器</Link>
      </CardFooter>
      <AlertDialog open={confirmOpen} onOpenChange={setConfirmOpen}>
        <AlertDialogContent>
          <AlertDialogHeader>
            <AlertDialogTitle>确认自动验证模式变更？</AlertDialogTitle>
            <AlertDialogDescription>
              {!policy.known ? "当前模式未确认，此操作只修复你明确选择的自动验证字段。" : draft === "off" ? "后续新事件将不再自动派发验证；已运行的验证和已保存结果保持原状。" : "仅允许后续新事件按既有 Agent、触发器和任务状态条件自动派发；不会补跑历史。"}
              不修改任务范围、审批规则、Agent 授权或触发器设置，也不立即发起一次验证。
            </AlertDialogDescription>
          </AlertDialogHeader>
          <AlertDialogFooter>
            <AlertDialogCancel>取消</AlertDialogCancel>
            <AlertDialogAction onClick={() => {
              if (draft !== null) { setConfirmOpen(false); void policy.persist(draft); }
            }}>确认仅保存验证模式</AlertDialogAction>
          </AlertDialogFooter>
        </AlertDialogContent>
      </AlertDialog>
    </Card>
  );
}
