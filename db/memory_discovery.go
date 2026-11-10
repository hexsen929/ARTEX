package db

import (
	"context"
	"errors"
	"time"
)

const memoryDiscoveryTimeout = 5 * time.Second

// Candidate discovery must not expand memory_live_sources: its fingerprints and
// bounded text are expensive to construct. Select cheap native identities first.
const memoryDiscoveryFilterSQL = `SELECT live.source_kind,live.source_id,live.origin_task_id,
	live.source_version,live.native_version,live.source_state,live.source_updated_at
	FROM memory_stage_metadata live
	LEFT JOIN memory_sources source ON source.source_kind=live.source_kind AND source.source_id=live.source_id
	WHERE ($1 OR live.source_updated_at>$2) AND ($3=0 OR live.origin_task_id=$3)
	AND source.suppressed_at IS NULL
	AND ($1 OR source.last_completed_version IS DISTINCT FROM live.source_version)
	AND ($1 OR source.last_attempted_version IS DISTINCT FROM live.source_version)
	AND ($1 OR NOT EXISTS(SELECT 1 FROM memory_discovery_checks checked
	  WHERE checked.source_kind=live.source_kind AND checked.source_id=live.source_id
	  AND checked.source_version=live.source_version AND checked.native_version=live.native_version
	  AND checked.source_state=live.source_state AND checked.source_updated_at=live.source_updated_at AND checked.checked
	  AND checked.checked_at>now()-interval '15 minutes'))
	AND NOT EXISTS(SELECT 1 FROM memory_jobs job WHERE job.source_ref=source.id
	  AND job.source_version=live.source_version AND job.settings_epoch=$4
	  AND job.extractor_version=1 AND job.state IN ('queued','running'))`

const memoryDiscoveryOrderSQL = ` ORDER BY live.source_updated_at,live.source_kind,live.source_id LIMIT $5`
const memoryDiscoverySQL = memoryDiscoveryFilterSQL + memoryDiscoveryOrderSQL

type memoryCandidate struct {
	kind, id, state                string
	taskID, version, nativeVersion int64
	updated                        time.Time
}

// MemoryDiscoveryAllowed is a cheap preflight, not a reservation. Claim and
// actual-send admission retain their transactional quota checks against races.
func (d *DB) MemoryDiscoveryAllowed(ctx context.Context) (bool, error) {
	var allowed bool
	err := d.QueryRowContext(ctx, `SELECT enabled AND auto_extract
	  AND (SELECT count(*) FROM memory_audit WHERE action='extract_start'
	    AND created_at>now()-interval '1 hour')<max_jobs_per_hour
	  AND (SELECT count(*) FROM memory_audit WHERE action='extract_admitted'
	    AND created_at>now()-interval '1 hour')<max_jobs_per_hour
	  AND (SELECT count(*) FROM memory_jobs WHERE state IN ('queued','running')
	    OR (state='failed' AND attempts<3))<100 FROM memory_settings WHERE singleton`).Scan(&allowed)
	return allowed, err
}

func (d *DB) discoverMemorySources(ctx context.Context, taskID int64, limit int, historical bool) ([]MemorySource, error) {
	ctx, cancel := context.WithTimeout(ctx, memoryDiscoveryTimeout)
	defer cancel()
	maxLimit := 100
	if historical {
		maxLimit = 50
	}
	if limit < 1 || limit > maxLimit {
		limit = maxLimit
	}
	settings, err := d.GetMemorySettings(ctx)
	if err != nil {
		return nil, err
	}
	if !settings.Enabled || !settings.AutoExtract {
		return []MemorySource{}, nil
	}
	rows, err := d.QueryContext(ctx, memoryDiscoverySQL, historical, settings.LearnAfter, taskID, settings.Epoch, limit)
	if err != nil {
		return nil, err
	}
	candidates := []memoryCandidate{}
	for rows.Next() {
		var c memoryCandidate
		if err := rows.Scan(&c.kind, &c.id, &c.taskID, &c.version, &c.nativeVersion, &c.state, &c.updated); err != nil {
			rows.Close()
			return nil, err
		}
		candidates = append(candidates, c)
	}
	err = rows.Err()
	rows.Close() // Never retain a pool connection while looking up each identity.
	if err != nil {
		return nil, err
	}
	out := []MemorySource{}
	for _, c := range candidates {
		source, err := d.prepareMemoryCandidate(ctx, c, settings, historical)
		if err != nil {
			return nil, err
		}
		if source != nil {
			out = append(out, *source)
		}
	}
	return out, nil
}

// Cache a complete eligibility decision, including a built but ineligible
// source. Otherwise old pre-admission failures monopolize the candidate LIMIT.
// The token and existing invalidation triggers prevent a late reader from
// acknowledging newer content; expiry covers writers with an older snapshot.
func (d *DB) prepareMemoryCandidate(ctx context.Context, c memoryCandidate, settings MemorySettings, historical bool) (out *MemorySource, err error) {
	var token int64
	checked := false
	defer func() {
		if token == 0 {
			return
		}
		var cacheErr error
		if err == nil && checked {
			_, cacheErr = d.ExecContext(ctx, `UPDATE memory_discovery_checks SET checked=true,checked_at=now() WHERE id=$1
			  AND EXISTS(SELECT 1 FROM memory_settings WHERE singleton AND epoch=$2)`, token, settings.Epoch)
		} else {
			_, cacheErr = d.ExecContext(ctx, `DELETE FROM memory_discovery_checks WHERE id=$1`, token)
		}
		if err == nil && cacheErr != nil {
			out, err = nil, cacheErr
		}
	}()
	if !historical {
		// This body-free marker is visible to the existing source invalidation
		// triggers. Edits that observe the marker delete it, and tokens prevent
		// stale acknowledgement. A bounded expiry is also necessary: a writer
		// whose statement snapshot predates marker registration may commit late
		// without observing it. Negative caching must never hide learning forever.
		err = d.QueryRowContext(ctx, `INSERT INTO memory_discovery_checks
			  (source_kind,source_id,origin_task_id,source_version,native_version,source_state,source_updated_at)
			  VALUES($1,$2,$3,$4,$5,$6,$7)
			  ON CONFLICT(source_kind,source_id) DO UPDATE SET
			  id=nextval('memory_discovery_checks_id_seq'),origin_task_id=EXCLUDED.origin_task_id,
			  source_version=EXCLUDED.source_version,native_version=EXCLUDED.native_version,
			  source_state=EXCLUDED.source_state,source_updated_at=EXCLUDED.source_updated_at,
			  checked=false,checked_at=now() RETURNING id`, c.kind, c.id, c.taskID, c.version, c.nativeVersion, c.state, c.updated).Scan(&token)
		if err != nil {
			return nil, err
		}
	}
	source, buildErr := getMemorySource(ctx, d, c.kind, c.id)
	if errors.Is(buildErr, ErrMemoryUnavailable) {
		source, buildErr = nil, nil
	}
	if buildErr != nil {
		return nil, buildErr
	}
	if source == nil {
		checked = true
		return nil, nil
	}
	if source.Version != c.version {
		return nil, nil
	}
	var eligible bool
	err = d.QueryRowContext(ctx, `SELECT NOT EXISTS(SELECT 1 FROM memory_sources source
		  WHERE source.source_kind=$1 AND source.source_id=$2 AND
		  (source.suppressed_at IS NOT NULL OR (NOT $5 AND
		    (source.last_completed_version=$3 OR source.last_attempted_version=$3))
		    OR (source.valid AND source.last_completed_fingerprint=$4)))
		  AND NOT EXISTS(SELECT 1 FROM memory_entries entry JOIN memory_sources source ON source.id=entry.source_ref
		    WHERE source.source_kind=$1 AND source.source_id=$2 AND entry.source_fingerprint=$4 AND entry.state IN ('active','disabled'))
		  AND NOT EXISTS(SELECT 1 FROM memory_jobs job JOIN memory_sources source ON source.id=job.source_ref
		    WHERE source.source_kind=$1 AND source.source_id=$2 AND job.source_fingerprint=$4
		    AND job.settings_epoch=$6 AND job.extractor_version=1 AND (NOT $5 OR job.state IN ('queued','running')))`,
		c.kind, c.id, source.Version, source.Fingerprint, historical, settings.Epoch).Scan(&eligible)
	if err != nil {
		return nil, err
	}
	if eligible {
		source.SettingsEpoch, source.Historical = settings.Epoch, historical
		return source, nil
	}
	checked = true
	return nil, nil
}
