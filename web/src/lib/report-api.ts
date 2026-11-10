import { http, downloadFilename } from "@/lib/api";
import { auth } from "@/lib/auth";
export type ReportTemplate = { id: number; version: number; name: string; title: string; company: string; client: string; project: string; personnel: string; locale: string; footer: string; logo: string };
export const blankTemplate: ReportTemplate = { id: 0, version: 0, name: "", title: "安全测试报告", company: "", client: "", project: "", personnel: "", locale: "zh-CN", footer: "", logo: "" };
export const reportAPI = {
  templates: () => http<{ items: ReportTemplate[] }>("/report-templates"),
  save: (t: ReportTemplate) => http<ReportTemplate>(t.id ? `/report-templates/${t.id}` : "/report-templates", { method: t.id ? "PUT" : "POST", body: JSON.stringify(t) }),
  remove: (t: ReportTemplate) => http(`/report-templates/${t.id}?version=${t.version}`, { method: "DELETE" }),
  generate: async (template: ReportTemplate, findings: { id: number; evidence_version: number }[], include: boolean) => {
    const response = await fetch("/api/reports/docx", { method: "POST", headers: { "Content-Type": "application/json", Authorization: `Bearer ${auth.getToken()}` }, body: JSON.stringify({ template_id: template.id, template_version: template.version, include_unverified: include, findings }) });
    if (!response.ok) { const error = await response.json(); throw new Error(error.error || "报告生成失败"); }
    const blob = await response.blob(); const url = URL.createObjectURL(blob); const a = document.createElement("a"); a.href = url; a.download = downloadFilename(response.headers.get("Content-Disposition") || "", "security-report.docx"); a.click(); setTimeout(() => URL.revokeObjectURL(url), 1000);
  },
};
