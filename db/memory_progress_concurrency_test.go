package db

import (
	"context"
	"fmt"
	"testing"
	"time"
)

func TestMemoryDiscoveryRejectedExpiredPrefixDoesNotStarveFresh(t *testing.T) {
	d := memoryFixtureDB(t)
	memoryEnableIncrementalHistory(t, d)
	task := memoryFixtureTask(t, d)
	// A prefix larger than one 15-minute scan cycle (4 candidates / 30s).
	for range 125 {
		memoryEmptyEndedStage(t, d, task)
	}
	if _, err := d.Exec(`INSERT INTO memory_discovery_checks(source_kind,source_id,origin_task_id,source_version,native_version,source_state,source_updated_at,checked,checked_at)
	 SELECT source_kind,source_id,origin_task_id,source_version,native_version,source_state,source_updated_at,true,now()-interval '16 minutes'
	 FROM memory_stage_metadata WHERE origin_task_id=$1`, task.ID); err != nil {
		t.Fatal(err)
	}
	fresh := memoryFixtureSource(t, d, task)
	found, err := d.FindMemorySources(t.Context(), 4)
	if err != nil || !memoryContainsSource(found, fresh) {
		t.Fatalf("expired checks starved untouched source: %+v %v", found, err)
	}
}

func memoryWaitForLock(t *testing.T, ctx context.Context, d *DB, condition string, args ...any) {
	t.Helper()
	for {
		var waiting bool
		if err := d.QueryRowContext(ctx, condition, args...).Scan(&waiting); err != nil {
			t.Fatal(err)
		}
		if waiting {
			return
		}
		select {
		case <-ctx.Done():
			t.Fatal("fixture did not reach lock barrier")
		case <-time.After(10 * time.Millisecond):
		}
	}
}

func TestMemoryDiscoveryRejectedSettingsAndSourceUpdateDoNotDeadlock(t *testing.T) {
	d := memoryFixtureDB(t)
	memoryEnableIncrementalHistory(t, d)
	task := memoryFixtureTask(t, d)
	source := memoryUnadmittedFailure(t, d, task, 1)
	if found, err := d.FindMemorySources(t.Context(), 4); err != nil || len(found) != 0 {
		t.Fatal(found, err)
	}
	var jobID int64
	if err := d.QueryRow(`SELECT j.id FROM memory_jobs j JOIN memory_sources s ON s.id=j.source_ref WHERE s.source_kind=$1 AND s.source_id=$2`, source.Kind, source.ID).Scan(&jobID); err != nil {
		t.Fatal(err)
	}
	const gateKey = 832644200
	gate, err := d.BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer gate.Rollback()
	if _, err = gate.Exec(`SELECT pg_advisory_xact_lock($1)`, gateKey); err != nil {
		t.Fatal(err)
	}
	// Stop the settings update after it has locked the job row. With the old
	// jobs -> checks order, a native writer can hold checks and wait on this job.
	_, err = d.Exec(fmt.Sprintf(`CREATE FUNCTION memory_progress_settings_gate() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN
	 IF NEW.id=%d AND NEW.result='settings_changed' THEN PERFORM pg_advisory_xact_lock(%d); END IF;
	 RETURN NEW; END $$;
	 CREATE TRIGGER memory_progress_settings_gate BEFORE UPDATE ON memory_jobs FOR EACH ROW EXECUTE FUNCTION memory_progress_settings_gate()`, jobID, gateKey))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		d.Exec(`DROP TRIGGER IF EXISTS memory_progress_settings_gate ON memory_jobs; DROP FUNCTION IF EXISTS memory_progress_settings_gate()`)
	})
	ctx, cancel := context.WithTimeout(t.Context(), 8*time.Second)
	defer cancel()
	settings, _ := d.GetMemorySettings(ctx)
	updated := make(chan error, 1)
	go func() { _, err := d.UpdateMemorySettings(ctx, settings, settings.Epoch); updated <- err }()
	memoryWaitForLock(t, ctx, d, `SELECT EXISTS(SELECT 1 FROM pg_locks WHERE locktype='advisory' AND objid=$1 AND NOT granted)`, gateKey)
	conn, err := d.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	var pid int
	if err = conn.QueryRowContext(ctx, `SELECT pg_backend_pid()`).Scan(&pid); err != nil {
		t.Fatal(err)
	}
	changed := make(chan error, 1)
	factID := memorySourceFactID(t, source)
	go func() {
		_, err := conn.ExecContext(ctx, `UPDATE exploration_nodes SET payload=jsonb_set(payload,'{summary}','"concurrently revised fixture"'::jsonb),content_version=content_version+1 WHERE id=$1`, factID)
		changed <- err
	}()
	memoryWaitForLock(t, ctx, d, `SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE pid=$1 AND wait_event_type='Lock')`, pid)
	if err = gate.Commit(); err != nil {
		t.Fatal(err)
	}
	if err = <-updated; err != nil {
		t.Fatalf("settings update failed during native edit: %v", err)
	}
	if err = <-changed; err != nil {
		t.Fatalf("native edit failed during settings update: %v", err)
	}
	found, err := d.FindMemorySources(ctx, 4)
	if err != nil || len(found) != 1 || found[0].ID != source.ID {
		t.Fatalf("new content/settings hidden after race: %+v %v", found, err)
	}
}

func TestMemoryDiscoveryRejectedLateAcknowledgementCannotSurviveSettingsChange(t *testing.T) {
	d := memoryFixtureDB(t)
	memoryEnableIncrementalHistory(t, d)
	task := memoryFixtureTask(t, d)
	intent := memoryEmptyEndedStage(t, d, task)
	settings, _ := d.GetMemorySettings(t.Context())
	update, err := d.BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer update.Rollback()
	if _, err = update.Exec(`UPDATE memory_settings SET epoch=epoch+1; DELETE FROM memory_discovery_checks`); err != nil {
		t.Fatal(err)
	}
	var settingsPID int
	if err = update.QueryRow(`SELECT pg_backend_pid()`).Scan(&settingsPID); err != nil {
		t.Fatal(err)
	}
	var token int64
	if err = d.QueryRow(`INSERT INTO memory_discovery_checks(source_kind,source_id,origin_task_id,source_version,native_version,source_state,source_updated_at)
	 SELECT source_kind,source_id,origin_task_id,source_version,native_version,source_state,source_updated_at FROM memory_stage_metadata
	 WHERE source_kind='intent_stage' AND source_id=$1 RETURNING id`, fmt.Sprint(intent)).Scan(&token); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	acked := make(chan error, 1)
	go func() { acked <- d.finishMemoryDiscoveryCheck(ctx, token, settings.Epoch, true) }()
	// The acknowledgement must wait for the settings transaction, rather than
	// accepting its old statement snapshot and leaving a stale checked marker.
	memoryWaitForLock(t, ctx, d, `SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE $1=ANY(pg_blocking_pids(pid)) AND wait_event_type='Lock')`, settingsPID)
	if err = update.Commit(); err != nil {
		t.Fatal(err)
	}
	if err = <-acked; err != nil {
		t.Fatal(err)
	}
	var retained bool
	if err = d.QueryRow(`SELECT EXISTS(SELECT 1 FROM memory_discovery_checks WHERE id=$1 AND checked)`, token).Scan(&retained); err != nil || retained {
		t.Fatal("stale rejection survived settings commit", retained, err)
	}
}
