"use client";

import * as React from "react";

import { toast } from "sonner";

import { StatusBadge } from "@/components/status-badge";
import { Button } from "@/components/ui/button";
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from "@/components/ui/dialog";
import { Label } from "@/components/ui/label";
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from "@/components/ui/select";
import { Textarea } from "@/components/ui/textarea";
import { api } from "@/lib/api";
import { statusMeta } from "@/lib/status";
import type { Finding, FindingVerification } from "@/lib/types";

export const VERIFICATION_STATES: FindingVerification[] = ["confirmed", "unverified", "blocked", "rejected", "legacy"];

export function FindingVerificationBadge({ finding }: { finding: Finding }) {
  return (
    <span
      className="inline-flex max-w-full shrink-0 align-middle"
      title={finding.verification_note?.trim() ? finding.verification_note : "尚无验证说明；缺少证据不等于漏洞不存在"}
    >
      <StatusBadge domain="verification" value={finding.verification_status ?? "legacy"} dot />
    </span>
  );
}

export function FindingVerificationEditor({
  finding,
  onUpdated,
}: {
  finding: Finding;
  onUpdated: (finding: Finding) => void;
}) {
  const [open, setOpen] = React.useState(false);
  const [status, setStatus] = React.useState<FindingVerification>("unverified");
  const [note, setNote] = React.useState("");
  const [version, setVersion] = React.useState(0);
  const [addition, setAddition] = React.useState("");
  const [saving, setSaving] = React.useState(false);
  function begin() {
    setStatus(finding.verification_status === "legacy" ? "unverified" : (finding.verification_status ?? "unverified"));
    setVersion(finding.evidence_version ?? 0);
    setAddition("");
    setNote(finding.verification_note ?? "");
    setOpen(true);
  }
  async function save() {
    if (!finding.finding_id || finding.inherited) return;
    setSaving(true);
    try {
      const updated = await api.setFindingVerification(finding.finding_id, status, note, version, addition);
      onUpdated(updated);
      setOpen(false);
      toast.success("验证状态已保存，严重等级和处理状态保持不变");
    } catch (e) {
      toast.error((e as Error).message);
    } finally {
      setSaving(false);
    }
  }
  return (
    <div className="space-y-2">
      <div className="flex flex-wrap items-center gap-2">
        <FindingVerificationBadge finding={finding} />
        {!finding.inherited && finding.finding_id && (
          <Button size="sm" variant="outline" onClick={begin}>
            更新验证
          </Button>
        )}
      </div>
      {finding.verification_note && (
        <p className="whitespace-pre-wrap break-words text-muted-foreground text-xs">{finding.verification_note}</p>
      )}
      <Dialog
        open={open}
        onOpenChange={(v) => {
          if (!saving) setOpen(v);
        }}
      >
        <DialogContent>
          <DialogHeader>
            <DialogTitle>更新验证状态</DialogTitle>
            <DialogDescription>
              待验证和验证受阻的发现仍保留在漏洞列表。严重等级与验证状态独立；验证失败不直接视为已排除。
            </DialogDescription>
          </DialogHeader>
          <Label htmlFor="verification-state">验证状态</Label>
          <Select value={status} onValueChange={(v) => setStatus(v as FindingVerification)} disabled={saving}>
            <SelectTrigger id="verification-state">
              <SelectValue />
            </SelectTrigger>
            <SelectContent>
              {VERIFICATION_STATES.filter((v) => v !== "legacy").map((v) => (
                <SelectItem key={v} value={v}>
                  {statusMeta("verification", v).label}
                </SelectItem>
              ))}
            </SelectContent>
          </Select>
          <Label htmlFor="verification-note">依据、缺少的证据与后续验证方法</Label>
          <Textarea
            id="verification-note"
            value={note}
            onChange={(e) => setNote(e.target.value)}
            maxLength={4000}
            rows={5}
            disabled={saving}
            placeholder="确认：说明已有证据如何支持结论；受阻：说明限制及缺少什么；排除：提供反证。"
          />
          <Label htmlFor="verification-addition">补充实际验证证据（可选，只追加）</Label>
          <Textarea
            id="verification-addition"
            value={addition}
            onChange={(e) => setAddition(e.target.value)}
            maxLength={32000}
            rows={4}
            disabled={saving}
            placeholder="仅填写实际得到的输出或记录；原证据不会被覆盖。"
          />
          <DialogFooter>
            <Button variant="outline" disabled={saving} onClick={() => setOpen(false)}>
              取消
            </Button>
            <Button onClick={() => void save()} disabled={saving || (status !== "unverified" && !note.trim())}>
              {saving ? "保存中…" : "保存"}
            </Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>
    </div>
  );
}
