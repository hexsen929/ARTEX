// Keep production reverse-proxy streams same-origin while bypassing Next's
// buffered /api rewrite in local development. An explicit empty base is valid.
export function sseBaseURL(
  configured: string | undefined,
  production: boolean,
  location?: { protocol: string; hostname: string },
): string {
  return configured ?? (!production && location ? `${location.protocol}//${location.hostname}:8787` : "");
}

export function sseTicketURL(base: string, path: string, ticket: string): string {
  if (!ticket) throw new Error("SSE ticket is missing");
  const separator = path.includes("?") ? "&" : "?";
  return `${base}${path}${separator}ticket=${encodeURIComponent(ticket)}`;
}

// Preserve the localhost Next/Go gate-cookie behavior without opting arbitrary
// remote endpoints into credentialed cross-origin requests.
export function sseWithCredentials(url: string, page: string): boolean {
  const current = new URL(page);
  const target = new URL(url, current);
  return ["localhost", "127.0.0.1", "[::1]"].includes(target.hostname) && target.hostname === current.hostname;
}
