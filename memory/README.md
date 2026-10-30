# ARTEX cross-task experience

This package prepares short reference notes, not findings, authorization, or
executable instructions. PostgreSQL owns versions, source dependencies and jobs;
the server owns administrator endpoints and provider selection; `agent` and
`memcontext` own transient per-call recall and final HTTP admission.

## Automatic operation

- The feature, automatic extraction and global sharing are initially enabled.
  There is no per-entry approval step. Disabling recall does not delete data.
- Incremental learning begins at the deployment's `learn_after` boundary. Native
  worker intents and tasks must end and settle for 30 seconds before learning.
  One stage has one bounded summary job, selecting at most eight complete native
  fact summaries. Coverage limitations and real confidence remain explicit.
  Administrators can explicitly enqueue at most 50 historical stages at a time.
- Briefs, ordinary chat, assistant repetitions, untracked digests, raw evidence,
  traffic, credentials and reports are not automatic sources. Task-level fallback
  includes only facts not already owned by worker stages.
- A stable native completion generation changes only on a real nonterminal to
  terminal transition. Persistent admitted/completed generation watermarks survive
  job cleanup. A late member change invalidates experience, not another automatic
  call for the same stage; explicit historical review can reprocess changed or
  failed snapshots. Normal bounded retries of the same job remain possible.
- Extraction uses the selected concrete model, without tools, under input,
  output, deadline, hourly admission, lease and queue bounds. Content sent for
  extraction/recall leaves the local database for that model service. Native fact
  summaries may include source-task context; only accepted generic experience is
  saved for cross-task recall. Extra asset identifiers stay host-side for output
  rejection rather than being added to the model input.
- Fresh installations default to three stage starts and three real HTTP attempts
  per hour, with an independent 200-job queue cap. Existing explicit choices are
  preserved. Status distinguishes waiting, queueing, retries, budget limits and
  a legitimate result with no new experience.
- Source and output checks reject obvious secrets and instruction-like content;
  these checks are not a guarantee of perfect secret detection or truth.

## Retrieval and lifecycle

- Only environment, procedure and troubleshooting experience is eligible for
  cross-task recall. Target records require a real source task and remain within
  that task, even when cross-task sharing is enabled. Retrieval and final HTTP
  admission use the same computed sharing rule.
- Automatically generated notes cite included native fact IDs and preserve
  explicit conditions. Unknown environment/version remains unknown; a source
  revision is never a tool version. Current record_fact inputs do not magically
  collect platform/tool versions. A site's negative finding is not a rule to skip
  that vulnerability class elsewhere.
- Only relevant, current, bounded notes are inserted into primary model requests.
  Restricted data-processing roles and auxiliary completions do not recall.
- The full temporary block, including references, is capped conservatively by
  UTF-8 bytes as a token upper bound and by 5% of the configured context window.
  Automatically generated notes are short. Long manual notes may supply only an
  explicit index, not a truncated statement or an implied full-text read.
- Every real HTTP attempt revalidates dependencies after rate-limit queueing.
  Invalid dependencies stop stale bytes, not unrelated tasks or tool actions.
  Requests admitted before a change may already be in flight.
- Source updates/deletion invalidate dependent notes. Human edits are not
  overwritten by extraction. Deletion suppresses subsequent extraction from that
  source ID, including a rerun; other new source IDs are unaffected.
- Deletion covers controlled memory bodies and future recall. It does not erase
  arbitrary prior business messages, external model retention or old backups.
- Memory-dependent/extraction model recordings retain explicit metadata and
  usage only. Side checkpoints exclude the temporary memory system segment.

All regression tests and builds run in GitHub Actions, not on the deployment
machine. Installing a release and applying its database schema are separate
operations from developing this branch.
