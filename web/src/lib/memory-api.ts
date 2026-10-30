import { http } from "@/lib/api";
import {
  assertMemorySettings,
  memoryQueryString,
  memorySettingsInput,
  type MemoryEntry,
  type MemoryEntryInput,
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
    return result;
  },
  get: (id: number) => http<MemoryEntry>(`/memories/${id}`),
  create: (input: MemoryEntryInput) =>
    http<MemoryEntry>("/memories", { method: "POST", body: JSON.stringify(input) }),
  update: (id: number, input: Partial<MemoryEntryInput> & { version: number; status?: MemoryStatus }) =>
    http<MemoryEntry>(`/memories/${id}`, { method: "PATCH", body: JSON.stringify(input) }),
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
  extract: (task_id?: number) =>
    http<{ queued: number }>("/memories/extract", {
      method: "POST",
      body: JSON.stringify(task_id ? { task_id } : {}),
    }),
};
