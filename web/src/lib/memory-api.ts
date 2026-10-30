import { http } from "@/lib/api";
import {
  assertMemorySettings,
  assertMemoryEntrySharing,
  assertMemoryLearning,
  memoryQueryString,
  memorySettingsInput,
  type MemoryEntry,
  type MemoryEntryInput,
  type MemoryLearningSnapshot,
  type MemoryQuery,
  type MemorySettings,
  type MemoryStatus,
  type MemoryUsage,
  type MemoryUsageQuery,
} from "@/lib/memory-types";

export const memoryAPI = {
  list: async (query: MemoryQuery = {}, signal?: AbortSignal) => {
    const result = await http<{ items: MemoryEntry[]; total: number }>(`/memories${memoryQueryString(query)}`, { signal });
    if (!Array.isArray(result.items) || !Number.isSafeInteger(result.total) || result.total < 0) {
      throw new Error("服务未返回完整的记忆列表");
    }
    result.items.forEach(assertMemoryEntrySharing);
    return result;
  },
  get: async (id: number) => {
    const entry = await http<MemoryEntry>(`/memories/${id}`);
    assertMemoryEntrySharing(entry);
    return entry;
  },
  create: async (input: MemoryEntryInput) => {
    const entry = await http<MemoryEntry>("/memories", { method: "POST", body: JSON.stringify(input) });
    assertMemoryEntrySharing(entry);
    return entry;
  },
  update: async (id: number, input: Partial<MemoryEntryInput> & { version: number; status?: MemoryStatus }) => {
    const entry = await http<MemoryEntry>(`/memories/${id}`, { method: "PATCH", body: JSON.stringify(input) });
    assertMemoryEntrySharing(entry);
    return entry;
  },
  remove: (id: number, version: number) =>
    http<{ deleted: boolean }>(`/memories/${id}`, { method: "DELETE", body: JSON.stringify({ version }) }),
  settings: async (signal?: AbortSignal) => {
    const settings = await http<MemorySettings>("/memories/settings", { signal });
    assertMemorySettings(settings);
    return settings;
  },
  saveSettings: async (input: MemorySettings) => {
    const settings = await http<MemorySettings>("/memories/settings", {
      method: "PUT",
      body: JSON.stringify(memorySettingsInput(input)),
    });
    assertMemorySettings(settings);
    return settings;
  },
  usage: async (query: MemoryUsageQuery = {}, signal?: AbortSignal) => {
    const result = await http<{ items: MemoryUsage[] }>(`/memories/usage${memoryQueryString(query)}`, { signal });
    if (!Array.isArray(result.items)) throw new Error("服务未返回完整的记忆使用记录");
    return result.items;
  },
  learning: async (signal?: AbortSignal) => {
    const snapshot = await http<MemoryLearningSnapshot>("/memories/learning", { signal });
    assertMemoryLearning(snapshot);
    return snapshot;
  },
  extract: async (task_id?: number) => {
    const result = await http<{ queued: number; unit: "stages" }>("/memories/extract", {
      method: "POST",
      body: JSON.stringify(task_id ? { task_id } : {}),
    });
    if (!Number.isSafeInteger(result.queued) || result.queued < 0 || result.unit !== "stages") {
      throw new Error("服务未确认已排队阶段数，未报告学习完成");
    }
    return result;
  },
};
