export type ProxyImportIssue = { line: number; error: string; kind?: "duplicate" | "invalid" };

export type ProxyImportReply = {
  ok: boolean;
  probe_queued: boolean;
  imported?: number;
  skipped_duplicates?: number;
  invalid?: number;
  errors: ProxyImportIssue[];
};

// Keep committed, queued, duplicate and invalid outcomes distinct. An older
// server has no counters; malformed/partial counters are not an older schema.
export function assessProxyImportReply(result: ProxyImportReply) {
  if (!result || result.ok !== true || typeof result.probe_queued !== "boolean" || !Array.isArray(result.errors) || result.errors.some((issue) =>
    !issue || !Number.isSafeInteger(issue.line) || issue.line < 1 || typeof issue.error !== "string" ||
    (issue.kind !== undefined && issue.kind !== "duplicate" && issue.kind !== "invalid")
  )) throw new Error("服务未确认代理导入结果，请刷新后核对，避免重复导入");

  const counters = [result.imported, result.skipped_duplicates, result.invalid];
  const counted = counters.some((value) => value !== undefined);
  const invalidErrors = result.errors.filter((issue) => issue.kind !== "duplicate");
  const duplicateCount = result.errors.length - invalidErrors.length;
  if (counted) {
    if (counters.some((value) => !Number.isSafeInteger(value) || value! < 0) ||
      result.skipped_duplicates !== duplicateCount || result.invalid !== invalidErrors.length ||
      (result.imported! > 0 && !result.probe_queued)) {
      throw new Error("导入数量或后台排队结果未能核对，请刷新后确认");
    }
  } else if (!result.probe_queued) throw new Error("服务未确认代理导入和后台探测排队，请刷新后核对");

  return {
    invalidErrors,
    probeQueued: result.probe_queued,
    clearDraft: invalidErrors.length === 0 && (counted ? result.imported! > 0 : result.errors.length === 0),
    summary: counted ? `新增 ${result.imported} 条，重复跳过 ${result.skipped_duplicates} 条，格式错误 ${result.invalid} 条` : null,
    added: counted ? result.imported! : null,
  };
}
