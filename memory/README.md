# ARTEX shared memory

This package prepares short reference notes, not findings, authorization, or
executable instructions. PostgreSQL owns versions, source dependencies and jobs;
the server owns administrator endpoints and provider selection; `agent` and
`memcontext` own transient per-call recall and final HTTP admission.

## Automatic operation

- The feature, automatic extraction and global sharing are initially enabled.
  There is no per-entry approval step. Disabling recall does not delete data.
- Incremental learning begins at the deployment's `learn_after` boundary.
  Administrators can explicitly enqueue at most 50 historical sources at a time.
- Sources are allowlisted task records and positively attributed human inputs.
  Assistant replies, generated digests, raw traffic, credentials and reports are
  not automatic learning sources. A remembered assistant repetition must not
  become a new independent source.
- Extraction uses the selected concrete model, without tools, under input,
  output, deadline, hourly admission, lease and queue bounds. Content sent for
  extraction/recall leaves the local database for that model service.
- Source and output checks reject obvious secrets and instruction-like content;
  these checks are not a guarantee of perfect secret detection or truth.

## Retrieval and lifecycle

- A global pool is searchable by all eligible task/conversation contexts. Only
  relevant, current, bounded notes are inserted into primary model requests.
  Restricted data-processing roles and auxiliary completions do not recall.
- The full temporary block, including references, is capped conservatively by
  UTF-8 bytes as a token upper bound and by 5% of the configured context window.
  Automatically generated notes are short. Long manual notes may supply only an
  explicit index, not a truncated statement or an implied full-text read.
- Every real HTTP attempt revalidates dependencies after rate-limit queueing.
  Invalid dependencies stop stale bytes, not unrelated tasks or tool actions.
  Requests admitted before a change may already be in flight.
- Source updates/deletion invalidate dependent notes. Human edits are not
  overwritten by extraction. Deletion suppresses re-extraction from that source.
- Deletion covers controlled memory bodies and future recall. It does not erase
  arbitrary prior business messages, external model retention or old backups.
- Memory-dependent/extraction model recordings retain explicit metadata and
  usage only. Side checkpoints exclude the temporary memory system segment.

All regression tests and builds run in GitHub Actions, not on the deployment
machine. Installing a release and applying its database schema are separate
operations from developing this branch.
