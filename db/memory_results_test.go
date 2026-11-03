package db

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
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
