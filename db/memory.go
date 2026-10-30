package db

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

type memoryQueryer interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}

const memorySettingsCols = `enabled,auto_extract,global_share,profile_id,max_entries,max_tokens,max_jobs_per_hour,retention_days,learn_after,epoch`

func memorySettings(ctx context.Context, q memoryQueryer, lock string) (MemorySettings, error) {
	var out MemorySettings
	err := q.QueryRowContext(ctx, `SELECT `+memorySettingsCols+` FROM memory_settings WHERE singleton=true `+lock).Scan(
		&out.Enabled, &out.AutoExtract, &out.GlobalShare, &out.ProfileID, &out.MaxEntries, &out.MaxTokens,
		&out.MaxJobsPerHour, &out.RetentionDays, &out.LearnAfter, &out.Epoch)
	if errors.Is(err, sql.ErrNoRows) {
		return out, ErrMemoryUnavailable
	}
	return out, err
}

// GetMemorySettings never writes defaults. The idempotent schema migration
// establishes the singleton and the deployment's incremental-learning boundary.
func (d *DB) GetMemorySettings(ctx context.Context) (MemorySettings, error) {
	return memorySettings(ctx, d, "")
}

func validateMemorySettings(in MemorySettings) error {
	if in.ProfileID < 0 || in.MaxEntries < 1 || in.MaxEntries > 10 || in.MaxTokens < 1024 || in.MaxTokens > 2000 ||
		in.MaxJobsPerHour < 1 || in.MaxJobsPerHour > 60 || in.RetentionDays < 1 || in.RetentionDays > 3650 {
		return ErrMemoryInput
	}
	return nil
}

func (d *DB) UpdateMemorySettings(ctx context.Context, in MemorySettings, expectedEpoch int64) (MemorySettings, error) {
	if err := validateMemorySettings(in); err != nil {
		return MemorySettings{}, err
	}
	tx, err := d.BeginTx(ctx, nil)
	if err != nil {
		return MemorySettings{}, err
	}
	defer tx.Rollback()
	current, err := memorySettings(ctx, tx, "FOR UPDATE")
	if err != nil {
		return current, err
	}
	if current.Epoch != expectedEpoch {
		return current, ErrMemoryConflict
	}
	if in.ProfileID > 0 {
		var exists bool
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM llm_profiles WHERE id=$1)`, in.ProfileID).Scan(&exists); err != nil {
			return current, err
		}
		if !exists {
			return current, ErrMemoryInput
		}
	}
	if _, err := tx.ExecContext(ctx, `UPDATE memory_settings SET enabled=$1,auto_extract=$2,global_share=$3,
		profile_id=$4,max_entries=$5,max_tokens=$6,max_jobs_per_hour=$7,retention_days=$8,epoch=epoch+1 WHERE singleton=true`,
		in.Enabled, in.AutoExtract, in.GlobalShare, in.ProfileID, in.MaxEntries, in.MaxTokens, in.MaxJobsPerHour, in.RetentionDays); err != nil {
		return current, err
	}
	// Jobs admitted under old settings are not silently carried into new model
	// or sharing choices. Fresh incremental discovery can queue them anew.
	if _, err := tx.ExecContext(ctx, `UPDATE memory_jobs SET state='skipped',lease_token='',lease_until=NULL,updated_at=now()
		WHERE state IN ('queued','running','failed')`); err != nil {
		return current, err
	}
	in.Epoch, in.LearnAfter = current.Epoch+1, current.LearnAfter
	return in, tx.Commit()
}

const memoryEntryStateSQL = `CASE WHEN e.state='active' AND e.expires_at IS NOT NULL AND e.expires_at<=now() THEN 'expired' ELSE e.state END`

const memoryEntryCols = `e.id,e.title,e.content,e.kind,e.applicability,e.confidence,` + memoryEntryStateSQL + `,e.target,e.tags,
	e.origin_task_id,e.origin_conversation_id,e.agent_key,e.session_id,
	COALESCE(s.source_kind,''),COALESCE(s.source_id,''),e.source_version,e.source_fingerprint,
	e.automatic,e.version,e.epoch,e.created_at,e.updated_at,e.expires_at,
	(SELECT count(*) FROM memory_audit used WHERE used.entry_id=e.id AND used.action='admitted'),
	(SELECT max(used.created_at) FROM memory_audit used WHERE used.entry_id=e.id AND used.action='admitted')`

type memoryScanner interface{ Scan(...any) error }

func scanMemoryEntry(row memoryScanner) (MemoryEntry, error) {
	var out MemoryEntry
	var tags []byte
	err := row.Scan(&out.ID, &out.Title, &out.Content, &out.Kind, &out.Applicability, &out.Confidence, &out.State, &out.Target, &tags,
		&out.OriginTaskID, &out.OriginConversationID, &out.AgentKey, &out.SessionID,
		&out.SourceKind, &out.SourceID, &out.SourceVersion, &out.SourceFingerprint,
		&out.Automatic, &out.Version, &out.Epoch, &out.CreatedAt, &out.UpdatedAt, &out.ExpiresAt, &out.UseCount, &out.LastUsedAt)
	if err == nil {
		err = json.Unmarshal(tags, &out.Tags)
	}
	if out.Tags == nil {
		out.Tags = []string{}
	}
	return out, err
}

func getMemoryEntry(ctx context.Context, q memoryQueryer, id int64, lock string) (*MemoryEntry, error) {
	entry, err := scanMemoryEntry(q.QueryRowContext(ctx, `SELECT `+memoryEntryCols+`
		FROM memory_entries e LEFT JOIN memory_sources s ON s.id=e.source_ref WHERE e.id=$1 `+lock, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return &entry, err
}

func (d *DB) GetMemoryEntry(ctx context.Context, id int64) (*MemoryEntry, error) {
	return getMemoryEntry(ctx, d, id, "")
}

func memoryFilterSQL(filter MemoryFilter) (string, []any) {
	clauses, args := []string{"e.state<>'deleted'"}, []any{}
	add := func(expr string, value any) {
		args = append(args, value)
		clauses = append(clauses, fmt.Sprintf(expr, len(args)))
	}
	if filter.Query != "" {
		add(`(e.title ILIKE $%[1]d OR e.content ILIKE $%[1]d OR e.applicability ILIKE $%[1]d OR e.target ILIKE $%[1]d)`, memoryLikePattern(filter.Query))
	}
	if filter.State != "" {
		add(`(`+memoryEntryStateSQL+`)=$%d`, filter.State)
	}
	if filter.Kind != "" {
		add(`e.kind=$%d`, filter.Kind)
	}
	if filter.TaskID > 0 {
		add(`e.origin_task_id=$%d`, filter.TaskID)
	}
	if filter.ConversationID > 0 {
		add(`e.origin_conversation_id=$%d`, filter.ConversationID)
	}
	if filter.Automatic != nil {
		add(`e.automatic=$%d`, *filter.Automatic)
	}
	return strings.Join(clauses, " AND "), args
}

func (d *DB) ListMemoryEntries(ctx context.Context, filter MemoryFilter, page, size int) ([]MemoryEntry, int, error) {
	if page < 0 {
		page = 0
	}
	if size < 1 || size > 100 {
		size = 30
	}
	where, args := memoryFilterSQL(filter)
	var total int
	if err := d.QueryRowContext(ctx, `SELECT count(*) FROM memory_entries e WHERE `+where, args...).Scan(&total); err != nil {
		return nil, 0, err
	}
	argN := len(args)
	args = append(args, size, page*size)
	rows, err := d.QueryContext(ctx, `SELECT `+memoryEntryCols+` FROM memory_entries e
		LEFT JOIN memory_sources s ON s.id=e.source_ref WHERE `+where+
		fmt.Sprintf(` ORDER BY e.updated_at DESC,e.id DESC LIMIT $%d OFFSET $%d`, argN+1, argN+2), args...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	out := []MemoryEntry{}
	for rows.Next() {
		entry, err := scanMemoryEntry(rows)
		if err != nil {
			return nil, 0, err
		}
		out = append(out, entry)
	}
	return out, total, rows.Err()
}

// This is defense in depth, not a claim of comprehensive secret detection.
// The learner/API must also apply their source and content policies.
var memorySecretPatterns = []*regexp.Regexp{
	regexp.MustCompile(`(?i)-----BEGIN (?:[A-Z ]*PRIVATE KEY|OPENSSH PRIVATE KEY)-----`),
	regexp.MustCompile(`(?i)(?:authorization\s*:\s*bearer\s+|(?:api[_-]?key|access[_-]?token|client[_-]?secret|password|passwd|cookie)\s*[=:]\s*["']?)[A-Za-z0-9_+/=.%~-]{8,}`),
	regexp.MustCompile(`(?:AKIA|ASIA)[A-Z0-9]{16}`),
	regexp.MustCompile(`(?i)https?://[^\s/@:]+:[^\s/@]+@`),
}

func validateMemoryInput(in MemoryInput, automatic bool) (MemoryInput, error) {
	in.Title = strings.TrimSpace(in.Title)
	if in.Title == "" || strings.TrimSpace(in.Content) == "" || utf8.RuneCountInString(in.Title) > 160 || len(in.Content) > 12000 ||
		len(in.Applicability) > 2000 || len(in.Target) > 512 || len(in.AgentKey) > 128 || len(in.SessionID) > 160 ||
		in.OriginTaskID < 0 || in.OriginConversationID < 0 || len(in.Tags) > 16 {
		return in, ErrMemoryInput
	}
	if in.Kind == "" {
		in.Kind = "lesson"
	}
	if in.Kind != "environment" && in.Kind != "procedure" && in.Kind != "lesson" && in.Kind != "target" {
		return in, ErrMemoryInput
	}
	if in.State == "" {
		in.State = "active"
	}
	if in.State != "active" && in.State != "disabled" {
		return in, ErrMemoryInput
	}
	if automatic {
		in.State = "active"
	}
	if in.Confidence == "" {
		in.Confidence = "unverified"
	}
	if in.Confidence != "observed" && in.Confidence != "unverified" {
		return in, ErrMemoryInput
	}
	values := []string{in.Title, in.Content, in.Applicability, in.Target, in.AgentKey, in.SessionID}
	tags, seen := []string{}, map[string]bool{}
	for _, tag := range in.Tags {
		tag = strings.TrimSpace(tag)
		if utf8.RuneCountInString(tag) > 48 {
			return in, ErrMemoryInput
		}
		if tag != "" && !seen[tag] {
			tags, seen[tag] = append(tags, tag), true
		}
	}
	in.Tags = tags
	values = append(values, tags...)
	for _, value := range values {
		if !utf8.ValidString(value) || strings.ContainsRune(value, 0) {
			return in, ErrMemoryInput
		}
		for _, pattern := range memorySecretPatterns {
			if pattern.MatchString(value) {
				return in, ErrMemoryInput
			}
		}
	}
	return in, nil
}

func memoryContentHash(content string) string {
	// Identity is based on original UTF-8 bytes, never search normalization.
	sum := sha256.Sum256([]byte(content))
	return hex.EncodeToString(sum[:])
}

func memoryAuditTx(ctx context.Context, tx *sql.Tx, entryID, version, jobID int64, admission MemoryAdmission, action string) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO memory_audit(entry_id,entry_version,job_id,task_id,conversation_id,agent_key,session_id,call_key,action)
		VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9)`, entryID, version, jobID, admission.TaskID, admission.ConversationID,
		admission.AgentKey, admission.SessionID, admission.CallKey, action)
	return err
}

func memoryOriginExists(ctx context.Context, q memoryQueryer, taskID, conversationID int64) error {
	if taskID > 0 {
		var exists bool
		if err := q.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM tasks WHERE id=$1 AND deleted_at IS NULL AND archived_at IS NULL)`, taskID).Scan(&exists); err != nil {
			return err
		}
		if !exists {
			return ErrMemoryStale
		}
	}
	if conversationID > 0 {
		var exists bool
		if err := q.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM conversations WHERE id=$1)`, conversationID).Scan(&exists); err != nil {
			return err
		}
		if !exists {
			return ErrMemoryStale
		}
	}
	return nil
}

func (d *DB) CreateManualMemory(ctx context.Context, in MemoryInput) (*MemoryEntry, error) {
	in, err := validateMemoryInput(in, false)
	if err != nil {
		return nil, err
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
	if err := memoryOriginExists(ctx, tx, in.OriginTaskID, in.OriginConversationID); err != nil {
		return nil, err
	}
	if in.ExpiresAt == nil {
		expires := time.Now().UTC().Add(time.Duration(settings.RetentionDays) * 24 * time.Hour)
		in.ExpiresAt = &expires
	}
	tags, _ := json.Marshal(in.Tags)
	var id int64
	err = tx.QueryRowContext(ctx, `INSERT INTO memory_entries(title,content,content_hash,kind,applicability,target,tags,confidence,state,
		origin_task_id,origin_conversation_id,agent_key,session_id,expires_at)
		VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14) RETURNING id`,
		in.Title, in.Content, memoryContentHash(in.Content), in.Kind, in.Applicability, in.Target, string(tags), in.Confidence, in.State,
		in.OriginTaskID, in.OriginConversationID, in.AgentKey, in.SessionID, in.ExpiresAt).Scan(&id)
	if err != nil {
		return nil, err
	}
	if err := memoryAuditTx(ctx, tx, id, 1, 0, MemoryAdmission{TaskID: in.OriginTaskID, ConversationID: in.OriginConversationID, AgentKey: in.AgentKey, SessionID: in.SessionID}, "created"); err != nil {
		return nil, err
	}
	entry, err := getMemoryEntry(ctx, tx, id, "")
	if err != nil {
		return nil, err
	}
	return entry, tx.Commit()
}

func (d *DB) EditMemoryEntry(ctx context.Context, id int64, in MemoryInput, expectedVersion int64) (*MemoryEntry, error) {
	in, err := validateMemoryInput(in, false)
	if err != nil {
		return nil, err
	}
	tx, err := d.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	// Lock settings before entries, matching admission, settings changes and clear.
	if _, err := memorySettings(ctx, tx, "FOR SHARE"); err != nil {
		return nil, err
	}
	if err := lockMemoryEntrySource(ctx, tx, id); err != nil {
		return nil, err
	}
	entry, err := getMemoryEntry(ctx, tx, id, "FOR UPDATE OF e")
	if err != nil {
		return nil, err
	}
	if entry == nil || entry.State == "deleted" || entry.Version != expectedVersion {
		return nil, ErrMemoryConflict
	}
	if entry.Automatic && entry.State == "source_invalid" {
		return nil, ErrMemoryStale
	}
	// Editing preserves provenance. A user can create a genuinely independent
	// note through CreateManualMemory instead of silently severing an old source.
	if _, err := tx.ExecContext(ctx, `UPDATE memory_sources SET suppressed_at=now(),updated_at=now()
		WHERE id=(SELECT source_ref FROM memory_entries WHERE id=$1)`, id); err != nil {
		return nil, err
	}
	tags, _ := json.Marshal(in.Tags)
	_, err = tx.ExecContext(ctx, `UPDATE memory_entries SET title=$2,content=$3,content_hash=$4,kind=$5,applicability=$6,
		target=$7,tags=$8,confidence=$9,state=$10,expires_at=$11,manual_override=true,version=version+1,epoch=epoch+1,updated_at=now()
		WHERE id=$1`, id, in.Title, in.Content, memoryContentHash(in.Content), in.Kind, in.Applicability, in.Target, string(tags), in.Confidence, in.State, in.ExpiresAt)
	if err != nil {
		return nil, err
	}
	if err := memoryAuditTx(ctx, tx, id, entry.Version+1, 0, MemoryAdmission{}, "edited"); err != nil {
		return nil, err
	}
	out, err := getMemoryEntry(ctx, tx, id, "")
	if err != nil {
		return nil, err
	}
	return out, tx.Commit()
}

func (d *DB) DeleteMemoryEntry(ctx context.Context, id, expectedVersion int64) error {
	tx, err := d.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := memorySettings(ctx, tx, "FOR SHARE"); err != nil {
		return err
	}
	if err := lockMemoryEntrySource(ctx, tx, id); err != nil {
		return err
	}
	entry, err := getMemoryEntry(ctx, tx, id, "FOR UPDATE OF e")
	if err != nil {
		return err
	}
	if entry == nil || entry.State == "deleted" || entry.Version != expectedVersion {
		return ErrMemoryConflict
	}
	if _, err := tx.ExecContext(ctx, `UPDATE memory_sources SET suppressed_at=now(),updated_at=now()
		WHERE id=(SELECT source_ref FROM memory_entries WHERE id=$1)`, id); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE memory_entries SET state='deleted',title='',content='',content_hash='',
		applicability='',target='',tags='[]',source_fingerprint='',version=version+1,epoch=epoch+1,updated_at=now() WHERE id=$1`, id); err != nil {
		return err
	}
	if err := memoryAuditTx(ctx, tx, id, entry.Version+1, 0, MemoryAdmission{}, "deleted"); err != nil {
		return err
	}
	return tx.Commit()
}

func (d *DB) ClearMemoryEntries(ctx context.Context) error {
	tx, err := d.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := memorySettings(ctx, tx, "FOR UPDATE"); err != nil {
		return err
	}
	// A new boundary prevents a delayed discoverer from repopulating old sources.
	if _, err := tx.ExecContext(ctx, `UPDATE memory_settings SET epoch=epoch+1,learn_after=now() WHERE singleton=true`); err != nil {
		return err
	}
	rows, err := tx.QueryContext(ctx, `SELECT id FROM memory_sources ORDER BY id FOR UPDATE`)
	if err != nil {
		return err
	}
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return err
		}
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE memory_sources SET suppressed_at=now(),updated_at=now()`); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE memory_entries SET state='deleted',title='',content='',content_hash='',
		applicability='',target='',tags='[]',source_fingerprint='',version=version+1,epoch=epoch+1,updated_at=now() WHERE state<>'deleted'`); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE memory_jobs SET state='skipped',lease_token='',lease_until=NULL,updated_at=now() WHERE state IN ('queued','running','failed')`); err != nil {
		return err
	}
	if err := memoryAuditTx(ctx, tx, 0, 0, 0, MemoryAdmission{}, "cleared"); err != nil {
		return err
	}
	return tx.Commit()
}

func lockMemoryEntrySource(ctx context.Context, tx *sql.Tx, id int64) error {
	var ref sql.NullInt64
	if err := tx.QueryRowContext(ctx, `SELECT source_ref FROM memory_entries WHERE id=$1`, id).Scan(&ref); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ErrMemoryConflict
		}
		return err
	}
	if ref.Valid {
		var sourceID int64
		return tx.QueryRowContext(ctx, `SELECT id FROM memory_sources WHERE id=$1 FOR UPDATE`, ref.Int64).Scan(&sourceID)
	}
	return nil
}

const memoryLivePredicate = `e.state='active' AND (e.expires_at IS NULL OR e.expires_at>now())
	AND (e.origin_task_id=0 OR EXISTS(SELECT 1 FROM tasks mt WHERE mt.id=e.origin_task_id AND mt.deleted_at IS NULL AND mt.archived_at IS NULL))
	AND (e.origin_conversation_id=0 OR EXISTS(SELECT 1 FROM conversations mc WHERE mc.id=e.origin_conversation_id))
	AND (NOT e.automatic OR (s.valid AND EXISTS(SELECT 1 FROM memory_live_sources live
		WHERE live.source_kind=s.source_kind AND live.source_id=s.source_id
		AND live.source_version=e.source_version AND live.source_fingerprint=e.source_fingerprint)))`

func memorySearchTerms(query string) []string {
	// These are search terms only. Do not normalize the content identity hash.
	seen, out := map[string]bool{}, []string{}
	add := func(term string) {
		if utf8.RuneCountInString(term) >= 2 && !seen[term] && len(out) < 12 {
			out, seen[term] = append(out, term), true
		}
	}
	for _, term := range strings.FieldsFunc(strings.ToLower(query), func(r rune) bool {
		return strings.ContainsRune(" \t\r\n,，。;；:：/\\()[]{}\"'", r)
	}) {
		if utf8.RuneCountInString(term) < 2 {
			continue
		}
		if len(term) > 256 {
			term = string([]rune(term)[:min(len([]rune(term)), 64)])
		}
		add(term)
		// Chinese has no required word separators. Bigrams improve recall while
		// never modifying original display text or exact identity hashes.
		han := []rune{}
		flush := func() {
			for i := 0; i+1 < len(han); i++ {
				add(string(han[i : i+2]))
			}
			han = han[:0]
		}
		for _, r := range term {
			if unicode.Is(unicode.Han, r) {
				han = append(han, r)
			} else {
				flush()
			}
		}
		flush()
		if len(out) >= 12 {
			break
		}
	}
	return out
}

func memoryLikePattern(query string) string {
	return "%" + strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(query) + "%"
}

func (d *DB) SearchMemory(ctx context.Context, in MemorySearchInput) ([]MemoryEntry, MemorySettings, error) {
	tx, err := d.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, MemorySettings{}, err
	}
	defer tx.Rollback()
	settings, err := memorySettings(ctx, tx, "")
	if err != nil {
		return nil, settings, err
	}
	if !settings.Enabled {
		return []MemoryEntry{}, settings, nil
	}
	if err := memoryOriginExists(ctx, tx, in.TaskID, in.ConversationID); err != nil {
		return nil, settings, err
	}
	limit := in.Limit
	if limit < 1 || limit > settings.MaxEntries {
		limit = settings.MaxEntries
	}
	args := []any{in.TaskID, in.ConversationID}
	visibility := `(e.origin_task_id=$1 AND $1>0) OR (e.origin_conversation_id=$2 AND $2>0)`
	if settings.GlobalShare {
		visibility = `true`
	}
	terms, matches, scores := memorySearchTerms(in.Query), []string{}, []string{}
	for _, term := range terms {
		args = append(args, memoryLikePattern(term))
		index := len(args)
		match := fmt.Sprintf(`(e.title ILIKE $%[1]d OR e.content ILIKE $%[1]d OR e.applicability ILIKE $%[1]d OR e.target ILIKE $%[1]d OR e.tags::text ILIKE $%[1]d)`, index)
		matches = append(matches, match)
		scores = append(scores, `CASE WHEN `+match+` THEN 1 ELSE 0 END`)
	}
	// Empty/irrelevant queries never cause an unrestricted "latest global notes"
	// dump. Same-context notes remain eligible when a message is just "continue".
	relevance := `((e.origin_task_id=$1 AND $1>0) OR (e.origin_conversation_id=$2 AND $2>0))`
	if len(matches) > 0 {
		relevance += ` OR (` + strings.Join(matches, ` OR `) + `)`
	}
	score := `0`
	if len(scores) > 0 {
		score = strings.Join(scores, `+`)
	}
	args = append(args, limit)
	rows, err := tx.QueryContext(ctx, `SELECT `+memoryEntryCols+` FROM memory_entries e LEFT JOIN memory_sources s ON s.id=e.source_ref
		WHERE `+memoryLivePredicate+` AND (`+visibility+`) AND (`+relevance+`)
		ORDER BY (`+score+`) DESC,CASE WHEN e.origin_task_id=$1 AND $1>0 THEN 1 ELSE 0 END DESC,
		e.updated_at DESC,e.id DESC LIMIT $`+fmt.Sprint(len(args)), args...)
	if err != nil {
		return nil, settings, err
	}
	out := []MemoryEntry{}
	for rows.Next() {
		entry, err := scanMemoryEntry(rows)
		if err != nil {
			rows.Close()
			return nil, settings, err
		}
		out = append(out, entry)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, settings, err
	}
	return out, settings, tx.Commit()
}

// ValidateMemoryAdmission runs after model queueing and before every actual
// HTTP attempt. Locks are short; no transaction remains open across the model
// request. Revoke/delete serializes on these entry rows; source freshness is
// explicitly a final checked snapshot, not an absolute HTTP-send guarantee.
func (d *DB) ValidateMemoryAdmission(ctx context.Context, in MemoryAdmission) error {
	if len(in.References) == 0 {
		return nil
	}
	if len(in.References) > 10 || len(in.AgentKey) > 128 || len(in.SessionID) > 160 || len(in.CallKey) > 160 {
		return ErrMemoryInput
	}
	refs := append([]MemoryRef(nil), in.References...)
	sort.Slice(refs, func(i, j int) bool { return refs[i].ID < refs[j].ID })
	tx, err := d.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	settings, err := memorySettings(ctx, tx, "FOR SHARE")
	if err != nil {
		return err
	}
	if !settings.Enabled || settings.Epoch != in.SettingsEpoch {
		return ErrMemoryStale
	}
	if err := memoryOriginExists(ctx, tx, in.TaskID, in.ConversationID); err != nil {
		return err
	}
	for i, ref := range refs {
		if ref.ID < 1 || ref.Version < 1 || (i > 0 && refs[i-1].ID == ref.ID) {
			return ErrMemoryInput
		}
		var version int64
		var taskID, convID int64
		err := tx.QueryRowContext(ctx, `SELECT e.version,e.origin_task_id,e.origin_conversation_id
			FROM memory_entries e LEFT JOIN memory_sources s ON s.id=e.source_ref
			WHERE e.id=$1 AND `+memoryLivePredicate+` FOR SHARE OF e`, ref.ID).Scan(&version, &taskID, &convID)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrMemoryStale
		}
		if err != nil {
			return err
		}
		if version != ref.Version || (!settings.GlobalShare && !(taskID > 0 && taskID == in.TaskID) && !(convID > 0 && convID == in.ConversationID)) {
			return ErrMemoryStale
		}
		if err := memoryAuditTx(ctx, tx, ref.ID, version, 0, in, "admitted"); err != nil {
			return err
		}
	}
	// Usage counters are intentionally not updated under a SHARE lock. Doing so
	// would create lock-upgrade deadlocks between concurrent admissions. Metrics
	// are derived from the metadata-only admission ledger instead.
	return tx.Commit()
}

func (d *DB) ListMemoryAudit(ctx context.Context, limit int) ([]MemoryAudit, error) {
	return d.ListMemoryAuditFiltered(ctx, 0, 0, "", limit)
}

func (d *DB) ListMemoryAuditFiltered(ctx context.Context, taskID, conversationID int64, sessionID string, limit int) ([]MemoryAudit, error) {
	if limit < 1 || limit > 200 {
		limit = 50
	}
	rows, err := d.QueryContext(ctx, `SELECT id,entry_id,entry_version,task_id,conversation_id,agent_key,session_id,call_key,action,created_at
		FROM memory_audit WHERE ($1=0 OR task_id=$1) AND ($2=0 OR conversation_id=$2) AND ($3='' OR session_id=$3)
		ORDER BY id DESC LIMIT $4`, taskID, conversationID, sessionID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []MemoryAudit{}
	for rows.Next() {
		var entry MemoryAudit
		if err := rows.Scan(&entry.ID, &entry.EntryID, &entry.EntryVersion, &entry.TaskID, &entry.ConversationID, &entry.AgentKey, &entry.SessionID, &entry.CallKey, &entry.Action, &entry.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, entry)
	}
	return out, rows.Err()
}
