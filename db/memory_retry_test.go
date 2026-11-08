package db

import (
	"errors"
	"testing"
	"time"
)

func readyMemoryRetry(t *testing.T, d *DB, job MemoryJob) MemoryJob {
	t.Helper()
	if _, err := d.Exec(`UPDATE memory_jobs SET retry_at=now()-interval '1 second' WHERE id=$1`, job.ID); err != nil {
		t.Fatal(err)
	}
	claimed, err := d.ClaimMemoryJob(t.Context(), time.Minute)
	if err != nil || claimed == nil || claimed.ID != job.ID {
		t.Fatalf("retry not claimable: %v", err)
	}
	return *claimed
}

func TestMemoryBudgetDeferralPreservesUnsentRetries(t *testing.T) {
	d := memoryFixtureDB(t)
	task := memoryFixtureTask(t, d)
	job := memoryQueueClaim(t, d, memoryFixtureSource(t, d, task))
	// An earlier lease really attempted HTTP. Its admission must not cause a
	// later unsent lease to be charged, and its failed attempt must not vanish.
	if err := d.ValidateMemoryExtractionJob(t.Context(), job); err != nil {
		t.Fatal(err)
	}
	if err := d.FailMemoryJobWithResult(t.Context(), job, time.Minute, "output_truncated"); err != nil {
		t.Fatal(err)
	}
	settings, err := d.GetMemorySettings(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	for range 3 {
		previous := job
		job = readyMemoryRetry(t, d, job)
		if job.Attempts != 2 || job.Truncations != 1 {
			t.Fatal("quota waiting lost prior retry or truncation state")
		}
		if err := d.FailMemoryJobWithResult(t.Context(), previous, time.Minute, "budget_deferred"); !errors.Is(err, ErrMemoryStale) {
			t.Fatal("old lease was allowed to refund a new claim")
		}
		if _, err := d.Exec(`INSERT INTO memory_audit(task_id,action) SELECT $1,'extract_admitted' FROM generate_series(1,$2::int)`, task.ID, settings.MaxJobsPerHour); err != nil {
			t.Fatal(err)
		}
		if err := d.ValidateMemoryExtractionJob(t.Context(), job); !errors.Is(err, ErrMemoryBudget) {
			t.Fatalf("fixture did not hit HTTP quota: %v", err)
		}
		if err := d.FailMemoryJobWithResult(t.Context(), job, time.Minute, "budget_deferred"); err != nil {
			t.Fatal(err)
		}
		if state, result, attempts := memoryJobOutcome(t, d, job); state != "failed" || result != "budget_deferred" || attempts != 1 {
			t.Fatalf("unsent quota wait consumed attempts: %s/%s/%d", state, result, attempts)
		}
		if next, err := d.ClaimMemoryJob(t.Context(), time.Minute); err != nil || next != nil {
			t.Fatal("quota refund bypassed the hourly gate")
		}
		// Move only this fixture's ledger to an earlier hour; retain every row.
		if _, err := d.Exec(`UPDATE memory_audit SET created_at=now()-interval '2 hours' WHERE task_id=$1`, task.ID); err != nil {
			t.Fatal(err)
		}
	}
	for attempt := 2; attempt <= 3; attempt++ {
		job = readyMemoryRetry(t, d, job)
		if job.Attempts != attempt {
			t.Fatal("actual failures no longer honor the three-attempt boundary")
		}
		if err := d.ValidateMemoryExtractionJob(t.Context(), job); err != nil {
			t.Fatal(err)
		}
		if err := d.FailMemoryJobWithResult(t.Context(), job, time.Minute, "output_truncated"); err != nil {
			t.Fatal(err)
		}
	}
	if state, _, attempts := memoryJobOutcome(t, d, job); state != "skipped" || attempts != 3 {
		t.Fatal("three real attempts were not terminal")
	}
	var starts, admitted int
	if err := d.QueryRow(`SELECT count(*) FILTER (WHERE action='extract_start'),count(*) FILTER (WHERE action='extract_admitted')
		FROM memory_audit WHERE job_id=$1`, job.ID).Scan(&starts, &admitted); err != nil || starts != 6 || admitted != 3 {
		t.Fatalf("quota waiting rewrote the ledger: starts=%d admitted=%d err=%v", starts, admitted, err)
	}
}

func TestMemoryBudgetDeniedSDKRetryKeepsAdmittedAttempt(t *testing.T) {
	d := memoryFixtureDB(t)
	task := memoryFixtureTask(t, d)
	job := memoryQueueClaim(t, d, memoryFixtureSource(t, d, task))
	for attempt := 1; attempt <= 3; attempt++ {
		if attempt > 1 {
			job = readyMemoryRetry(t, d, job)
		}
		if err := d.ValidateMemoryExtractionJob(t.Context(), job); err != nil {
			t.Fatal(err)
		}
		result := "provider_error"
		if attempt == 3 {
			settings, err := d.GetMemorySettings(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			if _, err := d.Exec(`INSERT INTO memory_audit(task_id,action) SELECT $1,'extract_admitted' FROM generate_series(1,$2::int)`, task.ID, settings.MaxJobsPerHour); err != nil {
				t.Fatal(err)
			}
			if err := d.ValidateMemoryExtractionJob(t.Context(), job); !errors.Is(err, ErrMemoryBudget) {
				t.Fatal("second HTTP admission was not stopped by quota")
			}
			result = "budget_deferred"
		}
		if err := d.FailMemoryJobWithResult(t.Context(), job, time.Minute, result); err != nil {
			t.Fatal(err)
		}
	}
	if state, result, attempts := memoryJobOutcome(t, d, job); state != "skipped" || result != "budget_deferred" || attempts != 3 {
		t.Fatalf("denied SDK retry refunded a sent attempt: %s/%s/%d", state, result, attempts)
	}
}

func TestMemoryRecoveryMigrationPreservesPendingAndTerminalJobs(t *testing.T) {
	d := memoryFixtureDB(t)
	task := memoryFixtureTask(t, d)
	running := memoryQueueClaim(t, d, memoryFixtureSource(t, d, task))
	pending := memoryQueueClaim(t, d, memoryFixtureSource(t, d, task))
	if err := d.FailMemoryJobWithResult(t.Context(), pending, time.Hour, "output_truncated"); err != nil {
		t.Fatal(err)
	}
	terminal := memoryQueueClaim(t, d, memoryFixtureSource(t, d, task))
	if _, err := d.Exec(`UPDATE memory_jobs SET state='skipped',result='output_truncated',attempts=3 WHERE id=$1`, terminal.ID); err != nil {
		t.Fatal(err)
	}
	// Reconstruct the old schema only inside a rollback-only CI transaction.
	tx, err := d.BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`ALTER TABLE memory_jobs DROP COLUMN lease_admitted, DROP COLUMN output_truncations`); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(schemaSQL); err != nil {
		t.Fatalf("upgrade old memory schema: %v", err)
	}
	for _, tc := range []struct {
		id          int64
		state       string
		truncations int
	}{{running.ID, "running", 0}, {pending.ID, "failed", 1}, {terminal.ID, "skipped", 0}} {
		var state string
		var truncations int
		var admitted bool
		if err := tx.QueryRow(`SELECT state,output_truncations,lease_admitted FROM memory_jobs WHERE id=$1`, tc.id).Scan(&state, &truncations, &admitted); err != nil {
			t.Fatal(err)
		}
		if state != tc.state || truncations != tc.truncations || !admitted {
			t.Fatal("migration revived a terminal job or invented an unsent old lease")
		}
	}
	if _, err := tx.Exec(`UPDATE memory_jobs SET output_truncations=2,lease_admitted=false WHERE id=$1`, pending.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(schemaSQL); err != nil {
		t.Fatalf("reapply memory schema: %v", err)
	}
	var truncations int
	var admitted bool
	if err := tx.QueryRow(`SELECT output_truncations,lease_admitted FROM memory_jobs WHERE id=$1`, pending.ID).Scan(&truncations, &admitted); err != nil || truncations != 2 || admitted {
		t.Fatal("schema reapplication reset recovery or admission state")
	}
	if err := tx.QueryRow(`INSERT INTO memory_jobs(source_ref,source_version,source_fingerprint,settings_epoch,extractor_version)
		SELECT source_ref,source_version,source_fingerprint,settings_epoch,999 FROM memory_jobs WHERE id=$1
		RETURNING output_truncations,lease_admitted`, pending.ID).Scan(&truncations, &admitted); err != nil || truncations != 0 || admitted {
		t.Fatal("new job inherited legacy recovery or admission state")
	}
}
