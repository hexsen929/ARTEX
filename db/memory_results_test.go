package db

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
)

func memoryJobOutcome(t *testing.T, d *DB, job MemoryJob) (string, string, int) {
	t.Helper()
	var state, result string
	var attempts int
	if err := d.QueryRow(`SELECT state,result,attempts FROM memory_jobs WHERE id=$1`, job.ID).Scan(&state, &result, &attempts); err != nil {
		t.Fatal(err)
	}
	return state, result, attempts
}

func TestMemoryResultClosedCategoriesDoNotPersistErrors(t *testing.T) {
	for _, value := range []string{"", "PRIVATE_PROVIDER_BODY_CANARY", "Bearer fixture-private-key"} {
		if NormalizeMemoryResult(value) != "legacy_unknown" {
			t.Fatal("unknown historical result was exposed")
		}
		if _, err := (&DB{}).CompleteMemoryJobWithResult(t.Context(), MemoryJob{}, nil, value); !errors.Is(err, ErrMemoryInput) {
			t.Fatal("untrusted completion category was not rejected before database access")
		}
		if err := (&DB{}).FailMemoryJobWithResult(t.Context(), MemoryJob{}, time.Minute, value); !errors.Is(err, ErrMemoryInput) {
			t.Fatal("untrusted failure category was not rejected before database access")
		}
		if err := (&DB{}).SkipMemoryJobWithResult(t.Context(), MemoryJob{}, value); !errors.Is(err, ErrMemoryInput) {
			t.Fatal("untrusted skip category was not rejected before database access")
		}
	}
	for _, value := range []string{"saved", "empty", "duplicate", "input_invalid", "input_sensitive", "output_invalid", "output_truncated",
		"output_rejected", "output_sensitive", "source_changed", "settings_changed", "cleared", "provider_error", "timeout", "storage_error",
		"lease_expired", "budget_deferred", "no_experience", "failed", "skipped", "legacy_unknown"} {
		if NormalizeMemoryResult(value) != value {
			t.Fatalf("known safe category missing: %q", value)
		}
	}
}

func TestMemoryExplicitOutcomesAreDistinctFromLegacyAndUnknown(t *testing.T) {
	d := memoryFixtureDB(t)
	task := memoryFixtureTask(t, d)
	newJob := func() MemoryJob { return memoryQueueClaim(t, d, memoryFixtureSource(t, d, task)) }
	legacy := newJob()
	if _, err := d.CompleteMemoryJob(t.Context(), legacy, nil); err != nil {
		t.Fatal(err)
	}
	empty := newJob()
	if _, err := d.CompleteMemoryJobWithResult(t.Context(), empty, nil, "empty"); err != nil {
		t.Fatal(err)
	}
	rejected := newJob()
	if err := d.SkipMemoryJobWithResult(t.Context(), rejected, "input_sensitive"); err != nil {
		t.Fatal(err)
	}
	failed := newJob()
	if err := d.FailMemoryJobWithResult(t.Context(), failed, time.Hour, "output_truncated"); err != nil {
		t.Fatal(err)
	}
	unknown := newJob()
	const canary = "PRIVATE_PROVIDER_ERROR_BODY_CANARY"
	if _, err := d.Exec(`UPDATE memory_jobs SET state='skipped',result=$2,lease_token='',lease_until=NULL,updated_at=now() WHERE id=$1`, unknown.ID, canary); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Exec(`UPDATE memory_sources SET last_result=$2,last_result_at=now(),last_job_id=$3 WHERE source_kind=$1 AND source_id=$4`,
		unknown.Source.Kind, canary, unknown.ID, unknown.Source.ID); err != nil {
		t.Fatal(err)
	}
	snapshot, err := d.MemoryLearningSnapshot(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	for _, result := range []string{"no_experience", "empty", "input_sensitive", "output_truncated", "legacy_unknown"} {
		if snapshot.OutcomeCounts[result] < 1 {
			t.Fatalf("missing distinct outcome %q: %+v", result, snapshot.OutcomeCounts)
		}
	}
	if snapshot.LastJob == nil || snapshot.LastJob.Result != "legacy_unknown" || snapshot.RetryableFailed < 1 {
		t.Fatalf("unknown last result or pending retry was misrepresented: %+v", snapshot)
	}
	wire, err := json.Marshal(snapshot)
	if err != nil || strings.Contains(string(wire), canary) {
		t.Fatal("snapshot leaked unknown historical result contents")
	}
	state, result, _ := memoryJobOutcome(t, d, legacy)
	if state != "done" || result != "no_experience" {
		t.Fatal("read-only snapshot rewrote legacy classification")
	}
	state, result, _ = memoryJobOutcome(t, d, rejected)
	if state != "skipped" || result != "input_sensitive" {
		t.Fatal("safety rejection was mislabeled as successful empty extraction")
	}
}

func TestMemoryDuplicateOutcomeAndDeletedEntryCount(t *testing.T) {
	d := memoryFixtureDB(t)
	task := memoryFixtureTask(t, d)
	before, err := d.MemoryLearningSnapshot(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	job := memoryQueueClaim(t, d, memoryFixtureSource(t, d, task))
	input := MemoryInput{Title: "conditional fixture lesson", Content: "use the documented fixture protocol under the recorded conditions", Kind: "lesson",
		Applicability: "fixture-tool v1.2 on fixture-os", EvidenceIDs: []string{memoryStage(t, job.Source).Facts[0].ID}}
	entries, err := d.CompleteMemoryJobWithResult(t.Context(), job, []MemoryInput{input}, "empty")
	if err != nil || len(entries) != 1 {
		t.Fatalf("saved outcome: %+v %v", entries, err)
	}
	// Simulate a ledger retry with the exact already-present identity. This is
	// not a new model call, and completion must not claim that no lesson exists.
	job.LeaseToken = "fixture-duplicate-lease"
	if _, err := d.Exec(`UPDATE memory_jobs SET state='running',lease_token=$2,lease_until=now()+interval '1 minute' WHERE id=$1`, job.ID, job.LeaseToken); err != nil {
		t.Fatal(err)
	}
	if repeated, err := d.CompleteMemoryJobWithResult(t.Context(), job, []MemoryInput{input}, "empty"); err != nil || len(repeated) != 0 {
		t.Fatalf("duplicate completion=%+v %v", repeated, err)
	}
	if state, result, _ := memoryJobOutcome(t, d, job); state != "done" || result != "duplicate" {
		t.Fatalf("duplicate result=%q/%q", state, result)
	}
	snapshot, err := d.MemoryLearningSnapshot(t.Context())
	if err != nil || snapshot.EntryCount != before.EntryCount+1 {
		t.Fatalf("live entry count=%d, before=%d err=%v", snapshot.EntryCount, before.EntryCount, err)
	}
	if err := d.DeleteMemoryEntry(t.Context(), entries[0].ID, entries[0].Version); err != nil {
		t.Fatal(err)
	}
	after, err := d.MemoryLearningSnapshot(t.Context())
	if err != nil || after.EntryCount != before.EntryCount || after.OutcomeCounts["duplicate"] < 1 {
		t.Fatalf("deleted entry became an empty-extraction inference: %+v %v", after, err)
	}
}

func TestMemorySettingsClearAndSourceChangesHaveExplicitOutcomes(t *testing.T) {
	for _, outcome := range []string{"settings_changed", "cleared", "source_changed"} {
		t.Run(outcome, func(t *testing.T) {
			d := memoryFixtureDB(t)
			task := memoryFixtureTask(t, d)
			job := memoryQueueClaim(t, d, memoryFixtureSource(t, d, task))
			if err := d.FailMemoryJobWithResult(t.Context(), job, time.Minute, "provider_error"); err != nil {
				t.Fatal(err)
			}
			switch outcome {
			case "settings_changed":
				settings, err := d.GetMemorySettings(t.Context())
				if err != nil {
					t.Fatal(err)
				}
				if _, err := d.UpdateMemorySettings(t.Context(), settings, settings.Epoch); err != nil {
					t.Fatal(err)
				}
			case "cleared":
				if err := d.ClearMemoryEntries(t.Context()); err != nil {
					t.Fatal(err)
				}
			case "source_changed":
				if _, err := d.Exec(`UPDATE exploration_nodes SET payload=payload||'{"new_condition":"changed fixture observation"}'::jsonb,content_version=content_version+1 WHERE id=$1`, memorySourceFactID(t, job.Source)); err != nil {
					t.Fatal(err)
				}
			}
			if state, result, _ := memoryJobOutcome(t, d, job); state != "skipped" || result != outcome {
				t.Fatalf("changed dependency retained old provider failure: %q/%q", state, result)
			}
			if err := d.FailMemoryJobWithResult(t.Context(), job, time.Minute, "provider_error"); !errors.Is(err, ErrMemoryStale) {
				t.Fatalf("late failure rewrote stopped outcome: %v", err)
			}
		})
	}
}

func TestMemoryLateOutcomeCannotConsumeNewSettingsGeneration(t *testing.T) {
	d := memoryFixtureDB(t)
	task := memoryFixtureTask(t, d)
	source := memoryFixtureSource(t, d, task)
	old := memoryQueueClaim(t, d, source)
	settings, err := d.GetMemorySettings(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.UpdateMemorySettings(t.Context(), settings, settings.Epoch); err != nil {
		t.Fatal(err)
	}
	current, err := d.GetMemorySource(t.Context(), source.Kind, source.ID)
	if err != nil || current == nil {
		t.Fatalf("new settings source: %+v %v", current, err)
	}
	current.Historical = true
	newJob := memoryQueueClaim(t, d, *current)
	if err := d.SkipMemoryJobWithResult(t.Context(), old, "settings_changed"); !errors.Is(err, ErrMemoryStale) {
		t.Fatalf("old lease affected new settings: %v", err)
	}
	var completed sql.NullInt64
	if err := d.QueryRow(`SELECT last_completed_version FROM memory_sources WHERE source_kind=$1 AND source_id=$2`, source.Kind, source.ID).Scan(&completed); err != nil || completed.Valid {
		t.Fatalf("old snapshot consumed a new generation: %+v %v", completed, err)
	}
	if state, _, _ := memoryJobOutcome(t, d, newJob); state != "running" {
		t.Fatal("new settings lease was retired by an old callback")
	}
	if err := d.SkipMemoryJobWithResult(t.Context(), newJob, "input_invalid"); err != nil {
		t.Fatal(err)
	}
}

func TestMemoryFailureMetadataDoesNotRebuildUnreadableSource(t *testing.T) {
	d := memoryFixtureDB(t)
	task := memoryFixtureTask(t, d)
	job := memoryQueueClaim(t, d, memoryFixtureSource(t, d, task))
	memoryRejectBuild(t, d, mustMemoryNodeID(t, job.Source.ID))
	if err := d.FailMemoryJobWithResult(t.Context(), job, time.Minute, "storage_error"); err != nil {
		t.Fatalf("metadata failure path tried to rebuild unreadable content: %v", err)
	}
	if state, result, attempts := memoryJobOutcome(t, d, job); state != "failed" || result != "storage_error" || attempts != 1 {
		t.Fatalf("source read failure became empty success: %q/%q/%d", state, result, attempts)
	}
}

func TestMemorySkipUnreadableInputDoesNotRebuildBody(t *testing.T) {
	d := memoryFixtureDB(t)
	task := memoryFixtureTask(t, d)
	job := memoryQueueClaim(t, d, memoryFixtureSource(t, d, task))
	memoryRejectBuild(t, d, mustMemoryNodeID(t, job.Source.ID))
	if err := d.SkipMemoryJobWithResult(t.Context(), job, "input_invalid"); err != nil {
		t.Fatalf("rejecting an unreadable input rebuilt its body: %v", err)
	}
	if state, result, attempts := memoryJobOutcome(t, d, job); state != "skipped" || result != "input_invalid" || attempts != 1 {
		t.Fatalf("invalid input was not terminal: %q/%q/%d", state, result, attempts)
	}
	var calls int
	if err := d.QueryRow(`SELECT count(*) FROM memory_audit WHERE job_id=$1 AND action='extract_admitted'`, job.ID).Scan(&calls); err != nil || calls != 0 {
		t.Fatalf("rejecting input created HTTP admission: %d %v", calls, err)
	}
}

func TestMemorySettingsFailureAndUnclaimedCancellationCounts(t *testing.T) {
	d := memoryFixtureDB(t)
	task := memoryFixtureTask(t, d)
	before, err := d.MemoryLearningSnapshot(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	job := memoryQueueClaim(t, d, memoryFixtureSource(t, d, task))
	if _, err := d.Exec(`UPDATE memory_jobs SET attempts=3 WHERE id=$1`, job.ID); err != nil {
		t.Fatal(err)
	}
	if err := d.FailMemoryJobWithResult(t.Context(), job, time.Minute, "settings_changed"); err != nil {
		t.Fatal(err)
	}
	afterFailure, err := d.MemoryLearningSnapshot(t.Context())
	if err != nil || afterFailure.StoppedFailed != before.StoppedFailed+1 {
		t.Fatalf("stopped third settings failure lost: before=%+v after=%+v %v", before, afterFailure, err)
	}
	source := memoryFixtureSource(t, d, task)
	if queued, err := d.QueueMemoryExtraction(t.Context(), source); err != nil || !queued {
		t.Fatalf("queue unused fixture: %v %v", queued, err)
	}
	settings, err := d.GetMemorySettings(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.UpdateMemorySettings(t.Context(), settings, settings.Epoch); err != nil {
		t.Fatal(err)
	}
	afterCancel, err := d.MemoryLearningSnapshot(t.Context())
	if err != nil || afterCancel.Failed != afterFailure.Failed || afterCancel.StoppedFailed != afterFailure.StoppedFailed ||
		afterCancel.OutcomeCounts["settings_changed"] != afterFailure.OutcomeCounts["settings_changed"]+1 {
		t.Fatalf("unclaimed configuration cancellation became a failed call: %+v %+v %v", afterFailure, afterCancel, err)
	}
}

func TestMemoryLateOutcomeCannotConsumeNewSourceGeneration(t *testing.T) {
	d := memoryFixtureDB(t)
	memoryEnableIncrementalHistory(t, d)
	task := memoryFixtureTask(t, d)
	store := d.Exploration(task.ExplorationID)
	intent, _ := memoryFixtureIntent(t, d, task, "open", 1)
	if claimed, err := store.ClaimIntent(intent, "worker"); err != nil || !claimed {
		t.Fatalf("claim first native generation: %v %v", claimed, err)
	}
	// ReopenIntent deliberately excludes successfully completed work. Start
	// from a genuinely blocked run, then use its public rerun/claim path.
	source := memoryFinishStage(t, d, task, intent, "blocked")
	source.Historical = false
	old := memoryQueueClaim(t, d, source)
	if reopened, err := store.ReopenIntent(intent); err != nil || !reopened {
		t.Fatalf("reopen native generation: %v %v", reopened, err)
	}
	if claimed, err := store.ClaimIntent(intent, "worker"); err != nil || !claimed {
		t.Fatalf("claim new native generation: %v %v", claimed, err)
	}
	current := memoryFinishStage(t, d, task, intent, "done")
	current.Historical = false
	newJob := memoryQueueClaim(t, d, current)
	if newJob.Source.Version <= old.Source.Version {
		t.Fatal("native rerun did not create a new completion generation")
	}
	if err := d.SkipMemoryJobWithResult(t.Context(), old, "source_changed"); !errors.Is(err, ErrMemoryStale) {
		t.Fatalf("old source callback was not fenced: %v", err)
	}
	var completed sql.NullInt64
	if err := d.QueryRow(`SELECT last_completed_version FROM memory_sources WHERE source_kind=$1 AND source_id=$2`, source.Kind, source.ID).Scan(&completed); err != nil || completed.Valid {
		t.Fatalf("late source result consumed a new completion generation: %+v %v", completed, err)
	}
	if state, _, _ := memoryJobOutcome(t, d, newJob); state != "running" {
		t.Fatal("late source callback retired the new native lease")
	}
	if err := d.SkipMemoryJobWithResult(t.Context(), newJob, "input_sensitive"); err != nil {
		t.Fatal(err)
	}
}

func TestMemoryLearningSnapshotCancellationIsReadOnly(t *testing.T) {
	d := memoryFixtureDB(t)
	task := memoryFixtureTask(t, d)
	job := memoryQueueClaim(t, d, memoryFixtureSource(t, d, task))
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := d.MemoryLearningSnapshot(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled observation returned apparent zero success: %v", err)
	}
	if state, _, attempts := memoryJobOutcome(t, d, job); state != "running" || attempts != 1 {
		t.Fatal("cancelled observation mutated the job")
	}
}

// Pause the budget read after the first job-count query. The view is scoped to
// this isolated fixture database and restored before its task cleanup. Advisory
// locking only coordinates the test; it never writes from the read-only tx.
func memorySnapshotBarrier(t *testing.T, d *DB) (func(), func()) {
	t.Helper()
	const key int64 = 705731189
	ddl, err := d.BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer ddl.Rollback()
	for _, query := range []string{
		`ALTER TABLE memory_audit RENAME TO memory_audit_snapshot_base`,
		`CREATE FUNCTION memory_snapshot_test_barrier() RETURNS boolean LANGUAGE plpgsql VOLATILE AS $$ BEGIN PERFORM pg_advisory_xact_lock(705731189::bigint); RETURN true; END $$`,
		`CREATE VIEW memory_audit AS SELECT * FROM memory_audit_snapshot_base WHERE memory_snapshot_test_barrier()`,
	} {
		if _, err := ddl.ExecContext(t.Context(), query); err != nil {
			t.Fatal(err)
		}
	}
	if err := ddl.Commit(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if _, err := d.ExecContext(ctx, `DROP VIEW memory_audit; ALTER TABLE memory_audit_snapshot_base RENAME TO memory_audit; DROP FUNCTION memory_snapshot_test_barrier()`); err != nil {
			t.Error(err)
		}
	})
	blocker, err := d.Conn(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := blocker.ExecContext(t.Context(), `SELECT pg_advisory_lock($1::bigint)`, key); err != nil {
		blocker.Close()
		t.Fatal(err)
	}
	var once sync.Once
	release := func() {
		once.Do(func() {
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			if _, err := blocker.ExecContext(ctx, `SELECT pg_advisory_unlock($1::bigint)`, key); err != nil {
				t.Error(err)
			}
			if err := blocker.Close(); err != nil {
				t.Error(err)
			}
		})
	}
	t.Cleanup(release)
	wait := func() {
		t.Helper()
		deadline := time.NewTimer(2 * time.Second)
		defer deadline.Stop()
		tick := time.NewTicker(5 * time.Millisecond)
		defer tick.Stop()
		for {
			var blocked bool
			if err := d.QueryRowContext(t.Context(), `SELECT EXISTS(SELECT 1 FROM pg_locks WHERE locktype='advisory'
				AND classid=0 AND objid=($1::bigint)::oid AND objsubid=1 AND NOT granted
				AND database=(SELECT oid FROM pg_database WHERE datname=current_database()))`, key).Scan(&blocked); err != nil {
				t.Fatal(err)
			}
			if blocked {
				return
			}
			select {
			case <-tick.C:
			case <-deadline.C:
				t.Fatal("snapshot did not reach the controlled budget-read barrier")
			}
		}
	}
	return wait, release
}

func TestMemoryLearningSnapshotUsesOneReadOnlyGeneration(t *testing.T) {
	d := memoryFixtureDB(t)
	task := memoryFixtureTask(t, d)
	job := memoryQueueClaim(t, d, memoryFixtureSource(t, d, task))
	var beforeEntries, beforeEmpty int64
	if err := d.QueryRow(`SELECT count(*) FROM memory_entries WHERE state<>'deleted'`).Scan(&beforeEntries); err != nil {
		t.Fatal(err)
	}
	if err := d.QueryRow(`SELECT count(*) FROM memory_jobs job JOIN memory_sources source ON source.id=job.source_ref
		WHERE source.source_kind IN ('intent_stage','task_stage') AND job.state IN ('done','failed','skipped') AND job.result='empty'`).Scan(&beforeEmpty); err != nil {
		t.Fatal(err)
	}
	wait, release := memorySnapshotBarrier(t, d)
	type observation struct {
		snapshot MemoryLearningSnapshot
		err      error
	}
	result := make(chan observation, 1)
	go func() { snapshot, err := d.MemoryLearningSnapshot(t.Context()); result <- observation{snapshot, err} }()
	wait()
	writer, err := d.BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Rollback()
	if _, err := writer.ExecContext(t.Context(), `UPDATE memory_jobs SET state='done',result='empty',lease_token='',lease_until=NULL,updated_at=now() WHERE id=$1`, job.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := writer.ExecContext(t.Context(), `INSERT INTO memory_entries(title,content,content_hash,kind,origin_task_id)
		VALUES('snapshot fixture','snapshot writer fixture',$1,'lesson',$2)`, memoryContentHash("snapshot writer fixture"), task.ID); err != nil {
		t.Fatal(err)
	}
	if err := writer.Commit(); err != nil {
		t.Fatal(err)
	}
	release()
	select {
	case observed := <-result:
		if observed.err != nil {
			t.Fatal(observed.err)
		}
		if observed.snapshot.EntryCount != beforeEntries || observed.snapshot.OutcomeCounts["empty"] != beforeEmpty ||
			(observed.snapshot.LastJob != nil && observed.snapshot.LastJob.ID == job.ID) {
			t.Fatalf("one observation mixed pre/post-commit generations: %+v", observed.snapshot)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("bounded read-only observation did not finish")
	}
	after, err := d.MemoryLearningSnapshot(t.Context())
	if err != nil || after.EntryCount != beforeEntries+1 || after.OutcomeCounts["empty"] != beforeEmpty+1 {
		t.Fatalf("next observation failed to see committed writer: %+v %v", after, err)
	}
}

func TestMemoryLearningSnapshotCancelsBlockedReadWithoutWrites(t *testing.T) {
	d := memoryFixtureDB(t)
	task := memoryFixtureTask(t, d)
	job := memoryQueueClaim(t, d, memoryFixtureSource(t, d, task))
	wait, release := memorySnapshotBarrier(t, d)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	result := make(chan error, 1)
	go func() { _, err := d.MemoryLearningSnapshot(ctx); result <- err }()
	wait()
	cancel()
	select {
	case err := <-result:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("cancelled read was reported as a successful empty observation: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("cancelled observation remained blocked")
	}
	release()
	if state, _, attempts := memoryJobOutcome(t, d, job); state != "running" || attempts != 1 {
		t.Fatal("cancelled blocked read modified the job")
	}
}

func TestMemoryThirdExpiredLeaseTerminatesWithoutFourthAttempt(t *testing.T) {
	d := memoryFixtureDB(t)
	memoryEnableIncrementalHistory(t, d)
	task := memoryFixtureTask(t, d)
	source := memoryFixtureSource(t, d, task)
	source.Historical = false
	job := memoryQueueClaim(t, d, source)
	for attempt := 1; attempt <= 3; attempt++ {
		if err := d.ValidateMemoryExtractionJob(t.Context(), job); err != nil {
			t.Fatal(err)
		}
		if _, err := d.Exec(`UPDATE memory_jobs SET lease_until=now()-interval '1 second' WHERE id=$1`, job.ID); err != nil {
			t.Fatal(err)
		}
		if attempt < 3 {
			recovered, err := d.ClaimMemoryJob(t.Context(), time.Minute)
			if err != nil || recovered == nil || recovered.ID != job.ID || recovered.Attempts != attempt+1 {
				t.Fatalf("lease recovery %d: %+v %v", attempt, recovered, err)
			}
			job = *recovered
		}
	}
	// Reaping must also commit while the hour budget is full, and the status
	// reader itself remains observational rather than performing the repair.
	if _, err := d.Exec(`UPDATE memory_settings SET max_jobs_per_hour=3 WHERE singleton`); err != nil {
		t.Fatal(err)
	}
	if _, err := d.MemoryLearningSnapshot(t.Context()); err != nil {
		t.Fatal(err)
	}
	if state, _, _ := memoryJobOutcome(t, d, job); state != "running" {
		t.Fatal("status read performed a lifecycle write")
	}
	if next, err := d.ClaimMemoryJob(t.Context(), time.Minute); err != nil || next != nil {
		t.Fatalf("exhausted third lease was sent again: %+v %v", next, err)
	}
	if state, result, attempts := memoryJobOutcome(t, d, job); state != "skipped" || result != "lease_expired" || attempts != 3 {
		t.Fatalf("third abandoned lease stayed stuck: %q/%q/%d", state, result, attempts)
	}
	if _, err := d.CompleteMemoryJobWithResult(t.Context(), job, nil, "empty"); !errors.Is(err, ErrMemoryStale) {
		t.Fatalf("expired token committed after retirement: %v", err)
	}
	if err := d.FailMemoryJobWithResult(t.Context(), job, time.Minute, "timeout"); !errors.Is(err, ErrMemoryStale) {
		t.Fatalf("expired token failed a retired job: %v", err)
	}
	var starts, admissions int
	if err := d.QueryRow(`SELECT count(*) FILTER(WHERE action='extract_start'),count(*) FILTER(WHERE action='extract_admitted') FROM memory_audit WHERE job_id=$1`, job.ID).Scan(&starts, &admissions); err != nil {
		t.Fatal(err)
	}
	if starts != 3 || admissions != 3 {
		t.Fatalf("reaper altered call accounting: %d/%d", starts, admissions)
	}
	if found, err := d.FindMemorySources(t.Context(), 100); err != nil || memoryContainsSource(found, source) {
		t.Fatalf("retired generation was rediscovered: %+v %v", found, err)
	}
}

func TestMemoryBudgetReleaseWaitsForBothWindowsAndLoweredLimit(t *testing.T) {
	d := memoryFixtureDB(t)
	task := memoryFixtureTask(t, d)
	var clock time.Time
	if err := d.QueryRow(`SELECT now()`).Scan(&clock); err != nil {
		t.Fatal(err)
	}
	for action, minutes := range map[string][]int{"extract_start": {50, 40, 30, 20, 10}, "extract_admitted": {45, 35, 25, 15}} {
		for _, ago := range minutes {
			if _, err := d.Exec(`INSERT INTO memory_audit(task_id,action,created_at) VALUES($1,$2,$3)`, task.ID, action, clock.Add(-time.Duration(ago)*time.Minute)); err != nil {
				t.Fatal(err)
			}
		}
	}
	if _, err := d.Exec(`UPDATE memory_settings SET max_jobs_per_hour=3 WHERE singleton`); err != nil {
		t.Fatal(err)
	}
	snapshot, err := d.MemoryLearningSnapshot(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.StartsLastHour != 5 || snapshot.AttemptsLastHour != 4 || snapshot.BudgetLimit != 3 || snapshot.NextBudgetAt == nil || !snapshot.NextBudgetAt.Equal(clock.Add(30*time.Minute)) {
		t.Fatalf("lowered quota promised the first expiry instead of an available slot: %+v", snapshot)
	}
	if _, err := d.Exec(`UPDATE memory_settings SET max_jobs_per_hour=6 WHERE singleton`); err != nil {
		t.Fatal(err)
	}
	snapshot, err = d.MemoryLearningSnapshot(t.Context())
	if err != nil || snapshot.NextBudgetAt != nil || snapshot.BudgetLimit != 6 {
		t.Fatalf("unblocked budget invented a waiting window: %+v %v", snapshot, err)
	}
}

func TestMemoryClosedOutcomeCategoriesSQLAndCodeStayAligned(t *testing.T) {
	for _, value := range strings.FieldsFunc(memoryKnownResultSQL, func(r rune) bool { return r == '(' || r == ')' || r == ',' || r == '\'' || r == ' ' || r == '\n' }) {
		if NormalizeMemoryResult(value) != value {
			t.Fatalf("SQL/API outcome vocabularies disagree: %s", fmt.Sprint(value))
		}
	}
}
