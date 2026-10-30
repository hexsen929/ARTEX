package db

import (
	"errors"
	"time"
)

var (
	ErrMemoryInput       = errors.New("invalid memory input")
	ErrMemoryConflict    = errors.New("memory version conflict")
	ErrMemoryUnavailable = errors.New("memory unavailable")
	ErrMemoryStale       = errors.New("memory dependency changed")
	ErrMemoryBudget      = errors.New("memory learning call budget exhausted")
)

// MemorySettings is deliberately independent of settings KV: one locked row
// gives configuration changes and final model-request admission an epoch.
type MemorySettings struct {
	Enabled        bool      `json:"enabled"`
	AutoExtract    bool      `json:"auto_extract"`
	GlobalShare    bool      `json:"global_share"`
	ProfileID      int64     `json:"profile_id"`
	MaxEntries     int       `json:"max_entries"`
	MaxTokens      int       `json:"max_tokens"`
	MaxJobsPerHour int       `json:"max_jobs_per_hour"`
	RetentionDays  int       `json:"retention_days"`
	LearnAfter     time.Time `json:"learn_after"`
	Epoch          int64     `json:"epoch"`
}

// MemoryEntry is reference material, never a task authorization or finding.
// Source identity is retained even after deletion; content and its hash are not.
type MemoryEntry struct {
	ID                   int64      `json:"id"`
	Title                string     `json:"title"`
	Content              string     `json:"content"`
	Kind                 string     `json:"kind"`
	Applicability        string     `json:"applicability"`
	Confidence           string     `json:"confidence"`
	State                string     `json:"status"`
	Target               string     `json:"target"`
	Tags                 []string   `json:"tags"`
	OriginTaskID         int64      `json:"origin_task_id"`
	OriginConversationID int64      `json:"origin_conversation_id"`
	AgentKey             string     `json:"agent_key"`
	SessionID            string     `json:"session_id"`
	SourceKind           string     `json:"source_kind"`
	SourceID             string     `json:"source_id"`
	SourceVersion        int64      `json:"source_version"`
	SourceFingerprint    string     `json:"-"`
	Automatic            bool       `json:"automatic"`
	Version              int64      `json:"version"`
	Epoch                int64      `json:"epoch"`
	CreatedAt            time.Time  `json:"created_at"`
	UpdatedAt            time.Time  `json:"updated_at"`
	ExpiresAt            *time.Time `json:"expires_at,omitempty"`
	UseCount             int64      `json:"use_count"`
	LastUsedAt           *time.Time `json:"last_used_at,omitempty"`
	EvidenceIDs          []string   `json:"evidence_ids"`
	ShareScope           string     `json:"share_scope"`
}

type MemoryInput struct {
	Title                string     `json:"title"`
	Content              string     `json:"content"`
	Kind                 string     `json:"kind"`
	Applicability        string     `json:"applicability"`
	Confidence           string     `json:"confidence"`
	State                string     `json:"status"`
	Target               string     `json:"target"`
	Tags                 []string   `json:"tags"`
	OriginTaskID         int64      `json:"origin_task_id"`
	OriginConversationID int64      `json:"origin_conversation_id"`
	AgentKey             string     `json:"agent_key"`
	SessionID            string     `json:"session_id"`
	ExpiresAt            *time.Time `json:"expires_at,omitempty"`
	EvidenceIDs          []string   `json:"evidence_ids,omitempty"`
}

type MemoryFilter struct {
	Query          string
	State          string
	Kind           string
	TaskID         int64
	ConversationID int64
	Automatic      *bool
}

type MemorySearchInput struct {
	Query          string
	TaskID         int64
	ConversationID int64
	AgentKey       string
	SessionID      string
	Limit          int
}

type MemoryRef struct {
	ID      int64 `json:"id"`
	Version int64 `json:"version"`
}

type MemoryAdmission struct {
	SettingsEpoch  int64
	References     []MemoryRef
	TaskID         int64
	ConversationID int64
	AgentKey       string
	SessionID      string
	CallKey        string
}

// MemorySource contains only an allowlisted, bounded source projection. Text
// is worker-only, not exposed by job/status API serialization.
type MemorySource struct {
	Kind           string    `json:"kind"`
	ID             string    `json:"id"`
	Version        int64     `json:"version"`
	Fingerprint    string    `json:"-"`
	TaskID         int64     `json:"task_id"`
	ConversationID int64     `json:"conversation_id"`
	AgentKey       string    `json:"agent_key"`
	SessionID      string    `json:"session_id"`
	State          string    `json:"state"`
	UpdatedAt      time.Time `json:"updated_at"`
	Text           string    `json:"-"`
	SettingsEpoch  int64     `json:"-"`
	Historical     bool      `json:"-"`
}

type MemoryJob struct {
	ID            int64        `json:"id"`
	Source        MemorySource `json:"source"`
	SettingsEpoch int64        `json:"settings_epoch"`
	State         string       `json:"state"`
	Attempts      int          `json:"attempts"`
	LeaseToken    string       `json:"-"`
	LeaseUntil    *time.Time   `json:"lease_until,omitempty"`
	CreatedAt     time.Time    `json:"created_at"`
	Historical    bool         `json:"-"`
}

type MemoryAudit struct {
	ID             int64     `json:"id"`
	EntryID        int64     `json:"entry_id"`
	EntryVersion   int64     `json:"entry_version"`
	TaskID         int64     `json:"task_id"`
	ConversationID int64     `json:"conversation_id"`
	AgentKey       string    `json:"agent_key"`
	SessionID      string    `json:"session_id"`
	CallKey        string    `json:"call_key"`
	Action         string    `json:"action"`
	CreatedAt      time.Time `json:"created_at"`
}

type MemoryLearningLastJob struct {
	ID        int64     `json:"id"`
	Result    string    `json:"result"`
	UpdatedAt time.Time `json:"updated_at"`
}

type MemoryLearningSnapshot struct {
	Queued           int64                  `json:"queued"`
	Running          int64                  `json:"running"`
	RetryableFailed  int64                  `json:"retryable_failed"`
	Failed           int64                  `json:"failed"`
	Done             int64                  `json:"done"`
	StartsLastHour   int64                  `json:"starts_last_hour"`
	AttemptsLastHour int64                  `json:"attempts_last_hour"`
	LastJob          *MemoryLearningLastJob `json:"last_job,omitempty"`
	WaitingStage     bool                   `json:"waiting_stage"`
}
