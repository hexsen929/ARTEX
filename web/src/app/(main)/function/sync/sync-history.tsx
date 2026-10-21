"use client";

import * as React from "react";

import { RefreshCwIcon } from "lucide-react";
import { toast } from "sonner";

import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card";
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from "@/components/ui/table";
import { api } from "@/lib/api";
import { syncResultStatus, syncStatusLabels } from "@/lib/sync-result";
import type { SSSyncRequest, SSSyncResult, SSSyncRun, SSSyncStatus } from "@/lib/types";

const assetLabels: Record<string, string> = { subdomain: "子域名", service: "服务", app: "App", ip: "IP" };
const outcomeLabels = { created: "新增", updated: "更新", unchanged: "未变化" };

export function showSyncResult(result: SSSyncResult) {
  const status = syncResultStatus(result);
  if (status === "running") {
    toast.success("后台同步已开始，可在历史中查看进度");
    return;
  }
  if (status === "success") toast.success("本次同步成功");
  else if (status === "failed" || status === "interrupted")
    toast.error(`同步${syncStatusLabels[status]}，请查看结果详情`);
  else toast.warning(`同步${syncStatusLabels[status]}，请查看未完成项`);
}

function SyncBadge({ status }: { status: SSSyncStatus }) {
  let variant: "destructive" | "secondary" | "outline" = "outline";
  if (status === "failed") variant = "destructive";
  if (status === "success") variant = "secondary";
  return <Badge variant={variant}>{syncStatusLabels[status]}</Badge>;
}

export function SyncResult({ result }: { result: SSSyncResult }) {
  return (
    <div className="space-y-3 rounded-md border bg-muted/40 p-3 text-sm">
      <div className="flex flex-wrap items-center gap-3">
        <SyncBadge status={syncResultStatus(result)} />
        {result.run_id && <span className="text-muted-foreground">记录 #{result.run_id}</span>}
      </div>
      {result.stats && (
        <p>
          源记录：拉取 {result.stats.fetched}，写入成功 {result.stats.written}，跳过 {result.stats.skipped}，失败{" "}
          {result.stats.failed}。
        </p>
      )}
      {result.unique_assets && (
        <p>
          本轮独立资产 {result.unique_assets.total}：新增 {result.unique_assets.created}，更新{" "}
          {result.unique_assets.updated}，未变化 {result.unique_assets.unchanged}。直接资产{" "}
          {result.unique_assets.direct}，派生资产 {result.unique_assets.derived}。
        </p>
      )}
      <div className="flex flex-wrap gap-2">
        {Object.entries(result.synced).map(([key, count]) => (
          <Badge key={key} variant="secondary">
            {assetLabels[key] ?? key}：{count} 次写入
          </Badge>
        ))}
      </div>
      <p className="text-muted-foreground text-xs">
        写入次数包含已有资产更新及派生资产，不等于新增或独立资产数量。独立资产按本轮资产 ID
        去重；“成功”指本次读取及处理未发现失败，源端实时数据不保证固定快照。源任务扫描进度不代表同步完成。
      </p>
      {!!result.companies?.length && <p>新建/更新企业：{result.companies.join("、")}</p>}
      {!!result.warnings?.length && (
        <ul className="list-inside list-disc text-amber-700 dark:text-amber-400">
          {[...new Set(result.warnings)].map((message) => (
            <li key={message}>{message}</li>
          ))}
        </ul>
      )}
      {!!result.errors?.length && (
        <ul className="list-inside list-disc text-destructive">
          {[...new Set(result.errors)].slice(0, 20).map((message) => (
            <li key={message}>{message}</li>
          ))}
          {result.errors.length > 20 && <li>共 {result.errors.length} 条错误，请查看逐项记录。</li>}
        </ul>
      )}
      {!!result.targets?.length && (
        <div className="overflow-x-auto">
          <Table>
            <TableHeader>
              <TableRow>
                <TableHead>来源范围</TableHead>
                <TableHead>类型</TableHead>
                <TableHead>同步状态</TableHead>
                <TableHead>拉取 / 写入 / 跳过 / 失败</TableHead>
              </TableRow>
            </TableHeader>
            <TableBody>
              {result.targets.map((target) => (
                <TableRow key={`${target.target}-${target.asset_type}`}>
                  <TableCell>
                    <div className="max-w-xs break-all">{target.target}</div>
                    {target.error && <p className="text-destructive text-xs">{target.error}</p>}
                  </TableCell>
                  <TableCell>{assetLabels[target.asset_type] ?? target.asset_type}</TableCell>
                  <TableCell>
                    <SyncBadge status={target.status} />
                    {target.truncated && <p className="text-amber-700 text-xs dark:text-amber-400">仍有数据未读取</p>}
                  </TableCell>
                  <TableCell>
                    {target.stats.fetched} / {target.stats.written} / {target.stats.skipped} / {target.stats.failed}
                    {target.stats.created !== undefined && (
                      <p className="text-muted-foreground text-xs">
                        主资产：新增 {target.stats.created} / 更新 {target.stats.updated} / 未变化{" "}
                        {target.stats.unchanged}
                      </p>
                    )}
                  </TableCell>
                </TableRow>
              ))}
            </TableBody>
          </Table>
        </div>
      )}
    </div>
  );
}

export function SyncHistory({
  refreshKey,
  canRetry,
  onChanged,
  syncing,
  onSyncingChange,
}: {
  refreshKey: number;
  canRetry: boolean;
  onChanged: () => void;
  syncing: boolean;
  onSyncingChange: (value: boolean) => void;
}) {
  const [page, setPage] = React.useState(1);
  const [revision, setRevision] = React.useState(0);
  const [runs, setRuns] = React.useState<SSSyncRun[]>([]);
  const [total, setTotal] = React.useState(0);
  const [loading, setLoading] = React.useState(true);
  const [error, setError] = React.useState("");
  const [selected, setSelected] = React.useState<number | null>(null);
  let emptyMessage = "暂无同步记录";
  if (error) emptyMessage = "历史暂不可用";
  if (loading) emptyMessage = "读取中…";
  // biome-ignore lint/correctness/useExhaustiveDependencies: Explicit refreshes and completed runs must reload server history.
  React.useEffect(() => {
    let active = true;
    setLoading(true);
    api
      .ssSyncHistory(page)
      .then((data) => {
        if (!active) return;
        setRuns(data.runs ?? []);
        setTotal(data.total);
        setError("");
      })
      .catch((reason: Error) => {
        if (active) setError(reason.message);
      })
      .finally(() => {
        if (active) setLoading(false);
      });
    return () => {
      active = false;
    };
  }, [page, revision, refreshKey]);
  React.useEffect(() => {
    if (loading || (!syncing && !runs.some((run) => run.status === "running"))) return;
    const timer = setTimeout(() => setRevision((value) => value + 1), 5000);
    return () => clearTimeout(timer);
  }, [runs, syncing, loading]);
  return (
    <Card>
      <CardHeader className="flex flex-row items-center justify-between">
        <CardTitle className="text-base">同步历史</CardTitle>
        <Button variant="ghost" size="sm" disabled={loading} onClick={() => setRevision((value) => value + 1)}>
          <RefreshCwIcon className={loading ? "size-4 animate-spin" : "size-4"} />
          刷新
        </Button>
      </CardHeader>
      <CardContent className="space-y-4">
        <p className="text-muted-foreground text-sm">
          历史保存在服务器，数据源停用后仍可查看。点击记录查看逐项结果及未完成原因。
        </p>
        {error && (
          <p role="alert" className="text-destructive text-sm">
            读取历史失败：{error}
          </p>
        )}
        <Table>
          <TableHeader>
            <TableRow>
              <TableHead>记录 / 开始时间</TableHead>
              <TableHead>同步范围</TableHead>
              <TableHead>状态</TableHead>
              <TableHead>详情</TableHead>
            </TableRow>
          </TableHeader>
          <TableBody>
            {runs.map((run) => (
              <TableRow key={run.id}>
                <TableCell>
                  #{run.id}
                  <div className="text-muted-foreground text-xs">{new Date(run.started_at).toLocaleString()}</div>
                </TableCell>
                <TableCell>
                  <div className="max-w-md break-all">{run.request.targets.join("、")}</div>
                  <span className="text-muted-foreground text-xs">
                    {run.request.asset_types.map((type) => assetLabels[type] ?? type).join(" / ")}
                  </span>
                </TableCell>
                <TableCell>
                  <SyncBadge status={run.status} />
                </TableCell>
                <TableCell>
                  <Button variant="outline" size="sm" onClick={() => setSelected(selected === run.id ? null : run.id)}>
                    {selected === run.id ? "收起" : "查看"}
                  </Button>
                </TableCell>
              </TableRow>
            ))}
            {!runs.length && (
              <TableRow>
                <TableCell colSpan={4} className="py-6 text-center text-muted-foreground">
                  {emptyMessage}
                </TableCell>
              </TableRow>
            )}
          </TableBody>
        </Table>
        <div className="flex items-center justify-end gap-2">
          <Button variant="outline" size="sm" disabled={page <= 1 || loading} onClick={() => setPage(page - 1)}>
            上一页
          </Button>
          <span className="text-xs">
            第 {page} 页 · 共 {total} 条
          </span>
          <Button
            variant="outline"
            size="sm"
            disabled={page * 20 >= total || loading}
            onClick={() => setPage(page + 1)}
          >
            下一页
          </Button>
        </div>
        {selected !== null && (
          <HistoryDetail
            key={selected}
            id={selected}
            refreshKey={refreshKey + revision}
            canRetry={canRetry}
            onChanged={onChanged}
            onSyncingChange={onSyncingChange}
          />
        )}
      </CardContent>
    </Card>
  );
}

function HistoryDetail({
  id,
  refreshKey,
  canRetry,
  onChanged,
  onSyncingChange,
}: {
  id: number;
  refreshKey: number;
  canRetry: boolean;
  onChanged: () => void;
  onSyncingChange: (value: boolean) => void;
}) {
  const [page, setPage] = React.useState(1);
  const [data, setData] = React.useState<Awaited<ReturnType<typeof api.ssSyncHistoryDetail>> | null>(null);
  const [error, setError] = React.useState("");
  const [retrying, setRetrying] = React.useState(false);
  // biome-ignore lint/correctness/useExhaustiveDependencies: A refreshed run must reload its persisted item results.
  React.useEffect(() => {
    let active = true;
    api
      .ssSyncHistoryDetail(id, page)
      .then((value) => {
        if (active) {
          setData(value);
          setError("");
        }
      })
      .catch((reason: Error) => {
        if (active) setError(reason.message);
      });
    return () => {
      active = false;
    };
  }, [id, page, refreshKey]);
  const retry = async (request: SSSyncRequest) => {
    setRetrying(true);
    onSyncingChange(true);
    try {
      showSyncResult(await api.ssStartSync(request));
    } catch (reason) {
      toast.error(`重试失败：${(reason as Error).message}`);
    } finally {
      setRetrying(false);
      onSyncingChange(false);
      onChanged();
    }
  };
  if (error)
    return (
      <p role="alert" className="text-destructive text-sm">
        详情读取失败：{error}
      </p>
    );
  if (!data) return <p className="text-sm">读取详情…</p>;
  const unfinished = data.run.result?.targets?.filter((target) => target.status !== "success") ?? [];
  return (
    <div className="space-y-3 rounded-md border p-3">
      <h3 className="font-medium text-sm">记录 #{id}</h3>
      {data.run.result && <SyncResult result={data.run.result} />}
      {data.run.status !== "running" && (
        <div className="flex flex-wrap gap-2">
          {(data.run.result?.stats?.failed ?? 0) + (data.run.result?.stats?.skipped ?? 0) > 0 && (
            <Button
              variant="outline"
              size="sm"
              disabled={retrying}
              onClick={() => retry({ ...data.run.request, retry_run_id: id })}
            >
              仅重试失败和跳过记录
            </Button>
          )}
          {data.run.request.background && data.run.status !== "success" && (
            <Button
              variant="outline"
              size="sm"
              disabled={!canRetry || retrying}
              onClick={async () => {
                setRetrying(true);
                try {
                  showSyncResult(await api.ssResumeSync(id));
                } catch (reason) {
                  toast.error(`恢复失败：${(reason as Error).message}`);
                } finally {
                  setRetrying(false);
                  onChanged();
                }
              }}
            >
              从保存的页面继续
            </Button>
          )}
          {unfinished.map((target) => (
            <Button
              key={`${target.target}-${target.asset_type}`}
              variant="outline"
              size="sm"
              disabled={!canRetry || retrying}
              onClick={() => retry({ ...data.run.request, targets: [target.target], asset_types: [target.asset_type] })}
            >
              重新同步 {target.target} / {assetLabels[target.asset_type] ?? target.asset_type}
            </Button>
          ))}
          {!unfinished.length && ["failed", "partial", "interrupted"].includes(data.run.status) && (
            <Button
              variant="outline"
              size="sm"
              disabled={!canRetry || retrying}
              onClick={() => retry(data.run.request)}
            >
              重新同步本次范围
            </Button>
          )}
        </div>
      )}
      <p className="text-muted-foreground text-xs">
        后台同步关闭页面仍会继续；恢复会跳过已处理记录，从保存的页面继续。重新同步创建新记录并核对整个范围。源端后续页面可能因扫描变化而移动。
      </p>
      <Table>
        <TableHeader>
          <TableRow>
            <TableHead>源资产 / 范围</TableHead>
            <TableHead>类型</TableHead>
            <TableHead>结果</TableHead>
            <TableHead>原因</TableHead>
          </TableRow>
        </TableHeader>
        <TableBody>
          {(data.items ?? []).map((item, index) => (
            <TableRow key={item.id}>
              <TableCell>
                <div className="max-w-xs break-all">{item.source_asset_id || "未提供源资产 ID"}</div>
                <div className="text-muted-foreground text-xs">{item.target}</div>
                {item.raw_omitted ? (
                  <p className="text-amber-700 text-xs">原始 JSON 超限未保存</p>
                ) : (
                  <Button
                    variant="ghost"
                    size="sm"
                    onClick={async () => {
                      try {
                        const detail = await api.ssSyncHistoryDetail(id, (page - 1) * 20 + index + 1, 1, true);
                        const raw = detail.items[0]?.raw;
                        if (!raw) {
                          toast.error("该记录未保留原始 JSON");
                          return;
                        }
                        const url = URL.createObjectURL(
                          new Blob([JSON.stringify(raw, null, 2)], { type: "application/json" }),
                        );
                        const link = document.createElement("a");
                        link.href = url;
                        link.download = `sync-${id}-${item.id}.json`;
                        link.click();
                        setTimeout(() => URL.revokeObjectURL(url), 1000);
                      } catch (reason) {
                        toast.error(`读取原始数据失败：${(reason as Error).message}`);
                      }
                    }}
                  >
                    下载原始 JSON
                  </Button>
                )}
              </TableCell>
              <TableCell>{assetLabels[item.asset_type] ?? item.asset_type}</TableCell>
              <TableCell>
                {(
                  { written: "已写入", success: "已写入", skipped: "已跳过", failed: "失败" } as Record<string, string>
                )[item.status] ?? item.status}
                {item.asset_id ? ` #${item.asset_id}` : ""}
                {item.outcome && <div className="text-muted-foreground text-xs">{outcomeLabels[item.outcome]}</div>}
              </TableCell>
              <TableCell className="max-w-md whitespace-normal break-all">{item.reason || "—"}</TableCell>
            </TableRow>
          ))}
          {!data.items?.length && (
            <TableRow>
              <TableCell colSpan={4} className="text-center text-muted-foreground">
                暂无逐项记录
              </TableCell>
            </TableRow>
          )}
        </TableBody>
      </Table>
      <div className="flex items-center justify-end gap-2">
        <Button variant="outline" size="sm" disabled={page <= 1} onClick={() => setPage(page - 1)}>
          上一页
        </Button>
        <span className="text-xs">
          第 {page} 页 · 共 {data.total} 项
        </span>
        <Button variant="outline" size="sm" disabled={page * 20 >= data.total} onClick={() => setPage(page + 1)}>
          下一页
        </Button>
      </div>
    </div>
  );
}
