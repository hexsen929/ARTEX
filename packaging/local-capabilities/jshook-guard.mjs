/** Local jshook stdio lifecycle guard. Imported before server startup. */
import fs from 'node:fs';
const initialParent = process.ppid;
const rssLimitMiB = Number(process.env.JSHOOK_GUARD_RSS_MB ?? 768);
if (!Number.isFinite(rssLimitMiB) || rssLimitMiB <= 0) throw new Error('Invalid JSHOOK_GUARD_RSS_MB');
const brokenTransport = new Set(['EPIPE', 'EBADF', 'ERR_STREAM_DESTROYED', 'ERR_STREAM_WRITE_AFTER_END']);
let exiting = false;
function stop(reason, code = 74) {
  if (exiting) return;
  exiting = true;
  try { fs.writeSync(2, `[jshook-guard] pid=${process.pid} parent=${initialParent} ${reason}\n`); } catch {}
  process.exit(code);
}
for (const [name, stream] of [['stdout', process.stdout], ['stderr', process.stderr]]) {
  stream.prependListener('error', error => stop(`${name}: ${error.code ?? error.message}`));
}
process.prependListener('uncaughtException', error => {
  if (brokenTransport.has(error?.code)) stop(`fatal transport: ${error.code}`);
});
process.prependListener('unhandledRejection', error => {
  if (brokenTransport.has(error?.code)) stop(`fatal transport rejection: ${error.code}`);
});
function connectionClosed() {
  const deadline = setTimeout(() => stop('stdin closed; shutdown deadline exceeded', 0), 2000);
  deadline.unref();
}
process.stdin.once('end', connectionClosed);
process.stdin.once('close', connectionClosed);
process.stdin.once('error', error => stop(`stdin: ${error.code ?? error.message}`));
const parentCheck = setInterval(() => {
  if (initialParent <= 1 || process.ppid !== initialParent) stop('original parent exited', 0);
  if (process.memoryUsage().rss > rssLimitMiB * 1024 * 1024) stop(`RSS exceeded ${rssLimitMiB} MiB`, 75);
}, 500);
parentCheck.unref();
if (initialParent <= 1) stop('started without parent', 0);
