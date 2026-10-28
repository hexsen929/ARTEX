import type { CredentialVerificationStatus, IntranetCredential } from "./types";

export const CREDENTIAL_SECRET_TTL_MS = 30_000;
export const CREDENTIAL_PAGE_SIZE = 20;

export const CREDENTIAL_STATUS_LABELS: Record<CredentialVerificationStatus, string> = {
  unverified: "未验证",
  verified: "验证通过",
  failed: "验证失败",
};

/** Legacy false is unknown, never proof of failure. */
export function credentialStatus(
  credential: Pick<IntranetCredential, "verified"> & Partial<Pick<IntranetCredential, "verification_status">>,
): CredentialVerificationStatus {
  const value = credential.verification_status;
  if (value === "unverified" || value === "verified" || value === "failed") return value;
  return credential.verified ? "verified" : "unverified";
}

/** Explicit external links only; never disclose URL credentials, query strings or fragments. */
export function credentialExternalURL(raw: string | undefined | null): string | undefined {
  if (!raw || !raw.trim()) return undefined;
  const value = raw.trim();
  // biome-ignore lint/suspicious/noControlCharactersInRegex: reject disguised schemes and ambiguous URLs
  if (/[\u0000-\u0020\u007f]/u.test(value)) return undefined;
  let url: URL;
  try {
    url = new URL(value);
  } catch {
    return undefined;
  }
  if (!["http:", "https:"].includes(url.protocol) || !url.hostname || url.username || url.password) return undefined;
  url.search = "";
  url.hash = "";
  return url.href;
}

export function credentialTaskHref(
  credential: Pick<IntranetCredential, "task_id" | "task_available">,
): string | undefined {
  return credential.task_available && Number.isSafeInteger(credential.task_id) && credential.task_id > 0
    ? `/function/tasks/detail?id=${credential.task_id}`
    : undefined;
}

export function credentialMatchesQuery(credential: IntranetCredential, query: string): boolean {
  const words = query.trim().toLocaleLowerCase().split(/\s+/u).filter(Boolean);
  if (words.length === 0) return true;
  const fields = [
    `#${credential.id}`,
    `任务 #${credential.task_id}`,
    credential.task_name,
    credential.username,
    credential.cred_type,
    credential.target,
    credential.target_label,
    credential.domain,
    credential.source,
    credential.source_url,
    CREDENTIAL_STATUS_LABELS[credentialStatus(credential)],
  ]
    .filter(Boolean)
    .join("\n")
    .toLocaleLowerCase();
  // Secret values/masks are deliberately excluded from search.
  return words.every((word) => fields.includes(word));
}

export function credentialPage<T>(
  items: T[],
  requestedPage: number,
  pageSize = CREDENTIAL_PAGE_SIZE,
): { items: T[]; page: number; pages: number } {
  const size = Number.isSafeInteger(pageSize) && pageSize > 0 ? pageSize : CREDENTIAL_PAGE_SIZE;
  const pages = Math.max(1, Math.ceil(items.length / size));
  const page = Math.min(pages, Math.max(1, Number.isSafeInteger(requestedPage) ? requestedPage : 1));
  return { items: items.slice((page - 1) * size, page * size), page, pages };
}
