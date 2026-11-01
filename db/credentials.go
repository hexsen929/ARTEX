package db

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"time"
)

const SettingCredentialAgent = "credential_agent_enabled"
const SettingCredentialTraffic = "credential_traffic_binding"
const CredentialAgentKey = "credential_processor"

type CredentialAccount struct {
	Label    string `json:"label,omitempty"`
	Username string `json:"username,omitempty"`
	Password string `json:"password,omitempty"`
	Token    string `json:"token,omitempty"`
	Cookie   string `json:"cookie,omitempty"`
	Endpoint string `json:"endpoint,omitempty"`
	Protocol string `json:"protocol,omitempty"`
}

// These are verified references and bounded excerpts, not full packet snapshots.
// The original capture can be opened while it remains in the traffic store.
type CredentialTraffic struct {
	ID       string `json:"id"`
	URL      string `json:"url"`
	Method   string `json:"method"`
	Status   int    `json:"status"`
	Side     string `json:"side"`
	Excerpt  string `json:"excerpt"`
	Accounts []int  `json:"accounts"`
}
type CredentialAnalysis struct {
	ConversationID int64               `json:"conversation_id,omitempty"`
	ForceExtract   bool                `json:"force_extract,omitempty"`
	TrafficNote    string              `json:"traffic_note,omitempty"`
	State          string              `json:"state"`
	Lease          string              `json:"lease,omitempty"`
	LeaseUntil     time.Time           `json:"lease_until,omitempty"`
	SourceHash     string              `json:"source_hash,omitempty"`
	Method         string              `json:"method,omitempty"`
	Accounts       []CredentialAccount `json:"accounts"`
	Traffic        []CredentialTraffic `json:"traffic"`
	TrafficState   string              `json:"traffic_state"`
	Note           string              `json:"note,omitempty"`
	Error          string              `json:"error,omitempty"`
	Model          string              `json:"model,omitempty"`
	InputTokens    int                 `json:"input_tokens"`
	OutputTokens   int                 `json:"output_tokens"`
	ProcessedAt    *time.Time          `json:"processed_at,omitempty"`
}

func CredentialSourceHash(content string) string {
	sum := sha256.Sum256([]byte(content))
	return hex.EncodeToString(sum[:])
}

// The dependent switch and its parent are committed atomically. Turning the
// parent off also clears binding, so re-enabling never silently enables binding.
func (d *DB) SetCredentialAgentSettings(ctx context.Context, enabled, binding *bool) error {
	return d.SetIntelligenceSettings(ctx, nil, enabled, binding)
}
func (d *DB) SetIntelligenceSettings(ctx context.Context, recording, enabled, binding *bool) error {
	tx, err := d.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(7337741006)`); err != nil {
		return err
	}
	record, on, bind := false, false, false
	var value string
	err = tx.QueryRowContext(ctx, `SELECT value FROM settings WHERE key=$1`, SettingTaskIntelligence).Scan(&value)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	record = value == "true" || value == "1"
	if recording != nil {
		record = *recording
	}
	value = ""
	err = tx.QueryRowContext(ctx, `SELECT value FROM settings WHERE key=$1`, SettingCredentialAgent).Scan(&value)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	on = value == "true" || value == "1"
	value = ""
	err = tx.QueryRowContext(ctx, `SELECT value FROM settings WHERE key=$1`, SettingCredentialTraffic).Scan(&value)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	bind = value == "true" || value == "1"
	if enabled != nil {
		on = *enabled
	}
	if binding != nil {
		bind = *binding
	}
	if !record {
		if enabled != nil && *enabled {
			return fmt.Errorf("%w: 请先开启 Agent 自动记录，再开启 Agent 开关", ErrIntelligenceInvalid)
		}
		if binding != nil && *binding {
			return fmt.Errorf("%w: 请依次开启 Agent 自动记录和 Agent 开关", ErrIntelligenceInvalid)
		}
		on = false
		bind = false
	}
	if !on {
		if binding != nil && *binding {
			return fmt.Errorf("%w: 请先开启 Agent 开关，再启用绑定流量", ErrIntelligenceInvalid)
		}
		bind = false
	}
	for key, val := range map[string]bool{SettingTaskIntelligence: record, SettingCredentialAgent: on, SettingCredentialTraffic: bind} {
		if _, err = tx.ExecContext(ctx, `INSERT INTO settings(key,value) VALUES($1,$2) ON CONFLICT(key) DO UPDATE SET value=EXCLUDED.value,updated_at=now()`, key, strconv.FormatBool(val)); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// One durable lease per record, shared by all application processes. Errors do
// not retry every tick; explicit retry queues the record again. Expired leases
// recover interrupted runs after restart. No task lock is held during LLM I/O.
func (d *DB) ClaimCredentialJob(ctx context.Context, bind bool, taskID int64) (*Intelligence, error) {
	tx, err := d.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	var id int64
	err = tx.QueryRowContext(ctx, `SELECT i.id FROM task_intelligence i JOIN tasks t ON t.id=i.task_id
 WHERE i.kind='credential' AND EXISTS(SELECT 1 FROM settings WHERE key='credential_agent_enabled' AND value IN ('true','1')) AND EXISTS(SELECT 1 FROM settings WHERE key='task_intelligence_enabled' AND value IN ('true','1')) AND ($2::bigint=0 OR i.task_id=$2) AND t.deleted_at IS NULL AND t.archived_at IS NULL
 AND NOT EXISTS(SELECT 1 FROM task_archives a WHERE a.task_id=t.id AND a.state IN ('archive_queued','archiving','restore_queued','restoring','delete_queued','deleting'))
 AND (i.credential_analysis IS NULL OR i.credential_analysis->>'state'='queued'
 OR (i.credential_analysis->>'state'='processing' AND (i.credential_analysis->>'lease_until')::timestamptz < now())
 OR ($1 AND i.credential_analysis->>'state'='ready' AND i.credential_analysis->>'traffic_state'='off'))
 ORDER BY i.id FOR UPDATE OF i SKIP LOCKED LIMIT 1`, bind, taskID).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	token := make([]byte, 16)
	if _, err = rand.Read(token); err != nil {
		return nil, err
	}
	patch, _ := json.Marshal(map[string]any{"state": "processing", "lease": hex.EncodeToString(token), "lease_until": time.Now().UTC().Add(3 * time.Minute)})
	if _, err = tx.ExecContext(ctx, `UPDATE task_intelligence SET credential_analysis=COALESCE(credential_analysis,'{}'::jsonb)||$2::jsonb WHERE id=$1`, id, string(patch)); err != nil {
		return nil, err
	}
	if err = tx.Commit(); err != nil {
		return nil, err
	}
	return d.GetIntelligence(ctx, id)
}

// FinishCredentialJob preserves the original API. A stale or disabled job is
// deliberately a no-op, not an error; callers needing admission use the checked
// form below. Conversation activities are committed only with an accepted result.
func (d *DB) FinishCredentialJob(ctx context.Context, item *Intelligence, result CredentialAnalysis) error {
	_, err := d.FinishCredentialJobAccepted(ctx, item, result)
	return err
}

func (d *DB) FinishCredentialJobAccepted(ctx context.Context, item *Intelligence, result CredentialAnalysis) (bool, error) {
	if item == nil || item.Credentials == nil || item.ID <= 0 || item.TaskID <= 0 || item.Credentials.Lease == "" {
		return false, fmt.Errorf("%w: missing credential job identity", ErrIntelligenceInvalid)
	}
	tx, err := d.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	// Match settings and archive/deletion lock order before touching the job row.
	if _, err = tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(7337741006)`); err != nil {
		return false, err
	}
	var taskID int64
	err = tx.QueryRowContext(ctx, `SELECT id FROM tasks t WHERE id=$1 AND deleted_at IS NULL AND archived_at IS NULL
 AND NOT EXISTS(SELECT 1 FROM task_archives a WHERE a.task_id=t.id AND a.state IN ('archive_queued','archiving','restore_queued','restoring','delete_queued','deleting'))
 FOR UPDATE`, item.TaskID).Scan(&taskID)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	var content string
	var previous []byte
	err = tx.QueryRowContext(ctx, `SELECT content,credential_analysis FROM task_intelligence
 WHERE id=$1 AND task_id=$2 AND kind='credential' FOR UPDATE`, item.ID, taskID).Scan(&content, &previous)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	var current CredentialAnalysis
	if err = json.Unmarshal(previous, &current); err != nil {
		return false, err
	}
	if current.State != "processing" || current.Lease != item.Credentials.Lease || !current.LeaseUntil.After(time.Now().UTC()) {
		return false, nil
	}
	var enabled, binding bool
	if err = tx.QueryRowContext(ctx, `SELECT (EXISTS(SELECT 1 FROM settings WHERE key=$1 AND value IN ('true','1')) AND EXISTS(SELECT 1 FROM settings WHERE key='task_intelligence_enabled' AND value IN ('true','1'))),EXISTS(SELECT 1 FROM settings WHERE key=$2 AND value IN ('true','1'))`, SettingCredentialAgent, SettingCredentialTraffic).Scan(&enabled, &binding); err != nil {
		return false, err
	}
	if !enabled || CredentialSourceHash(content) != CredentialSourceHash(item.Content) {
		// Release only this lease. No derived result or completion transcript is
		// published when the feature was disabled or its source changed in flight.
		current.State, current.Lease, current.LeaseUntil = "queued", "", time.Time{}
		raw, marshalErr := json.Marshal(current)
		if marshalErr != nil {
			return false, marshalErr
		}
		if _, err = tx.ExecContext(ctx, `UPDATE task_intelligence SET credential_analysis=$2::jsonb WHERE id=$1`, item.ID, string(raw)); err != nil {
			return false, err
		}
		return false, tx.Commit()
	}
	if result.State != "ready" && result.State != "error" {
		return false, fmt.Errorf("%w: invalid credential completion state", ErrIntelligenceInvalid)
	}
	modelAttempted := result.Model != "" && (current.ForceExtract || current.SourceHash != CredentialSourceHash(content) || current.Method == "" || (current.Error != "" && len(current.Accounts) == 0))
	result.SourceHash = CredentialSourceHash(content)
	if !binding {
		result.Traffic = current.Traffic
		result.TrafficState = "off"
		result.TrafficNote = ""
	}
	// The caller's pointer is not authority. Only a persisted conversation with
	// this exact host-written source marker may be reused.
	result.ConversationID, err = credentialConversationTx(ctx, tx, current.ConversationID, taskID, item.ID)
	if err != nil {
		return false, err
	}
	result.Lease = ""
	result.LeaseUntil = time.Time{}
	now := time.Now().UTC()
	result.ProcessedAt = &now
	raw, err := json.Marshal(result)
	if err != nil {
		return false, err
	}
	updated, err := tx.ExecContext(ctx, `UPDATE task_intelligence SET credential_analysis=$2::jsonb WHERE id=$1`, item.ID, string(raw))
	if err != nil {
		return false, err
	}
	if n, err := updated.RowsAffected(); err != nil || n != 1 {
		if err != nil {
			return false, err
		}
		return false, ErrIntelligenceNotFound
	}
	if err := appendCredentialResultTx(ctx, tx, result.ConversationID, taskID, item.ID, result, modelAttempted); err != nil {
		return false, err
	}
	if err := tx.Commit(); err != nil {
		return false, err
	}
	return true, nil
}

// Credential conversations are an index into the source record, not another
// store for passwords, tokens, raw traffic, leases or arbitrary provider errors.
func credentialConversationTx(ctx context.Context, tx *sql.Tx, previous, taskID, intelligenceID int64) (int64, error) {
	source, _ := json.Marshal(struct {
		TaskID         int64 `json:"task_id"`
		IntelligenceID int64 `json:"intelligence_id"`
	}{taskID, intelligenceID})
	if previous > 0 {
		var id int64
		err := tx.QueryRowContext(ctx, `SELECT c.id FROM conversations c WHERE c.id=$1 AND c.agent_key=$2
 AND EXISTS(SELECT 1 FROM conversation_activities a WHERE a.conversation_id=c.id AND a.worker=$2 AND a.tool='credential_processing_source' AND a.detail=$3)
 AND NOT EXISTS(SELECT 1 FROM task_intelligence i WHERE i.id<>$4 AND i.credential_analysis->>'conversation_id'=$1::text)
 FOR UPDATE OF c`, previous, CredentialAgentKey, string(source), intelligenceID).Scan(&id)
		if err == nil {
			return id, nil
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return 0, err
		}
	}
	var id int64
	if err := tx.QueryRowContext(ctx, `INSERT INTO conversations(agent_key,title) VALUES($1,$2) RETURNING id`, CredentialAgentKey,
		fmt.Sprintf("凭据整理 · task#%d / intelligence#%d", taskID, intelligenceID)).Scan(&id); err != nil {
		return 0, err
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO conversation_activities(conversation_id,worker,kind,tool,summary,detail)
 VALUES($1,$2,'text','credential_processing_source','凭据来源（仅记录关联，不复制密钥）',$3)`, id, CredentialAgentKey, string(source))
	return id, err
}

func appendCredentialResultTx(ctx context.Context, tx *sql.Tx, conversationID, taskID, intelligenceID int64, result CredentialAnalysis, modelAttempted bool) error {
	state, summary := "ready", "凭据整理完成；结果请查看来源任务的情报记录"
	failed := result.State != "ready"
	if failed {
		state, summary = "error", "凭据整理失败；详情请查看来源任务的情报记录"
	}
	method := "unknown"
	if result.Method == "rules" || result.Method == "agent" {
		method = result.Method
	}
	// Model is deployment metadata, not the unbounded provider error/body.
	model := []rune(result.Model)
	if len(model) > 160 {
		model = model[:160]
	}
	detail, _ := json.Marshal(struct {
		TaskID         int64  `json:"task_id"`
		IntelligenceID int64  `json:"intelligence_id"`
		State          string `json:"state"`
		Method         string `json:"method"`
		AccountCount   int    `json:"account_count"`
		TrafficCount   int    `json:"traffic_count"`
		Model          string `json:"model,omitempty"`
	}{taskID, intelligenceID, state, method, len(result.Accounts), len(result.Traffic), string(model)})
	inputTokens, outputTokens := 0, 0
	if modelAttempted {
		inputTokens, outputTokens = max(0, result.InputTokens), max(0, result.OutputTokens)
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO conversation_activities(conversation_id,worker,kind,tool,is_error,summary,detail,input_tokens,output_tokens)
 VALUES($1,$2,'result','credential_processing_status',$3,$4,$5,$6,$7)`, conversationID, CredentialAgentKey, failed, summary, string(detail), inputTokens, outputTokens)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `UPDATE conversations SET updated_at=now() WHERE id=$1`, conversationID)
	return err
}

func (d *DB) RetryCredentialJob(ctx context.Context, taskID, id int64) error {
	result, err := d.ExecContext(ctx, `UPDATE task_intelligence i SET credential_analysis=COALESCE(credential_analysis,'{}'::jsonb)||'{"state":"queued","error":"","force_extract":true}'::jsonb
 WHERE id=$1 AND task_id=$2 AND kind='credential'
 AND COALESCE(credential_analysis->>'state','')<>'processing'
 AND EXISTS(SELECT 1 FROM tasks t WHERE t.id=i.task_id AND t.deleted_at IS NULL AND t.archived_at IS NULL)`, id, taskID)
	if err != nil {
		return err
	}
	n, _ := result.RowsAffected()
	if n == 0 {
		return ErrIntelligenceNotFound
	}
	return nil
}
