// The task-filtered session endpoint returns owned or explicitly shared sessions.
// Only a selected task's shared usage grant can be removed here; ownership and
// the all-task view must never expose an "unbind" action.
export function canUnbindTaskSession(taskId: string | undefined, createdByTask: number | undefined): boolean {
  return Boolean(taskId && /^[1-9]\d*$/.test(taskId) && String(createdByTask ?? 0) !== taskId);
}
