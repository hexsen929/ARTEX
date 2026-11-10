package db

import (
	"errors"
	"fmt"
	"testing"
	"time"
)

// Fail through the real lease transition without admitting an HTTP request.
func memoryUnadmittedFailure(t *testing.T, d *DB, task *Task, attempts int) MemorySource {
	t.Helper()
	source := memoryFixtureSource(t, d, task)
	source.Historical = false
	job := memoryQueueClaim(t, d, source)
	for i := 0; i < attempts; i++ {
		if err := d.FailMemoryJobWithResult(t.Context(), job, time.Minute, "timeout"); err != nil {
			t.Fatal(err)
		}
		if i+1 < attempts {
			if _, err := d.Exec(`UPDATE memory_jobs SET retry_at=now()-interval '1 second' WHERE id=$1`, job.ID); err != nil {
				t.Fatal(err)
			}
			next, err := d.ClaimMemoryJob(t.Context(), time.Minute)
			if err != nil || next == nil || next.ID != job.ID {
				t.Fatalf("retry claim: %+v %v", next, err)
			}
			job = *next
		}
	}
	var admitted int
	if err := d.QueryRow(`SELECT count(*) FROM memory_audit WHERE job_id=$1 AND action='extract_admitted'`, job.ID).Scan(&admitted); err != nil || admitted != 0 {
		t.Fatal("fixture sent HTTP", admitted, err)
	}
	return source
}

func TestMemoryDiscoveryRejectedCandidatesDoNotBlockNewStages(t *testing.T) {
	for _, attempts := range []int{1, 3} {
		t.Run(fmt.Sprintf("attempts-%d", attempts), func(t *testing.T) {
			d := memoryFixtureDB(t)
			memoryEnableIncrementalHistory(t, d)
			task := memoryFixtureTask(t, d)
			old := make([]MemorySource, 4)
			for i := range old {
				old[i] = memoryUnadmittedFailure(t, d, task, attempts)
			}
			fresh := memoryFixtureSource(t, d, task)
			if sources, err := d.FindMemorySources(t.Context(), 4); err != nil || len(sources) != 0 {
				t.Fatalf("first rejection pass: %+v %v", sources, err)
			}
			sources, err := d.FindMemorySources(t.Context(), 4)
			if err != nil || len(sources) != 1 || sources[0].ID != fresh.ID {
				t.Fatalf("old failures hid newer stage: %+v %v", sources, err)
			}
			memoryRejectBuild(t, d, mustMemoryNodeID(t, old[0].ID))
			if _, err := d.FindMemorySources(t.Context(), 4); err != nil {
				t.Fatalf("warm rejected source rebuilt: %v", err)
			}
		})
	}
}

func TestMemoryDiscoveryRejectedCacheRechecksChangedSourceAndSettings(t *testing.T) {
	for _, change := range []string{"source", "settings"} {
		t.Run(change, func(t *testing.T) {
			d := memoryFixtureDB(t)
			memoryEnableIncrementalHistory(t, d)
			task := memoryFixtureTask(t, d)
			source := memoryUnadmittedFailure(t, d, task, 3)
			if found, err := d.FindMemorySources(t.Context(), 4); err != nil || len(found) != 0 {
				t.Fatal(found, err)
			}
			if change == "source" {
				fact := memorySourceFactID(t, source)
				if _, err := d.Exec(`UPDATE exploration_nodes SET payload=jsonb_set(payload,'{summary}','"changed fixture observation"'::jsonb),content_version=content_version+1 WHERE id=$1`, fact); err != nil {
					t.Fatal(err)
				}
			} else {
				settings, err := d.GetMemorySettings(t.Context())
				if err != nil {
					t.Fatal(err)
				}
				if _, err := d.UpdateMemorySettings(t.Context(), settings, settings.Epoch); err != nil {
					t.Fatal(err)
				}
			}
			found, err := d.FindMemorySources(t.Context(), 4)
			if err != nil || len(found) != 1 || found[0].ID != source.ID {
				t.Fatalf("changed eligibility remained cached: %+v %v", found, err)
			}
		})
	}
}

func TestMemoryHistoryProgressPastCompletedAndEmptyPrefix(t *testing.T) {
	for _, prefix := range []string{"completed", "empty"} {
		t.Run(prefix, func(t *testing.T) {
			d := memoryFixtureDB(t)
			task := memoryFixtureTask(t, d)
			for range 50 {
				if prefix == "empty" {
					memoryEmptyEndedStage(t, d, task)
					continue
				}
				source := memoryFixtureSource(t, d, task)
				job := memoryQueueClaim(t, d, source)
				if _, err := memoryComplete(t, d, job, nil); err != nil {
					t.Fatal(err)
				}
			}
			fresh := memoryFixtureSource(t, d, task)
			first, err := d.QueueMemoryHistoryPage(t.Context(), task.ID, "")
			if err != nil || first.Scanned != 50 || first.Queued != 0 || !first.HasMore || first.NextCursor == "" {
				t.Fatalf("first page: %+v %v", first, err)
			}
			second, err := d.QueueMemoryHistoryPage(t.Context(), task.ID, first.NextCursor)
			if err != nil || second.Scanned != 1 || second.Queued != 1 || second.HasMore || second.NextCursor != "" {
				t.Fatalf("history stalled after prefix: %+v %v", second, err)
			}
			var jobs int
			if err := d.QueryRow(`SELECT count(*) FROM memory_jobs j JOIN memory_sources s ON s.id=j.source_ref WHERE s.source_kind=$1 AND s.source_id=$2 AND j.state='queued'`, fresh.Kind, fresh.ID).Scan(&jobs); err != nil || jobs != 1 {
				t.Fatal("later stage missing", jobs, err)
			}
			if replay, err := d.QueueMemoryHistoryPage(t.Context(), task.ID, first.NextCursor); err != nil || replay.Queued != 0 {
				t.Fatal("cursor replay duplicated queue", replay, err)
			}
			other := memoryFixtureTask(t, d)
			if _, err := d.QueueMemoryHistoryPage(t.Context(), other.ID, first.NextCursor); !errors.Is(err, ErrMemoryInput) {
				t.Fatal("foreign scope cursor accepted", err)
			}
			settings, _ := d.GetMemorySettings(t.Context())
			if _, err := d.UpdateMemorySettings(t.Context(), settings, settings.Epoch); err != nil {
				t.Fatal(err)
			}
			if _, err := d.QueueMemoryHistoryPage(t.Context(), task.ID, first.NextCursor); !errors.Is(err, ErrMemoryConflict) {
				t.Fatal("stale settings cursor accepted", err)
			}
		})
	}
}

func TestMemoryHistoryProgressRetainsSourceWhenQueueFull(t *testing.T) {
	d := memoryFixtureDB(t)
	busy := memoryFixtureTask(t, d)
	source := memoryFixtureSource(t, d, busy)
	if ok, err := d.QueueMemoryExtraction(t.Context(), source); err != nil || !ok {
		t.Fatal(ok, err)
	}
	// Fill capacity using separate extractor fixture identities; no HTTP occurs.
	if _, err := d.Exec(`INSERT INTO memory_jobs(source_ref,source_version,source_fingerprint,settings_epoch,extractor_version)
	 SELECT j.source_ref,j.source_version,j.source_fingerprint,j.settings_epoch,n FROM memory_jobs j
	 JOIN memory_sources s ON s.id=j.source_ref CROSS JOIN generate_series(2,200) n WHERE s.source_kind=$1 AND s.source_id=$2`, source.Kind, source.ID); err != nil {
		t.Fatal(err)
	}
	task := memoryFixtureTask(t, d)
	fresh := memoryFixtureSource(t, d, task)
	full, err := d.QueueMemoryHistoryPage(t.Context(), task.ID, "")
	if err != nil || !full.QueueFull || !full.HasMore || full.Scanned != 0 || full.Queued != 0 || full.NextCursor == "" {
		t.Fatalf("queue capacity lost progress: %+v %v", full, err)
	}
	if _, err := d.Exec(`UPDATE memory_jobs SET state='skipped' WHERE source_ref IN (SELECT id FROM memory_sources WHERE origin_task_id=$1)`, busy.ID); err != nil {
		t.Fatal(err)
	}
	retry, err := d.QueueMemoryHistoryPage(t.Context(), task.ID, full.NextCursor)
	if err != nil || retry.Queued != 1 || retry.HasMore {
		t.Fatalf("full-queue retry skipped source %s: %+v %v", fresh.ID, retry, err)
	}
}
