"use client";

import * as React from "react";
import Link from "next/link";
import {
  CheckCircle2Icon,
  ChevronLeftIcon,
  ChevronRightIcon,
  CircleHelpIcon,
  CopyIcon,
  ExternalLinkIcon,
  EyeIcon,
  EyeOffIcon,
  Loader2Icon,
  SearchIcon,
  XCircleIcon,
  XIcon,
} from "lucide-react";
import { toast } from "sonner";

import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Card } from "@/components/ui/card";
import { Dialog, DialogContent, DialogDescription, DialogHeader, DialogTitle } from "@/components/ui/dialog";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from "@/components/ui/select";
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from "@/components/ui/table";
import { Textarea } from "@/components/ui/textarea";
import { api } from "@/lib/api";
import {
  CREDENTIAL_SECRET_TTL_MS,
  CREDENTIAL_STATUS_LABELS,
  credentialExternalURL,
  credentialMatchesQuery,
  credentialPage,
  credentialStatus,
  credentialTaskHref,
} from "@/lib/credentials";
import type { CredentialVerificationStatus, IntranetCredential } from "@/lib/types";
import { cn, copyText } from "@/lib/utils";

import { fmtTime } from "./format";

function VerificationBadge({ credential }: { credential: IntranetCredential }) {
  const status = credentialStatus(credential);
  const Icon = status === "verified" ? CheckCircle2Icon : status === "failed" ? XCircleIcon : CircleHelpIcon;
  return (
    <Badge
      variant="outline"
      className={cn(
        "gap-1 whitespace-nowrap",
        status === "verified" && "border-emerald-200 text-emerald-700 dark:border-emerald-800 dark:text-emerald-400",
        status === "failed" && "border-red-200 text-red-700 dark:border-red-800 dark:text-red-400",
        status === "unverified" && "text-muted-foreground",
      )}
    >
      <Icon className="size-3" />
      {CREDENTIAL_STATUS_LABELS[status]}
    </Badge>
  );
}

function TaskLabel({ credential }: { credential: IntranetCredential }) {
  const href = credentialTaskHref(credential);
  const text = `#${credential.task_id} · ${credential.task_name || "未命名任务"}`;
  return href ? (
    <Link href={href} prefetch={false} className="block truncate text-xs text-primary hover:underline">
      {text}
    </Link>
  ) : (
    <span className="block text-xs text-muted-foreground">
      <span className="block truncate">{text}</span>
      <span className="text-[10px]">任务当前不可操作</span>
    </span>
  );
}

function ExternalCredentialLink({ href, children }: { href: string | null | undefined; children: React.ReactNode }) {
  const safe = credentialExternalURL(href);
  return safe ? (
    <a
      href={safe}
      target="_blank"
      rel="noopener noreferrer"
      referrerPolicy="no-referrer"
      className="inline-flex items-center gap-1 text-xs text-primary hover:underline"
    >
      {children}
      <ExternalLinkIcon className="size-3 shrink-0" />
    </a>
  ) : null;
}

function CredentialDetails({
  credential,
  onClose,
  onUpdated,
}: {
  credential: IntranetCredential;
  onClose: () => void;
  onUpdated: (credential: IntranetCredential) => void;
}) {
  // Full secrets belong only to this mounted dialog, never list data, storage or URL attributes.
  const [secret, setSecret] = React.useState("");
  const [secretBusy, setSecretBusy] = React.useState<"view" | "copy" | null>(null);
  const [target, setTarget] = React.useState(credential.target || "");
  const [domain, setDomain] = React.useState(credential.domain || "");
  const [sourceURL, setSourceURL] = React.useState(credential.source_url || "");
  const [metadataBusy, setMetadataBusy] = React.useState(false);
  const [status, setStatus] = React.useState<CredentialVerificationStatus>(credentialStatus(credential));
  const [method, setMethod] = React.useState(credential.verification_method || "");
  const [evidence, setEvidence] = React.useState(credential.verification_evidence || "");
  const [verificationBusy, setVerificationBusy] = React.useState(false);
  const mounted = React.useRef(true);
  const revealGeneration = React.useRef(0);
  const revealAbort = React.useRef<AbortController | null>(null);
  const secretTimer = React.useRef<ReturnType<typeof setTimeout> | null>(null);
  const operationBusy = React.useRef(false);
  const saveOperation = React.useRef(false);

  const clearSecret = React.useCallback(() => {
    revealGeneration.current++;
    revealAbort.current?.abort();
    revealAbort.current = null;
    if (secretTimer.current) clearTimeout(secretTimer.current);
    secretTimer.current = null;
    operationBusy.current = false;
    if (mounted.current) {
      setSecret("");
      setSecretBusy(null);
    }
  }, []);

  React.useEffect(() => {
    mounted.current = true;
    const onVisibility = () => {
      if (document.visibilityState !== "visible") clearSecret();
    };
    document.addEventListener("visibilitychange", onVisibility);
    window.addEventListener("pagehide", clearSecret);
    return () => {
      mounted.current = false;
      clearSecret();
      document.removeEventListener("visibilitychange", onVisibility);
      window.removeEventListener("pagehide", clearSecret);
    };
  }, [clearSecret]);

  const reveal = async (purpose: "view" | "copy") => {
    if (
      operationBusy.current ||
      saveOperation.current ||
      !credential.task_available ||
      document.visibilityState !== "visible"
    )
      return;
    clearSecret();
    operationBusy.current = true;
    const controller = new AbortController();
    revealAbort.current = controller;
    const generation = revealGeneration.current;
    const current = () =>
      mounted.current &&
      generation === revealGeneration.current &&
      !controller.signal.aborted &&
      document.visibilityState === "visible";
    setSecretBusy(purpose);
    try {
      const response = await api.revealCredential(credential.id, purpose, controller.signal);
      if (!current()) return;
      if (response.id !== credential.id || typeof response.secret !== "string")
        throw new Error("凭据读取响应异常，请重试。");
      if (purpose === "view") {
        setSecret(response.secret);
        secretTimer.current = setTimeout(clearSecret, CREDENTIAL_SECRET_TTL_MS);
      } else {
        const copied = await copyText(response.secret);
        if (!current()) return;
        if (copied) toast.success("完整凭据已复制；剪贴板不会自动清除，请自行处理。");
        else toast.error("复制失败，请查看后手动复制。");
      }
    } catch (error) {
      if (current()) toast.error(error instanceof Error ? error.message : "读取完整凭据失败");
    } finally {
      if (current()) {
        operationBusy.current = false;
        revealAbort.current = null;
        setSecretBusy(null);
      }
    }
  };

  const saveMetadata = async (event: React.FormEvent) => {
    event.preventDefault();
    if (saveOperation.current || !credential.task_available) return;
    const targetText = target.trim();
    const normalizedTarget = targetText.includes("://") ? credentialExternalURL(targetText) : targetText;
    const domainText = domain.trim();
    const source = sourceURL.trim();
    const safeSource = source ? credentialExternalURL(source) : "";
    if ((targetText && !normalizedTarget) || (source && !safeSource)) {
      toast.error("链接请填写不含用户名、密码的完整 HTTP(S) 地址。");
      return;
    }
    const bytes = (value: string) => new TextEncoder().encode(value).length;
    if (bytes(normalizedTarget || "") > 2048 || bytes(domainText) > 255 || bytes(safeSource || "") > 8192) {
      toast.error("关联信息过长：目标最多 2048 字节、认证域 255 字节、来源链接 8192 字节。");
      return;
    }
    saveOperation.current = true;
    clearSecret();
    setMetadataBusy(true);
    try {
      const updated = await api.updateCredentialMetadata(credential.id, {
        target: normalizedTarget || "",
        domain: domainText,
        source_url: safeSource || "",
      });
      if (mounted.current) {
        onUpdated(updated);
        setTarget(updated.target || "");
        setDomain(updated.domain || "");
        setSourceURL(updated.source_url || "");
        toast.success("凭据关联信息已保存");
      }
    } catch (error) {
      if (mounted.current) toast.error(error instanceof Error ? error.message : "关联信息保存失败");
    } finally {
      saveOperation.current = false;
      if (mounted.current) setMetadataBusy(false);
    }
  };

  const saveVerification = async (event: React.FormEvent) => {
    event.preventDefault();
    if (saveOperation.current || !credential.task_available) return;
    if (status !== "unverified" && (!method.trim() || !evidence.trim())) {
      toast.error("请填写验证方法与证据；结果未知时保留“未验证”。");
      return;
    }
    if (
      status !== "unverified" &&
      (new TextEncoder().encode(method.trim()).length > 512 || new TextEncoder().encode(evidence.trim()).length > 8192)
    ) {
      toast.error("验证记录过长：方法最多 512 字节，证据最多 8192 字节。");
      return;
    }
    saveOperation.current = true;
    clearSecret();
    setVerificationBusy(true);
    try {
      const updated = await api.updateCredentialVerification(credential.id, {
        status,
        method: status === "unverified" ? "" : method.trim(),
        evidence: status === "unverified" ? "" : evidence.trim(),
      });
      if (mounted.current) {
        onUpdated(updated);
        setStatus(credentialStatus(updated));
        setMethod(updated.verification_method || "");
        setEvidence(updated.verification_evidence || "");
        toast.success("验证记录已保存（未发起网络验证）");
      }
    } catch (error) {
      if (mounted.current) toast.error(error instanceof Error ? error.message : "验证记录保存失败");
    } finally {
      saveOperation.current = false;
      if (mounted.current) setVerificationBusy(false);
    }
  };

  return (
    <Dialog
      open
      onOpenChange={(open) => {
        if (!open) {
          clearSecret();
          onClose();
        }
      }}
    >
      <DialogContent className="sm:max-w-3xl">
        <DialogHeader>
          <DialogTitle>
            凭据 #{credential.id} · {credential.cred_type}
          </DialogTitle>
          <DialogDescription>查看来源、补充关联信息及记录验证结果。完整凭据默认隐藏。</DialogDescription>
        </DialogHeader>
        <div className="space-y-5">
          {!credential.task_available ? (
            <p role="status" className="rounded-md bg-muted p-3 text-xs text-muted-foreground">
              所属任务当前不可操作，以下记录仅供查看。
            </p>
          ) : null}
          <section className="grid gap-3 rounded-lg border p-3 text-sm sm:grid-cols-2">
            <div>
              <p className="mb-1 text-xs text-muted-foreground">所属任务</p>
              <TaskLabel credential={credential} />
            </div>
            <div>
              <p className="mb-1 text-xs text-muted-foreground">登记时间</p>
              {fmtTime(credential.created_at)}
            </div>
            <div>
              <p className="mb-1 text-xs text-muted-foreground">用户名</p>
              <p className="break-all font-mono">{credential.username || "未提供"}</p>
            </div>
            <div>
              <p className="mb-1 text-xs text-muted-foreground">当前状态</p>
              <VerificationBadge credential={credential} />
            </div>
            <div className="sm:col-span-2">
              <p className="mb-1 text-xs text-muted-foreground">所属目标 / 站点</p>
              <p className="break-all">{credential.target_label || credential.target || "未记录，可在下方补充"}</p>
              <ExternalCredentialLink href={credential.target_url}>打开目标（不含查询参数）</ExternalCredentialLink>
            </div>
            {credential.host_asset_id ? (
              <p className="text-xs text-muted-foreground">关联资产 #{credential.host_asset_id}</p>
            ) : null}
          </section>

          <section className="space-y-2 rounded-lg border p-3">
            <div className="flex flex-wrap items-center justify-between gap-2">
              <Label>完整凭据</Label>
              <div className="flex items-center gap-2">
                <Button
                  size="sm"
                  variant="outline"
                  disabled={secretBusy !== null || metadataBusy || verificationBusy || !credential.task_available}
                  onClick={() => (secret ? clearSecret() : void reveal("view"))}
                >
                  {secretBusy === "view" ? (
                    <Loader2Icon className="size-3.5 animate-spin" />
                  ) : secret ? (
                    <EyeOffIcon className="size-3.5" />
                  ) : (
                    <EyeIcon className="size-3.5" />
                  )}
                  {secret ? "隐藏" : "查看 30 秒"}
                </Button>
                <Button
                  size="sm"
                  variant="outline"
                  disabled={secretBusy !== null || metadataBusy || verificationBusy || !credential.task_available}
                  onClick={() => void reveal("copy")}
                >
                  {secretBusy === "copy" ? (
                    <Loader2Icon className="size-3.5 animate-spin" />
                  ) : (
                    <CopyIcon className="size-3.5" />
                  )}
                  复制完整值
                </Button>
              </div>
            </div>
            <pre className="max-h-52 overflow-auto rounded-md bg-muted p-3 font-mono text-xs break-all whitespace-pre-wrap select-text">
              {secret || credential.secret_masked || "—"}
            </pre>
            <p className="text-xs text-muted-foreground">
              显示满 30
              秒、隐藏页面、切换任务或关闭详情时自动隐藏。查看与复制会记录操作审计，不记录密钥内容。复制后的剪贴板需自行处理。
            </p>
          </section>

          <section className="space-y-2 rounded-lg border p-3">
            <Label>原始来源描述（只读）</Label>
            <p className="max-h-48 overflow-auto text-sm break-words whitespace-pre-wrap">
              {credential.source || "未记录来源描述"}
            </p>
            <div className="flex flex-wrap items-center gap-x-4 gap-y-2">
              <ExternalCredentialLink href={credential.source_url}>打开来源（不含查询参数）</ExternalCredentialLink>
              {credentialTaskHref(credential) ? (
                <Link
                  href={credentialTaskHref(credential)!}
                  prefetch={false}
                  className="text-xs text-primary hover:underline"
                >
                  回到所属任务
                </Link>
              ) : null}
              {(credential.source_intent_id || credential.source_exploration_id) && credentialTaskHref(credential) ? (
                <Link
                  href={`${credentialTaskHref(credential)}&tab=graph`}
                  prefetch={false}
                  className="text-xs text-primary hover:underline"
                >
                  查看任务探索图
                </Link>
              ) : null}
            </div>
            {credential.source_intent_id || credential.source_exploration_id ? (
              <p className="text-xs text-muted-foreground">
                来源关联：{credential.source_intent_id ? `意图 #${credential.source_intent_id}` : ""}
                {credential.source_intent_id && credential.source_exploration_id ? " · " : ""}
                {credential.source_exploration_id ? `探索 #${credential.source_exploration_id}` : ""}
              </p>
            ) : (
              <p className="text-xs text-muted-foreground">
                该记录未关联精确的工具调用或证据位置，可先回到所属任务查看记录。
              </p>
            )}
          </section>

          <form onSubmit={saveMetadata} className="space-y-3 rounded-lg border p-3">
            <h3 className="font-medium">补充目标与关联信息</h3>
            <div className="grid gap-3 sm:grid-cols-2">
              <div className="space-y-1.5">
                <Label htmlFor="credential-target">所属目标 / 站点</Label>
                <Input
                  id="credential-target"
                  value={target}
                  onChange={(event) => setTarget(event.target.value)}
                  maxLength={2048}
                  placeholder="主机名、IP 或 HTTP(S) 地址"
                />
              </div>
              <div className="space-y-1.5">
                <Label htmlFor="credential-domain">认证域（AD / Realm，可留空）</Label>
                <Input
                  id="credential-domain"
                  value={domain}
                  onChange={(event) => setDomain(event.target.value)}
                  maxLength={255}
                  placeholder="例如 CORP 或 corp.local，不是来源网站"
                />
              </div>
            </div>
            <div className="space-y-1.5">
              <Label htmlFor="credential-source-url">来源链接（可留空）</Label>
              <Input
                id="credential-source-url"
                value={sourceURL}
                onChange={(event) => setSourceURL(event.target.value)}
                maxLength={2048}
                placeholder="https://...（查询参数和片段不会保存）"
              />
            </div>
            <p className="text-xs text-muted-foreground">
              只补充元信息，不修改原始来源描述、用户名或凭据内容。目标不确定时留空，不自动猜测。
            </p>
            <Button type="submit" size="sm" disabled={metadataBusy || verificationBusy || !credential.task_available}>
              {metadataBusy && <Loader2Icon className="size-3.5 animate-spin" />}保存关联信息
            </Button>
          </form>

          <form onSubmit={saveVerification} className="space-y-3 rounded-lg border p-3">
            <h3 className="font-medium">记录人工验证结果</h3>
            <p className="text-xs text-muted-foreground">
              这里只登记已完成的验证结果，不执行登录、扫描或其它网络请求。未验证不等于失败；通过或失败均须说明方法与证据，请勿粘贴明文密码或
              Token。
            </p>
            <div className="space-y-1.5">
              <Label htmlFor="credential-verification-status">验证状态</Label>
              <Select value={status} onValueChange={(value) => setStatus(value as CredentialVerificationStatus)}>
                <SelectTrigger id="credential-verification-status">
                  <SelectValue />
                </SelectTrigger>
                <SelectContent>
                  {Object.entries(CREDENTIAL_STATUS_LABELS).map(([value, label]) => (
                    <SelectItem key={value} value={value}>
                      {label}
                    </SelectItem>
                  ))}
                </SelectContent>
              </Select>
            </div>
            {status !== "unverified" ? (
              <>
                <div className="space-y-1.5">
                  <Label htmlFor="credential-verification-method">验证方法</Label>
                  <Input
                    id="credential-verification-method"
                    value={method}
                    onChange={(event) => setMethod(event.target.value)}
                    required
                    maxLength={500}
                    placeholder="例如：人工登录指定测试账号，核对响应状态"
                  />
                </div>
                <div className="space-y-1.5">
                  <Label htmlFor="credential-verification-evidence">验证证据</Label>
                  <Textarea
                    id="credential-verification-evidence"
                    value={evidence}
                    onChange={(event) => setEvidence(event.target.value)}
                    required
                    maxLength={8000}
                    rows={3}
                    placeholder="记录时间、受测目标、结果与内部证据编号，不包含凭据原值"
                  />
                </div>
              </>
            ) : (
              <p className="text-xs text-muted-foreground">保存“未验证”会清除当前验证信息；历史验证记录仍保留。</p>
            )}
            <Button type="submit" size="sm" disabled={metadataBusy || verificationBusy || !credential.task_available}>
              {verificationBusy && <Loader2Icon className="size-3.5 animate-spin" />}保存验证记录
            </Button>
            {credential.verification_at ? (
              <div className="space-y-1 border-t pt-3 text-xs text-muted-foreground">
                <p>
                  最近验证：{fmtTime(credential.verification_at)} · {credential.verification_by || "操作者未记录"}
                </p>
                <p className="break-words whitespace-pre-wrap">方法：{credential.verification_method || "未记录"}</p>
                <p className="break-words whitespace-pre-wrap">证据：{credential.verification_evidence || "未记录"}</p>
              </div>
            ) : null}
            {credential.verification_history?.length ? (
              <details className="border-t pt-3">
                <summary className="cursor-pointer text-xs text-primary">
                  查看历史验证记录（{credential.verification_history.length}）
                </summary>
                <ol className="mt-3 space-y-3">
                  {[...credential.verification_history].reverse().map((item, index) => (
                    <li key={`${item.at}-${index}`} className="space-y-1 rounded-md bg-muted p-2 text-xs">
                      <p>
                        {CREDENTIAL_STATUS_LABELS[item.status] || "未验证"} · {fmtTime(item.at)} ·{" "}
                        {item.by || "操作者未记录"}
                      </p>
                      <p className="break-words whitespace-pre-wrap">方法：{item.method || "—"}</p>
                      <p className="break-words whitespace-pre-wrap">证据：{item.evidence || "—"}</p>
                    </li>
                  ))}
                </ol>
              </details>
            ) : null}
          </form>
        </div>
      </DialogContent>
    </Dialog>
  );
}

export function CredsTab({
  credentials,
  loading,
  onUpdated,
}: {
  credentials: IntranetCredential[];
  loading: boolean;
  onUpdated: (credential: IntranetCredential) => void;
}) {
  const [query, setQuery] = React.useState("");
  const [type, setType] = React.useState("all");
  const [status, setStatus] = React.useState("all");
  const [page, setPage] = React.useState(1);
  const [selectedId, setSelectedId] = React.useState<number | null>(null);
  const types = [...new Set(credentials.map((credential) => credential.cred_type).filter(Boolean))].sort();
  const filtered = credentials
    .filter(
      (credential) =>
        (type === "all" || credential.cred_type === type) &&
        (status === "all" || credentialStatus(credential) === status) &&
        credentialMatchesQuery(credential, query),
    )
    .sort((a, b) => b.id - a.id);
  const paged = credentialPage(filtered, page);
  const selected = credentials.find((credential) => credential.id === selectedId);
  const activeFilters = Boolean(query.trim() || type !== "all" || status !== "all");
  const reset = () => {
    setQuery("");
    setType("all");
    setStatus("all");
    setPage(1);
  };

  return (
    <>
      <div className="flex flex-wrap items-center gap-2 pb-3">
        <div className="relative min-w-56 flex-1">
          <SearchIcon className="absolute top-2.5 left-2.5 size-3.5 text-muted-foreground" />
          <Input
            aria-label="搜索凭据"
            placeholder="搜索任务、用户名、目标、认证域或来源…"
            className="h-8 pl-8"
            value={query}
            onChange={(event) => {
              setQuery(event.target.value);
              setPage(1);
            }}
          />
        </div>
        <Select
          value={type}
          onValueChange={(value) => {
            setType(value);
            setPage(1);
          }}
        >
          <SelectTrigger size="sm" className="w-36" aria-label="凭据类型">
            <SelectValue />
          </SelectTrigger>
          <SelectContent>
            <SelectItem value="all">全部类型</SelectItem>
            {types.map((value) => (
              <SelectItem key={value} value={value}>
                {value}
              </SelectItem>
            ))}
          </SelectContent>
        </Select>
        <Select
          value={status}
          onValueChange={(value) => {
            setStatus(value);
            setPage(1);
          }}
        >
          <SelectTrigger size="sm" className="w-36" aria-label="验证状态">
            <SelectValue />
          </SelectTrigger>
          <SelectContent>
            <SelectItem value="all">全部验证状态</SelectItem>
            {Object.entries(CREDENTIAL_STATUS_LABELS).map(([value, label]) => (
              <SelectItem key={value} value={value}>
                {label}
              </SelectItem>
            ))}
          </SelectContent>
        </Select>
        {activeFilters ? (
          <Button variant="ghost" size="sm" onClick={reset}>
            <XIcon className="size-3.5" />
            清空筛选
          </Button>
        ) : null}
        <span className="text-xs text-muted-foreground tabular-nums">
          {filtered.length} / {credentials.length} 条{loading ? " · 刷新中…" : ""}
        </span>
      </div>
      <Card className="flex min-h-0 flex-1 flex-col overflow-hidden py-0">
        <div className="min-h-0 flex-1 overflow-auto">
          <Table className="min-w-[1180px]">
            <TableHeader className="sticky top-0 z-10 bg-card">
              <TableRow>
                <TableHead className="w-16">ID</TableHead>
                <TableHead className="w-44">所属任务</TableHead>
                <TableHead className="w-40">目标 / 站点</TableHead>
                <TableHead className="w-24">类型</TableHead>
                <TableHead className="w-36">用户名</TableHead>
                <TableHead className="w-28">认证域</TableHead>
                <TableHead className="w-32">凭据（脱敏）</TableHead>
                <TableHead className="w-28">验证状态</TableHead>
                <TableHead className="w-44">来源</TableHead>
                <TableHead className="w-36">登记时间 / 操作</TableHead>
              </TableRow>
            </TableHeader>
            <TableBody>
              {loading && credentials.length === 0 ? (
                <TableRow>
                  <TableCell colSpan={10} className="py-12 text-center">
                    <Loader2Icon className="mx-auto size-5 animate-spin text-muted-foreground" />
                  </TableCell>
                </TableRow>
              ) : paged.items.length === 0 ? (
                <TableRow>
                  <TableCell colSpan={10} className="space-y-2 py-12 text-center text-sm text-muted-foreground">
                    <p>{activeFilters ? "没有匹配的凭据，请调整或清空筛选。" : "当前任务范围内暂无凭据。"}</p>
                    {activeFilters ? (
                      <Button variant="outline" size="sm" onClick={reset}>
                        清空筛选
                      </Button>
                    ) : null}
                  </TableCell>
                </TableRow>
              ) : (
                paged.items.map((credential) => (
                  <TableRow key={credential.id}>
                    <TableCell className="font-mono text-xs text-muted-foreground">#{credential.id}</TableCell>
                    <TableCell className="max-w-44">
                      <TaskLabel credential={credential} />
                    </TableCell>
                    <TableCell className="max-w-40">
                      <span className="block truncate text-xs">
                        {credential.target_label || credential.target || "未记录"}
                      </span>
                      <ExternalCredentialLink href={credential.target_url}>打开目标</ExternalCredentialLink>
                    </TableCell>
                    <TableCell>
                      <Badge variant="secondary" className="font-mono text-xs">
                        {credential.cred_type || "—"}
                      </Badge>
                    </TableCell>
                    <TableCell className="max-w-36">
                      <span className="block truncate font-mono text-xs">{credential.username || "未提供"}</span>
                    </TableCell>
                    <TableCell className="max-w-28">
                      <span className="block truncate font-mono text-xs text-muted-foreground">
                        {credential.domain || "未提供"}
                      </span>
                    </TableCell>
                    <TableCell>
                      <code className="block font-mono text-xs">{credential.secret_masked || "—"}</code>
                      <Button
                        variant="link"
                        size="sm"
                        className="h-6 p-0 text-xs"
                        onClick={() => setSelectedId(credential.id)}
                      >
                        查看 / 复制
                      </Button>
                    </TableCell>
                    <TableCell>
                      <VerificationBadge credential={credential} />
                    </TableCell>
                    <TableCell className="max-w-44">
                      <button
                        type="button"
                        className="block w-full truncate text-left text-xs text-primary hover:underline"
                        onClick={() => setSelectedId(credential.id)}
                      >
                        {credential.source || "查看来源详情"}
                      </button>
                    </TableCell>
                    <TableCell className="space-y-1">
                      <span className="block text-xs text-muted-foreground tabular-nums">
                        {fmtTime(credential.created_at)}
                      </span>
                      <Button
                        size="sm"
                        variant="outline"
                        className="h-6 text-xs"
                        data-testid={`credential-details-${credential.id}`}
                        onClick={() => setSelectedId(credential.id)}
                      >
                        详情 / 管理
                      </Button>
                    </TableCell>
                  </TableRow>
                ))
              )}
            </TableBody>
          </Table>
        </div>
        <div className="flex items-center justify-between gap-3 border-t px-3 py-2 text-xs text-muted-foreground">
          <span>
            第 {paged.page} / {paged.pages} 页 · 每页 20 条
          </span>
          <div className="flex gap-2">
            <Button size="sm" variant="outline" disabled={paged.page <= 1} onClick={() => setPage(paged.page - 1)}>
              <ChevronLeftIcon className="size-3.5" />
              上一页
            </Button>
            <Button
              size="sm"
              variant="outline"
              disabled={paged.page >= paged.pages}
              onClick={() => setPage(paged.page + 1)}
            >
              下一页
              <ChevronRightIcon className="size-3.5" />
            </Button>
          </div>
        </div>
      </Card>
      {selected ? (
        <CredentialDetails
          key={selected.id}
          credential={selected}
          onClose={() => setSelectedId(null)}
          onUpdated={onUpdated}
        />
      ) : null}
    </>
  );
}
