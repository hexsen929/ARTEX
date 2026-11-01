/** Case-insensitive, literal, non-overlapping packet search count. */
export function countPacketMatches(value: string, query: string): number {
  const needle = query.trim().toLowerCase();
  if (!needle) return 0;
  const haystack = value.toLowerCase();
  let count = 0;
  let cursor = 0;
  while (cursor <= haystack.length - needle.length) {
    const index = haystack.indexOf(needle, cursor);
    if (index < 0) return count;
    count += 1;
    cursor = index + needle.length;
  }
  // A match at EOF, a short suffix, or a query longer than the packet all
  // finish the loop normally. Always return a number rather than undefined.
  return count;
}
