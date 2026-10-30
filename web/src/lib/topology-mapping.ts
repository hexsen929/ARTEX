export type TopologyMappingPhase = "none" | "running" | "finished" | "failed" | "interrupted";
export interface TopologyMappingStatus {
  running: boolean;
  conversation_id?: number;
  status: TopologyMappingPhase;
  finished_at?: string;
  association?: "native" | "legacy_title";
}

export function topologyMappingStatus(value: unknown): TopologyMappingStatus {
  if (!value || typeof value !== "object" || Array.isArray(value)) throw new Error("拓扑梳理状态响应无效");
  const s = value as Record<string, unknown>;
  const phases = ["none", "running", "finished", "failed", "interrupted"];
  if (typeof s.running !== "boolean" || typeof s.status !== "string" || !phases.includes(s.status) || s.running !== (s.status === "running")) {
    throw new Error("拓扑梳理状态不完整，暂不判断已完成");
  }
  if (s.status !== "none" && (typeof s.conversation_id !== "number" || !Number.isSafeInteger(s.conversation_id) || s.conversation_id <= 0)) {
    throw new Error("拓扑梳理会话关联缺失");
  }
  if (s.status === "none" && s.conversation_id !== undefined) throw new Error("拓扑梳理状态与会话关联不一致");
  if (s.status !== "none" && s.association !== "native" && s.association !== "legacy_title") throw new Error("拓扑梳理关联来源未知");
  if (s.finished_at !== undefined && (typeof s.finished_at !== "string" || !Number.isFinite(Date.parse(s.finished_at)))) {
    throw new Error("拓扑梳理结束时间无效");
  }
  return s as unknown as TopologyMappingStatus;
}

export function topologyMappingLabel(status: TopologyMappingPhase): string {
  switch (status) {
    case "running": return "正在只读梳理，结果记录在会话中…";
    case "finished": return "梳理会话已结束，请查看结果；不代表结论已验证";
    case "failed": return "梳理失败，已有会话记录保留";
    case "interrupted": return "梳理未正常结束，请查看会话记录";
    default: return "网络拓扑等待梳理";
  }
}
