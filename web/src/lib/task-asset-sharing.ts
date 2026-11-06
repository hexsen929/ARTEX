// A successful HTTP status is not evidence that the task sharing policy was
// accepted. Keep the last confirmed state on a partial, ignored or wrong-task
// response, rather than making a security-sensitive toggle look successful.
export function readTaskAssetSharingUpdate(value: unknown, taskId: string, expected: boolean): boolean {
  if (!value || typeof value !== "object" || Array.isArray(value)) {
    throw new Error("服务未确认任务资产共享设置，请刷新后核对");
  }
  const task = value as Record<string, unknown>;
  if (task.id !== taskId || typeof task.share_global_assets !== "boolean" || task.share_global_assets !== expected) {
    throw new Error("服务未确认任务资产共享设置，请刷新后核对");
  }
  return task.share_global_assets;
}
