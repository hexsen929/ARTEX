export type UpdateOperation =
  | { kind: "update"; from: string; target: string }
  | { kind: "rollback"; from: string };

export type UpdateObservation = "waiting" | "confirmed" | "rollback_observed" | "unexpected";

export function isUpdateTransportFailure(error: unknown): boolean {
  return error instanceof TypeError || error instanceof Error && (error.name === "AbortError" || error.name === "NetworkError");
}

export function normalizedUpdateVersion(value: unknown): string {
  return typeof value === "string" ? value.trim().replace(/^v(?=\d)/, "") : "";
}

// A healthy but unrelated release is not proof that the requested update won.
// Rollback has no advertised target in the existing API, so observing another
// version must remain explicitly unverified rather than become "updated".
export function observeUpdateVersion(operation: UpdateOperation, value: unknown): UpdateObservation {
  const observed = normalizedUpdateVersion(value);
  if (!observed || observed === normalizedUpdateVersion(operation.from)) return "waiting";
  if (operation.kind === "rollback") return "rollback_observed";
  return observed === normalizedUpdateVersion(operation.target) ? "confirmed" : "unexpected";
}
