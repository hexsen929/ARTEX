"use client";

import * as React from "react";

import {
  AlertCircleIcon,
  ChevronDownIcon,
  ChevronRightIcon,
  Loader2Icon,
  NetworkIcon,
  RefreshCwIcon,
} from "lucide-react";

import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Card, CardContent } from "@/components/ui/card";
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from "@/components/ui/select";
import { Tabs, TabsContent, TabsList, TabsTrigger } from "@/components/ui/tabs";
import { api } from "@/lib/api";
import type { IntranetCredential, IntranetTopology, ShellSession, Task, TunnelInfo } from "@/lib/types";
import { cn } from "@/lib/utils";

import { CredsTab } from "./_components/creds-tab";
import { SessionWorkbench } from "./_components/session-workbench";
import { type HostFilter, SessionsTab } from "./_components/sessions-tab";
import { TopologyGraph } from "./_components/topology-graph";
import { TunnelsTab } from "./_components/tunnels-tab";

const SESSIONS_POLL_MS = 10_000;
type LoadArea = "tasks" | "topology" | "sessions" | "tunnels" | "credentials";
const LOAD_LABELS: Record<LoadArea, string> = {
  tasks: "任务列表",
  topology: "拓扑",
  sessions: "会话",
  tunnels: "隧道",
  credentials: "凭据",
};
function loadMessage(error: unknown): string {
  return error instanceof Error ? error.message : "请求失败，请稍后重试";
}

export default function IntranetPage() {
  const [topology, setTopology] = React.useState<IntranetTopology | null>(null);
  const [sessions, setSessions] = React.useState<ShellSession[]>([]);
  const [tunnels, setTunnels] = React.useState<TunnelInfo[]>([]);
  const [credentials, setCredentials] = React.useState<IntranetCredential[]>([]);
  const [loadingTopology, setLoadingTopology] = React.useState(true);
  const [loadingSessions, setLoadingSessions] = React.useState(true);
  const [loadingTunnels, setLoadingTunnels] = React.useState(true);
  const [loadingCreds, setLoadingCreds] = React.useState(true);
  const [loadErrors, setLoadErrors] = React.useState<Partial<Record<LoadArea, string>>>({});
  const [topologyExpanded, setTopologyExpanded] = React.useState<boolean | null>(null);
  const setLoadError = React.useCallback((area: LoadArea, message: string) => {
    setLoadErrors((current) => ({ ...current, [area]: message }));
  }, []);

  const [tab, setTab] = React.useState("sessions");
  const [hostFilter, setHostFilter] = React.useState<HostFilter | null>(null);
  const [workbenchSession, setWorkbenchSession] = React.useState<ShellSession | null>(null);

  // 任务过滤:"all" = 全局视图(后端不传 task_id);四个台账 + 拓扑共用同一选择。
  const [taskId, setTaskId] = React.useState("all");
  const [tasks, setTasks] = React.useState<Task[]>([]);
  const requests = React.useRef({ tasks: 0, topology: 0, sessions: 0, tunnels: 0, credentials: 0 });
  const taskParam = taskId === "all" ? undefined : taskId;

  const loadTasks = React.useCallback(() => {
    const request = ++requests.current.tasks;
    api
      .tasks()
      .then((result) => {
        if (request === requests.current.tasks) {
          setTasks(result.tasks);
          setLoadError("tasks", "");
        }
      })
      .catch((error: unknown) => {
        if (request === requests.current.tasks) setLoadError("tasks", loadMessage(error));
      });
  }, [setLoadError]);

  React.useEffect(() => {
    loadTasks();
    return () => {
      requests.current.tasks++;
    };
  }, [loadTasks]);

  const loadTopology = React.useCallback(() => {
    const request = ++requests.current.topology;
    setLoadingTopology(true);
    api
      .intranetTopology(taskParam)
      .then((value) => {
        if (request === requests.current.topology) {
          setTopology(value);
          setLoadError("topology", "");
        }
      })
      .catch((error: unknown) => {
        if (request === requests.current.topology) setLoadError("topology", loadMessage(error));
      })
      .finally(() => {
        if (request === requests.current.topology) setLoadingTopology(false);
      });
  }, [taskParam, setLoadError]);

  // 会话列表 10s 轮询；签名相同则跳过 setState，避免打开工作台时无谓重渲染。
  const sigRef = React.useRef("");
  const loadSessions = React.useCallback(
    (initial = false) => {
      const request = ++requests.current.sessions;
      if (initial) setLoadingSessions(true);
      api
        .shellSessions(taskParam)
        .then((items) => {
          if (request !== requests.current.sessions) return;
          setLoadError("sessions", "");
          const sig = JSON.stringify(items);
          if (sig !== sigRef.current) {
            sigRef.current = sig;
            setSessions(items);
          }
        })
        .catch((error: unknown) => {
          if (request === requests.current.sessions) setLoadError("sessions", loadMessage(error));
        })
        .finally(() => {
          if (request === requests.current.sessions) setLoadingSessions(false);
        });
    },
    [taskParam, setLoadError],
  );

  const loadTunnels = React.useCallback(() => {
    const request = ++requests.current.tunnels;
    setLoadingTunnels(true);
    api
      .tunnels(taskParam)
      .then((value) => {
        if (request === requests.current.tunnels) {
          setTunnels(value);
          setLoadError("tunnels", "");
        }
      })
      .catch((error: unknown) => {
        if (request === requests.current.tunnels) setLoadError("tunnels", loadMessage(error));
      })
      .finally(() => {
        if (request === requests.current.tunnels) setLoadingTunnels(false);
      });
  }, [taskParam, setLoadError]);

  const loadCreds = React.useCallback(() => {
    const request = ++requests.current.credentials;
    setLoadingCreds(true);
    api
      .intranetCredentials(taskParam)
      .then((value) => {
        if (request === requests.current.credentials) {
          setCredentials(value);
          setLoadError("credentials", "");
        }
      })
      .catch((error: unknown) => {
        if (request === requests.current.credentials) setLoadError("credentials", loadMessage(error));
      })
      .finally(() => {
        if (request === requests.current.credentials) setLoadingCreds(false);
      });
  }, [taskParam, setLoadError]);

  React.useEffect(() => {
    // A task switch must not retain an actionable workbench or accept late
    // responses from the previous task's topology/session/credential requests.
    setTopology(null);
    setSessions([]);
    setTunnels([]);
    setCredentials([]);
    setLoadErrors((current) => ({ tasks: current.tasks }));
    setTopologyExpanded(null);
    setHostFilter(null);
    setWorkbenchSession(null);
    loadTopology();
    loadTunnels();
    loadCreds();
    sigRef.current = ""; // 切换任务后强制刷新会话列表
    loadSessions(true);
    const timer = setInterval(() => loadSessions(), SESSIONS_POLL_MS);
    return () => {
      clearInterval(timer);
      requests.current.topology++;
      requests.current.sessions++;
      requests.current.tunnels++;
      requests.current.credentials++;
    };
  }, [loadTopology, loadTunnels, loadCreds, loadSessions]);

  const refreshAll = () => {
    loadTasks();
    loadTopology();
    loadTunnels();
    loadCreds();
    sigRef.current = "";
    loadSessions(true);
  };

  // 拓扑节点点击 → 切到「会话」Tab 并按该主机过滤。
  const onSelectHost = React.useCallback(
    (nodeId: string) => {
      const node = topology?.nodes.find((n) => n.id === nodeId);
      setHostFilter({ id: nodeId, ip: node?.ip ?? "" });
      setTab("sessions");
    },
    [topology],
  );

  // 轮询刷新后，工作台下那条会话可能已被删除/状态变化：同步最新记录。
  React.useEffect(() => {
    setWorkbenchSession((cur) => {
      if (!cur) return cur;
      return sessions.find((s) => s.id === cur.id) ?? null;
    });
  }, [sessions]);

  const aliveSessions = sessions.filter((s) => s.status === "alive").length;
  const aliveTunnels = tunnels.filter((tunnel) => tunnel.state === "alive").length;
  const showTopology = topologyExpanded ?? (aliveSessions > 0 || aliveTunnels > 0);
  const visibleErrors = (Object.entries(loadErrors) as Array<[LoadArea, string]>).filter(([, message]) =>
    Boolean(message),
  );

  return (
    <div className="flex min-h-0 flex-1 flex-col gap-4">
      {/* Header */}
      <div className="flex flex-col gap-3 sm:flex-row sm:items-center sm:justify-between">
        <div className="flex min-w-0 flex-wrap items-center gap-2">
          <NetworkIcon className="h-5 w-5 shrink-0 text-muted-foreground" />
          <h1 className="text-xl font-semibold tracking-tight whitespace-nowrap">内网作战</h1>
          <Badge variant="secondary" className="tabular-nums">
            会话 {aliveSessions}/{sessions.length}
          </Badge>
          <Badge variant="secondary" className="tabular-nums">
            隧道 {tunnels.length}
          </Badge>
          <Badge variant="secondary" className="tabular-nums">
            凭据 {loadErrors.credentials && credentials.length === 0 ? "—" : credentials.length}
          </Badge>
        </div>
        <div className="flex items-center gap-2 self-stretch sm:self-auto">
          {/* 任务过滤:照 findings 页下拉范式;选中值同时作用于拓扑与三台账(含 10s 轮询)。 */}
          <Select value={taskId} onValueChange={setTaskId}>
            <SelectTrigger size="sm" className="min-w-0 flex-1 sm:w-48 sm:flex-none">
              <SelectValue placeholder="任务" />
            </SelectTrigger>
            <SelectContent>
              <SelectItem value="all">全部任务</SelectItem>
              {tasks.map((t) => {
                const id = String(t.id);
                const label = t.name || t.description || `任务 #${id}`;
                return (
                  <SelectItem key={id} value={id}>
                    <span className="max-w-[14rem] truncate" title={label}>
                      {label}
                    </span>
                  </SelectItem>
                );
              })}
            </SelectContent>
          </Select>
          <Button variant="outline" size="sm" className="h-8 shrink-0 gap-1" onClick={refreshAll}>
            <RefreshCwIcon className={cn("size-3.5", loadingTopology && "animate-spin")} />
            刷新
          </Button>
        </div>
      </div>

      {visibleErrors.length > 0 ? (
        <div
          role="alert"
          className="space-y-1 rounded-lg border border-amber-300 bg-amber-50 px-3 py-2 text-sm text-amber-900 dark:border-amber-900 dark:bg-amber-950/30 dark:text-amber-200"
        >
          <p className="flex items-center gap-2 font-medium">
            <AlertCircleIcon className="size-4" />
            部分数据加载失败，已有数据保留；空白不代表没有记录。
          </p>
          {visibleErrors.map(([area, message]) => (
            <p key={area} className="text-xs break-words">
              {LOAD_LABELS[area]}：{message}
            </p>
          ))}
          <Button variant="outline" size="sm" onClick={refreshAll}>
            重新加载
          </Button>
        </div>
      ) : null}

      {/* Empty session/tunnel inventories should not reserve half the page for a graph. */}
      <Card className="gap-0 py-0">
        <button
          type="button"
          className="flex w-full flex-wrap items-center justify-between gap-2 px-4 py-3 text-left sm:flex-nowrap sm:gap-3"
          aria-expanded={showTopology}
          aria-controls="intranet-topology-content"
          onClick={() => setTopologyExpanded(!showTopology)}
        >
          <span className="flex shrink-0 items-center gap-2 text-sm font-medium whitespace-nowrap">
            {showTopology ? <ChevronDownIcon className="size-4" /> : <ChevronRightIcon className="size-4" />}
            资产与会话拓扑
          </span>
          <span className="text-xs text-muted-foreground">
            {loadErrors.topology && !topology
              ? "拓扑读取失败"
              : loadingTopology
                ? "加载中…"
                : `主机 ${topology?.nodes.length ?? 0} · 活跃会话 ${aliveSessions} · 存活隧道 ${aliveTunnels}`}{" "}
            · {showTopology ? "收起" : "展开"}
          </span>
        </button>
        {showTopology ? (
          <CardContent id="intranet-topology-content" className="border-t p-0">
            <div className="h-[36vh] min-h-56 w-full overflow-hidden rounded-b-xl">
              <TopologyGraph
                data={topology}
                loading={loadingTopology}
                onRefresh={loadTopology}
                onSelectHost={onSelectHost}
              />
            </div>
          </CardContent>
        ) : null}
      </Card>

      {/* 下半部：会话 / 隧道 / 凭据 */}
      <Tabs value={tab} onValueChange={setTab} className="flex min-h-0 flex-1 flex-col gap-0">
        <TabsList className="w-fit">
          <TabsTrigger value="sessions">会话</TabsTrigger>
          <TabsTrigger value="tunnels">隧道</TabsTrigger>
          <TabsTrigger value="creds">凭据</TabsTrigger>
        </TabsList>
        <TabsContent value="sessions" className="flex min-h-0 flex-1 flex-col pt-2">
          <SessionsTab
            selectedTaskId={taskParam}
            sessions={sessions}
            loading={loadingSessions}
            hostFilter={hostFilter}
            onClearHostFilter={() => setHostFilter(null)}
            onOpenWorkbench={setWorkbenchSession}
            onChanged={() => {
              sigRef.current = "";
              loadSessions(true);
              loadTopology();
            }}
          />
        </TabsContent>
        <TabsContent value="tunnels" className="flex min-h-0 flex-1 flex-col pt-2">
          <TunnelsTab
            tunnels={tunnels}
            loading={loadingTunnels}
            onRefresh={() => {
              loadTunnels();
              loadTopology();
            }}
          />
        </TabsContent>
        <TabsContent value="creds" className="flex min-h-0 flex-1 flex-col pt-2">
          {loadingCreds && credentials.length === 0 ? (
            <div className="py-12 text-center">
              <Loader2Icon className="mx-auto h-5 w-5 animate-spin text-muted-foreground" />
            </div>
          ) : loadErrors.credentials && credentials.length === 0 ? (
            <Card role="alert" className="items-center gap-3 p-6 text-center text-sm">
              <p>凭据加载失败，当前尚未取得记录。</p>
              <Button variant="outline" size="sm" onClick={loadCreds}>
                重试加载凭据
              </Button>
            </Card>
          ) : tab === "creds" ? (
            <CredsTab
              key={taskId}
              credentials={credentials}
              loading={loadingCreds}
              onUpdated={(updated) =>
                setCredentials((current) => current.map((item) => (item.id === updated.id ? updated : item)))
              }
            />
          ) : null}
        </TabsContent>
      </Tabs>

      <SessionWorkbench
        session={workbenchSession}
        onOpenChange={(o) => !o && setWorkbenchSession(null)}
        onDeleted={() => {
          sigRef.current = "";
          loadSessions(true);
          loadTopology();
        }}
      />
    </div>
  );
}
