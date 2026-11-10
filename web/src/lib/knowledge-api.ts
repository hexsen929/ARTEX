import { http } from "@/lib/api";
import { auth } from "@/lib/auth";

export type KnowledgeDoc = { id: number; task_id: number; title: string; category: string; content?: string; file_name: string; version: number; bytes: number; chunks: number; indexed: boolean };
export type KnowledgeConfig = { enabled: boolean; base_url: string; model: string; dimension: number; has_key: boolean; epoch: number; vector_available: boolean };
export type KnowledgeResult = { mode: string; notice: string; hits: { doc_id: number; version: number; chunk: number; task_id: number; title: string; content: string; score: number }[] };
export const knowledgeAPI = {
  list: (task: number, signal?: AbortSignal) => http<{ items: KnowledgeDoc[] }>(`/knowledge?task_id=${task}`, { signal }),
  get: (id: number) => http<KnowledgeDoc>(`/knowledge/${id}`),
  save: (d: Partial<KnowledgeDoc>) => http<KnowledgeDoc>(d.id ? `/knowledge/${d.id}` : "/knowledge", { method: d.id ? "PUT" : "POST", body: JSON.stringify(d) }),
  remove: (d: KnowledgeDoc) => http(`/knowledge/${d.id}?version=${d.version}`, { method: "DELETE" }),
  config: () => http<{ config: KnowledgeConfig; pdf_available: boolean }>("/knowledge/config"),
  saveConfig: (c: KnowledgeConfig, apiKey?: string) => http<KnowledgeConfig>("/knowledge/config", { method: "PUT", body: JSON.stringify({ enabled: c.enabled, base_url: c.base_url, model: c.model, dimension: c.dimension, epoch: c.epoch, ...(apiKey !== undefined ? { api_key: apiKey } : {}) }) }),
  index: (d: KnowledgeDoc, c: KnowledgeConfig) => http(`/knowledge/${d.id}/index`, { method: "POST", body: JSON.stringify({ version: d.version, epoch: c.epoch }) }),
  search: (task: number, query: string, signal?: AbortSignal) => http<KnowledgeResult>("/knowledge/search", { method: "POST", body: JSON.stringify({ task_id: task, query, top_k: 5 }), signal }),
  upload: async (file: File, task: number) => {
    const form = new FormData(); form.append("file", file); form.append("task_id", String(task));
    const response = await fetch("/api/knowledge/upload", { method: "POST", headers: { Authorization: `Bearer ${auth.getToken()}` }, body: form });
    const result = await response.json(); if (!response.ok) throw new Error(result.error || "导入失败"); return result as KnowledgeDoc;
  },
};
