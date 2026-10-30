package db

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"
)

const memorySourceCols = `live.source_kind,live.source_id,live.source_version,live.source_fingerprint,
	live.origin_task_id,live.origin_conversation_id,live.agent_key,live.session_id,
	live.source_state,live.source_updated_at,live.content_text`

// MarkMemoryHumanActivity is called only by the authenticated human input path,
// never by a scheduler, agent tool or automatic conversation constructor.
func (d *DB) MarkMemoryHumanActivity(ctx context.Context, id int64) error {
	res, err := d.ExecContext(ctx, `UPDATE conversation_activities SET memory_input_origin='human'
		WHERE id=$1 AND kind='user' AND NOT is_error`, id)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrMemoryInput
	}
	return nil
}

func scanMemorySource(row memoryScanner) (MemorySource, error) {
	var out MemorySource
	err := row.Scan(&out.Kind, &out.ID, &out.Version, &out.Fingerprint, &out.TaskID, &out.ConversationID,
		&out.AgentKey, &out.SessionID, &out.State, &out.UpdatedAt, &out.Text)
	if out.Kind == "conversation_activity" {
		// Attachment cards are business metadata, not reusable human text. Never
		// send their paths/identifiers merely because Detail stores a JSON wrapper.
		var wrapped map[string]json.RawMessage
		decodeErr := json.Unmarshal([]byte(out.Text), &wrapped)
		if decodeErr == nil && wrapped["attachments"] != nil {
			var text string
			if json.Unmarshal(wrapped["text"], &text) == nil {
				out.Text = text
			} else {
				out.Text = ""
			}
		} else if decodeErr != nil && strings.HasPrefix(strings.TrimSpace(out.Text), `{"attachments":`) {
			// The source view's cap may truncate a large attachment wrapper. A
			// failed parse must not turn paths/card metadata into memory input.
			out.Text = ""
		}
	}
	if len(out.Text) > 16000 {
		out.Text = out.Text[:16000]
		for !utf8.ValidString(out.Text) {
			out.Text = out.Text[:len(out.Text)-1]
		}
	}
	return out, err
}

func getMemorySource(ctx context.Context, q memoryQueryer, kind, id string) (*MemorySource, error) {
	source, err := scanMemorySource(q.QueryRowContext(ctx, `SELECT `+memorySourceCols+`
		FROM memory_live_sources live WHERE live.source_kind=$1 AND live.source_id=$2`, kind, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return &source, err
}

func (d *DB) GetMemorySource(ctx context.Context, kind, id string) (*MemorySource, error) {
	source, err := getMemorySource(ctx, d, kind, id)
	if err != nil || source == nil {
		return source, err
	}
	settings, err := d.GetMemorySettings(ctx)
	if err == nil {
		source.SettingsEpoch = settings.Epoch
	}
	return source, err
}

func (d *DB) FindMemorySources(ctx context.Context, limit int) ([]MemorySource, error) {
	return d.findMemorySources(ctx, 0, limit, false)
}

func (d *DB) FindMemoryHistorySources(ctx context.Context, taskID int64, limit int) ([]MemorySource, error) {
	if taskID < 0 {
		return nil, ErrMemoryInput
	}
	return d.findMemorySources(ctx, taskID, limit, true)
}

func (d *DB) findMemorySources(ctx context.Context, taskID int64, limit int, historical bool) ([]MemorySource, error) {
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
	rows, err := d.QueryContext(ctx, `SELECT `+memorySourceCols+` FROM memory_live_sources live
		LEFT JOIN memory_sources source ON source.source_kind=live.source_kind AND source.source_id=live.source_id
		WHERE ($1 OR live.source_updated_at>$2) AND ($3=0 OR live.origin_task_id=$3)
		AND source.suppressed_at IS NULL
		AND NOT EXISTS(SELECT 1 FROM memory_entries entry WHERE entry.source_ref=source.id
		  AND entry.source_fingerprint=live.source_fingerprint AND entry.state IN ('active','disabled'))
		AND NOT EXISTS(SELECT 1 FROM memory_jobs job WHERE job.source_ref=source.id
		  AND job.source_fingerprint=live.source_fingerprint AND job.settings_epoch=$4 AND job.extractor_version=1)
		ORDER BY live.source_updated_at,live.source_kind,live.source_id LIMIT $5`, historical, settings.LearnAfter, taskID, settings.Epoch, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []MemorySource{}
	for rows.Next() {
		source, err := scanMemorySource(rows)
		if err != nil {
			return nil, err
		}
		source.SettingsEpoch, source.Historical = settings.Epoch, historical
		out = append(out, source)
	}
	return out, rows.Err()
}

func sameMemorySource(a, b MemorySource) bool {
	return a.Kind == b.Kind && a.ID == b.ID && a.Version == b.Version && a.Fingerprint == b.Fingerprint &&
		a.TaskID == b.TaskID && a.ConversationID == b.ConversationID && a.State == b.State
}

func (d *DB) QueueMemoryExtraction(ctx context.Context, source MemorySource) (bool, error) {
	if source.Kind != "node" && source.Kind != "conversation_activity" {
		return false, ErrMemoryInput
	}
	tx, err := d.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	settings, err := memorySettings(ctx, tx, "FOR UPDATE")
	if err != nil {
		return false, err
	}
	if !settings.Enabled || !settings.AutoExtract {
		return false, nil
	}
	if source.SettingsEpoch != settings.Epoch {
		return false, ErrMemoryStale
	}
	current, err := getMemorySource(ctx, tx, source.Kind, source.ID)
	if err != nil {
		return false, err
	}
	if current == nil || !sameMemorySource(source, *current) {
		return false, ErrMemoryStale
	}
	if !source.Historical && !current.UpdatedAt.After(settings.LearnAfter) {
		return false, nil
	}
	var pending int
	if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM memory_jobs WHERE state IN ('queued','running') OR (state='failed' AND attempts<3)`).Scan(&pending); err != nil {
		return false, err
	}
	if pending >= 200 {
		return false, nil
	}
	var refID int64
	var suppressed sql.NullTime
	err = tx.QueryRowContext(ctx, `INSERT INTO memory_sources(source_kind,source_id,source_version,source_fingerprint,
		origin_task_id,origin_conversation_id,agent_key,session_id,source_state,source_updated_at,valid)
		VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,true)
		ON CONFLICT(source_kind,source_id) DO UPDATE SET source_version=EXCLUDED.source_version,
		source_fingerprint=EXCLUDED.source_fingerprint,origin_task_id=EXCLUDED.origin_task_id,
		origin_conversation_id=EXCLUDED.origin_conversation_id,agent_key=EXCLUDED.agent_key,
		session_id=EXCLUDED.session_id,source_state=EXCLUDED.source_state,
		source_updated_at=EXCLUDED.source_updated_at,valid=true,updated_at=now()
		RETURNING id,suppressed_at`, current.Kind, current.ID, current.Version, current.Fingerprint,
		current.TaskID, current.ConversationID, current.AgentKey, current.SessionID, current.State, current.UpdatedAt).Scan(&refID, &suppressed)
	if err != nil {
		return false, err
	}
	if suppressed.Valid {
		return false, nil
	}
	res, err := tx.ExecContext(ctx, `INSERT INTO memory_jobs(source_ref,source_version,source_fingerprint,settings_epoch)
		VALUES($1,$2,$3,$4) ON CONFLICT DO NOTHING`, refID, current.Version, current.Fingerprint, settings.Epoch)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	return n > 0, tx.Commit()
}

func (d *DB) ClaimMemoryJob(ctx context.Context, leaseDuration time.Duration) (*MemoryJob, error) {
	if leaseDuration < 30*time.Second {
		leaseDuration = 3 * time.Minute
	}
	if leaseDuration > 15*time.Minute {
		leaseDuration = 15 * time.Minute
	}
	tx, err := d.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	// The single settings-row lock serializes quota decisions between processes.
	settings, err := memorySettings(ctx, tx, "FOR UPDATE")
	if err != nil {
		return nil, err
	}
	if !settings.Enabled || !settings.AutoExtract {
		return nil, nil
	}
	var started, attempts int
	if err := tx.QueryRowContext(ctx, `SELECT count(*) FILTER (WHERE action='extract_start'),
		count(*) FILTER (WHERE action='extract_admitted') FROM memory_audit
		WHERE action IN ('extract_start','extract_admitted') AND created_at>now()-interval '1 hour'`).Scan(&started, &attempts); err != nil {
		return nil, err
	}
	if started >= settings.MaxJobsPerHour || attempts >= settings.MaxJobsPerHour {
		return nil, nil
	}
	var job MemoryJob
	var kind, id string
	err = tx.QueryRowContext(ctx, `SELECT job.id,source.source_kind,source.source_id,job.source_version,
		job.source_fingerprint,job.settings_epoch,job.attempts,job.created_at
		FROM memory_jobs job JOIN memory_sources source ON source.id=job.source_ref
		WHERE job.settings_epoch=$1 AND job.attempts<3 AND source.suppressed_at IS NULL AND source.valid
		AND ((job.state IN ('queued','failed') AND job.retry_at<=now()) OR (job.state='running' AND job.lease_until<now()))
		ORDER BY job.retry_at,job.id LIMIT 1 FOR UPDATE OF job SKIP LOCKED`, settings.Epoch).Scan(&job.ID, &kind, &id,
		&job.Source.Version, &job.Source.Fingerprint, &job.SettingsEpoch, &job.Attempts, &job.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	current, err := getMemorySource(ctx, tx, kind, id)
	if err != nil {
		return nil, err
	}
	if current == nil || current.Version != job.Source.Version || current.Fingerprint != job.Source.Fingerprint {
		if _, err := tx.ExecContext(ctx, `UPDATE memory_jobs SET state='skipped',lease_token='',lease_until=NULL,updated_at=now() WHERE id=$1`, job.ID); err != nil {
			return nil, err
		}
		return nil, tx.Commit()
	}
	job.Source = *current
	job.Source.SettingsEpoch = settings.Epoch
	var token [16]byte
	if _, err := rand.Read(token[:]); err != nil {
		return nil, err
	}
	job.LeaseToken = hex.EncodeToString(token[:])
	leaseUntil := time.Now().UTC().Add(leaseDuration)
	job.LeaseUntil, job.State, job.Attempts = &leaseUntil, "running", job.Attempts+1
	if _, err := tx.ExecContext(ctx, `UPDATE memory_jobs SET state='running',attempts=attempts+1,
		lease_token=$2,lease_until=$3,updated_at=now() WHERE id=$1`, job.ID, job.LeaseToken, leaseUntil); err != nil {
		return nil, err
	}
	if err := memoryAuditTx(ctx, tx, 0, 0, job.ID, MemoryAdmission{TaskID: current.TaskID, ConversationID: current.ConversationID,
		AgentKey: current.AgentKey, SessionID: current.SessionID}, "extract_start"); err != nil {
		return nil, err
	}
	return &job, tx.Commit()
}

func skipMemoryJob(ctx context.Context, tx *sql.Tx, job MemoryJob) error {
	_, err := tx.ExecContext(ctx, `UPDATE memory_jobs SET state='skipped',lease_token='',lease_until=NULL,updated_at=now()
		WHERE id=$1 AND state='running' AND lease_token=$2`, job.ID, job.LeaseToken)
	return err
}

// ValidateMemoryExtractionJob is the extraction-side final admission check.
// Call after queueing, immediately before each real HTTP attempt (including
// retries/failover), not just before creating the logical model request.
func (d *DB) ValidateMemoryExtractionJob(ctx context.Context, job MemoryJob) error {
	if job.ID < 1 || job.LeaseToken == "" {
		return ErrMemoryInput
	}
	tx, err := d.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	// Serialize the actual-attempt count and ledger insertion; the short lock
	// ends before HTTP and is also used by ClaimMemoryJob's preflight quota.
	settings, err := memorySettings(ctx, tx, "FOR UPDATE")
	if err != nil {
		return err
	}
	if !settings.Enabled || !settings.AutoExtract || settings.Epoch != job.SettingsEpoch {
		return ErrMemoryStale
	}
	var attempts int
	if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM memory_audit
		WHERE action='extract_admitted' AND created_at>now()-interval '1 hour'`).Scan(&attempts); err != nil {
		return err
	}
	if attempts >= settings.MaxJobsPerHour {
		return ErrMemoryBudget
	}
	var refID int64
	err = tx.QueryRowContext(ctx, `SELECT source_ref FROM memory_jobs WHERE id=$1`, job.ID).Scan(&refID)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrMemoryStale
	}
	if err != nil {
		return err
	}
	var kind, id string
	var suppressed sql.NullTime
	var valid bool
	err = tx.QueryRowContext(ctx, `SELECT source_kind,source_id,suppressed_at,valid FROM memory_sources WHERE id=$1 FOR SHARE`, refID).Scan(&kind, &id, &suppressed, &valid)
	if err != nil {
		return err
	}
	if !valid || suppressed.Valid {
		return ErrMemoryStale
	}
	var version, epoch int64
	var fingerprint, state, token string
	var leaseUntil sql.NullTime
	err = tx.QueryRowContext(ctx, `SELECT source_version,source_fingerprint,settings_epoch,state,lease_token,lease_until
		FROM memory_jobs WHERE id=$1 FOR SHARE`, job.ID).Scan(&version, &fingerprint, &epoch, &state, &token, &leaseUntil)
	if err != nil {
		return err
	}
	if state != "running" || token != job.LeaseToken || epoch != settings.Epoch ||
		!leaseUntil.Valid || !leaseUntil.Time.After(time.Now()) {
		return ErrMemoryStale
	}
	current, err := getMemorySource(ctx, tx, kind, id)
	if err != nil {
		return err
	}
	if current == nil || current.Version != version || current.Fingerprint != fingerprint || !sameMemorySource(job.Source, *current) {
		return ErrMemoryStale
	}
	if err := memoryAuditTx(ctx, tx, 0, 0, job.ID, MemoryAdmission{TaskID: current.TaskID, ConversationID: current.ConversationID,
		AgentKey: current.AgentKey, SessionID: current.SessionID}, "extract_admitted"); err != nil {
		return err
	}
	return tx.Commit()
}

func (d *DB) CompleteMemoryJob(ctx context.Context, job MemoryJob, inputs []MemoryInput) ([]MemoryEntry, error) {
	if len(inputs) > 10 || job.ID < 1 || job.LeaseToken == "" {
		return nil, ErrMemoryInput
	}
	prepared := make([]MemoryInput, 0, len(inputs))
	for _, input := range inputs {
		in, err := validateMemoryInput(input, true)
		if err != nil {
			return nil, err
		}
		prepared = append(prepared, in)
	}
	tx, err := d.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	settings, err := memorySettings(ctx, tx, "FOR SHARE")
	if err != nil {
		return nil, err
	}
	stale := func() ([]MemoryEntry, error) {
		if err := skipMemoryJob(ctx, tx, job); err != nil {
			return nil, err
		}
		if err := tx.Commit(); err != nil {
			return nil, err
		}
		return nil, ErrMemoryStale
	}
	if !settings.Enabled || !settings.AutoExtract || settings.Epoch != job.SettingsEpoch {
		return stale()
	}
	var refID int64
	err = tx.QueryRowContext(ctx, `SELECT source_ref FROM memory_jobs WHERE id=$1 AND state='running' AND lease_token=$2`, job.ID, job.LeaseToken).Scan(&refID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrMemoryStale
	}
	if err != nil {
		return nil, err
	}
	// Source before entry/job, matching source invalidation triggers and CRUD.
	var kind, id string
	var suppressed sql.NullTime
	var valid bool
	err = tx.QueryRowContext(ctx, `SELECT source_kind,source_id,suppressed_at,valid FROM memory_sources WHERE id=$1 FOR UPDATE`, refID).Scan(&kind, &id, &suppressed, &valid)
	if err != nil {
		return nil, err
	}
	var version, epoch int64
	var fingerprint, state, token string
	var leaseUntil sql.NullTime
	err = tx.QueryRowContext(ctx, `SELECT source_version,source_fingerprint,settings_epoch,state,lease_token,lease_until
		FROM memory_jobs WHERE id=$1 FOR UPDATE`, job.ID).Scan(&version, &fingerprint, &epoch, &state, &token, &leaseUntil)
	if err != nil {
		return nil, err
	}
	if !valid || suppressed.Valid || state != "running" || token != job.LeaseToken || epoch != settings.Epoch ||
		!leaseUntil.Valid || !leaseUntil.Time.After(time.Now()) {
		return stale()
	}
	current, err := getMemorySource(ctx, tx, kind, id)
	if err != nil {
		return nil, err
	}
	if current == nil || current.Version != version || current.Fingerprint != fingerprint || !sameMemorySource(job.Source, *current) {
		return stale()
	}
	if len(prepared) > settings.MaxEntries {
		prepared = prepared[:settings.MaxEntries]
	}
	var sourceProjection struct {
		Target string `json:"target"`
	}
	_ = json.Unmarshal([]byte(current.Text), &sourceProjection)
	out := []MemoryEntry{}
	expires := time.Now().UTC().Add(time.Duration(settings.RetentionDays) * 24 * time.Hour)
	for _, in := range prepared {
		if (in.Kind == "target" && sourceProjection.Target == "") ||
			(in.Target != "" && in.Target != sourceProjection.Target) {
			return nil, ErrMemoryInput
		}
		if in.Kind == "target" {
			in.Target = sourceProjection.Target
		}
		// The extractor does not choose provenance or change task permissions.
		in.OriginTaskID, in.OriginConversationID = current.TaskID, current.ConversationID
		in.AgentKey, in.SessionID = current.AgentKey, current.SessionID
		if in.ExpiresAt == nil || in.ExpiresAt.After(expires) {
			in.ExpiresAt = &expires
		}
		tags, _ := json.Marshal(in.Tags)
		var entryID int64
		err := tx.QueryRowContext(ctx, `INSERT INTO memory_entries(title,content,content_hash,kind,applicability,target,tags,confidence,
			origin_task_id,origin_conversation_id,agent_key,session_id,source_ref,source_version,source_fingerprint,automatic,expires_at)
			VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,true,$16)
			ON CONFLICT DO NOTHING RETURNING id`, in.Title, in.Content, memoryContentHash(in.Content), in.Kind, in.Applicability,
			in.Target, string(tags), in.Confidence, in.OriginTaskID, in.OriginConversationID, in.AgentKey, in.SessionID,
			refID, version, fingerprint, in.ExpiresAt).Scan(&entryID)
		if errors.Is(err, sql.ErrNoRows) {
			continue
		}
		if err != nil {
			return nil, err
		}
		entry, err := getMemoryEntry(ctx, tx, entryID, "")
		if err != nil {
			return nil, err
		}
		out = append(out, *entry)
		if err := memoryAuditTx(ctx, tx, entryID, 1, job.ID, MemoryAdmission{TaskID: current.TaskID, ConversationID: current.ConversationID,
			AgentKey: current.AgentKey, SessionID: current.SessionID}, "created"); err != nil {
			return nil, err
		}
	}
	if _, err := tx.ExecContext(ctx, `UPDATE memory_jobs SET state='done',lease_token='',lease_until=NULL,updated_at=now() WHERE id=$1`, job.ID); err != nil {
		return nil, err
	}
	if err := memoryAuditTx(ctx, tx, 0, 0, job.ID, MemoryAdmission{TaskID: current.TaskID, ConversationID: current.ConversationID,
		AgentKey: current.AgentKey, SessionID: current.SessionID}, "extract_complete"); err != nil {
		return nil, err
	}
	return out, tx.Commit()
}

func (d *DB) FailMemoryJob(ctx context.Context, job MemoryJob, retryAfter time.Duration) error {
	if job.ID < 1 || job.LeaseToken == "" {
		return ErrMemoryInput
	}
	if retryAfter < 30*time.Second {
		retryAfter = 5 * time.Minute
	}
	if retryAfter > time.Hour {
		retryAfter = time.Hour
	}
	tx, err := d.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	res, err := tx.ExecContext(ctx, `UPDATE memory_jobs SET state=CASE WHEN attempts>=3 THEN 'skipped' ELSE 'failed' END,
		retry_at=$3,lease_token='',lease_until=NULL,updated_at=now()
		WHERE id=$1 AND state='running' AND lease_token=$2`, job.ID, job.LeaseToken, time.Now().UTC().Add(retryAfter))
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrMemoryStale
	}
	if err := memoryAuditTx(ctx, tx, 0, 0, job.ID, MemoryAdmission{TaskID: job.Source.TaskID, ConversationID: job.Source.ConversationID,
		AgentKey: job.Source.AgentKey, SessionID: job.Source.SessionID}, "extract_failed"); err != nil {
		return err
	}
	return tx.Commit()
}

// MemoryJobCounts returns body-free operational metadata for the management UI.
func (d *DB) MemoryJobCounts(ctx context.Context) (map[string]int64, error) {
	rows, err := d.QueryContext(ctx, `SELECT state,count(*) FROM memory_jobs GROUP BY state`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]int64{}
	for rows.Next() {
		var state string
		var count int64
		if err := rows.Scan(&state, &count); err != nil {
			return nil, err
		}
		out[state] = count
	}
	return out, rows.Err()
}

func (s MemorySource) String() string {
	// Never accidentally format source text or fingerprints into operational logs.
	return fmt.Sprintf("memory source %s/%s", s.Kind, s.ID)
}

// PruneMemory removes only bounded batches of body-free telemetry and finished
// jobs. Source suppression tombstones survive pruning so deleted entries cannot
// be relearned from old task history. Entry validity is enforced by expires_at.
func (d *DB) PruneMemory(ctx context.Context) error {
	settings, err := d.GetMemorySettings(ctx)
	if err != nil {
		return err
	}
	days := min(settings.RetentionDays, 90)
	tx, err := d.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `DELETE FROM memory_audit WHERE id IN (
		SELECT id FROM memory_audit WHERE created_at<now()-($1*interval '1 day')
		ORDER BY created_at,id LIMIT 1000 FOR UPDATE SKIP LOCKED)`, days); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM memory_jobs WHERE id IN (
		SELECT id FROM memory_jobs WHERE state IN ('done','skipped') AND updated_at<now()-($1*interval '1 day')
		ORDER BY updated_at,id LIMIT 1000 FOR UPDATE SKIP LOCKED)`, days); err != nil {
		return err
	}
	return tx.Commit()
}
