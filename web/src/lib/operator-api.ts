import { http } from "@/lib/api";
import { auth } from "@/lib/auth";
export type OperatorRow = Record<string, unknown>;
export const operatorAPI = {
  get: <T>(path: string, signal?: AbortSignal) => http<T>(path, { signal }),
  save: <T = OperatorRow>(path: string, body: unknown, method = "POST") =>
    http<T>(path, { method, body: JSON.stringify(body) }),
  remove: (path: string) => http<OperatorRow>(path, { method: "DELETE" }),
  async download(id: number) {
    const response = await fetch(`/api/c2/files/${id}/download`, {
      headers: { Authorization: `Bearer ${auth.getToken() ?? ""}` },
    });
    if (!response.ok) throw new Error(`下载失败 (${response.status})`);
    const blob = await response.blob();
    const url = URL.createObjectURL(blob);
    const a = document.createElement("a");
    a.href = url;
    a.download = `c2-file-${id}.bin`;
    a.click();
    setTimeout(() => URL.revokeObjectURL(url), 1000);
  },
};
