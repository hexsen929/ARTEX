package server

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"strconv"
	"sync/atomic"
	"time"

	"github.com/Autumn-27/artex/agent"
	"github.com/Autumn-27/artex/db"
	"github.com/Autumn-27/artex/llmrec"
	"github.com/Autumn-27/artex/memcontext"
	longmemory "github.com/Autumn-27/artex/memory"
	"github.com/Autumn-27/norma/llm"
)

// Error text from providers and database drivers can contain request bodies,
// endpoints or credentials. Only a closed host-selected category is logged.
type memoryWorkerError struct {
	category string
	cause    error
}

func (e *memoryWorkerError) Error() string { return "memory learning deferred: " + e.category }
func (e *memoryWorkerError) Unwrap() error { return e.cause }

func memoryWorkerStorageError(ctx context.Context, cause error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return &memoryWorkerError{category: "storage_error", cause: cause}
}

func memoryWorkerErrorCategory(err error) string {
	var classified *memoryWorkerError
	if errors.As(err, &classified) {
		return classified.category
	}
	if errors.Is(err, context.Canceled) {
		return "cancelled"
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return "timeout"
	}
	return "storage_error"
}

func (s *Server) wakeMemoryWorker() {
	if s.memoryWake == nil {
		return
	}
	select {
	case s.memoryWake <- struct{}{}:
	default:
	}
}

func memoryDiscoveryBackoff(failures int) time.Duration {
	return time.Minute * time.Duration(1<<(max(1, min(failures, 5))-1))
}

func (s *Server) startMemoryWorker() {
	if s.m == nil || s.m.pg == nil {
		return
	}
	s.memoryWake = make(chan struct{}, 1)
	s.memoryWG.Add(1)
	go func() {
		defer s.memoryWG.Done()
		ticker := time.NewTicker(30 * time.Second)
		defer ticker.Stop()
		retention := time.NewTicker(time.Hour)
		defer retention.Stop()
		prune := func() {
			pruneCtx, cancel := context.WithTimeout(s.ctx, 3*time.Second)
			defer cancel()
			_ = s.m.pg.PruneMemory(pruneCtx)
		}
		prune()
		var nextAttempt time.Time
		failures := 0
		// Discover only completed native stages, never individual fact/hint/chat
		// writes. Historical backfill is explicit and bounded in the API.
		for {
			select {
			case <-s.ctx.Done():
				return
			case <-retention.C:
				prune()
				continue
			case <-ticker.C:
			case <-s.memoryWake:
			}
			if time.Now().Before(nextAttempt) {
				continue
			}
			if err := s.memoryWorkerTickResult(s.ctx); err != nil {
				failures = min(failures+1, 5)
				delay := memoryDiscoveryBackoff(failures)
				nextAttempt = time.Now().Add(delay)
				log.Printf("[memory] learning deferred; category=%s retry_in=%s", memoryWorkerErrorCategory(err), delay)
			} else {
				failures = 0
				nextAttempt = time.Time{}
			}
		}
	}()
}

func (s *Server) memoryWorkerTick(ctx context.Context) {
	_ = s.memoryWorkerTickResult(ctx)
}

// Admission checks precede source construction, including when the HTTP budget
// or queue is full. Database work has a separate deadline from model inference.
func (s *Server) discoverAndQueueMemorySources(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	allowed, err := s.m.pg.MemoryDiscoveryAllowed(ctx)
	if err != nil || !allowed {
		return err
	}
	sources, err := s.m.pg.FindMemorySources(ctx, 4)
	if err != nil {
		return err
	}
	for _, source := range sources {
		if _, err := s.m.pg.QueueMemoryExtraction(ctx, source); err != nil {
			return err
		}
	}
	return nil
}

func (s *Server) memoryWorkerTickResult(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	settings, err := s.m.pg.GetMemorySettings(ctx)
	if err != nil {
		return memoryWorkerStorageError(ctx, err)
	}
	if !settings.Enabled || !settings.AutoExtract {
		return nil
	}
	provider, cfg, err := s.memoryModel(settings)
	if err != nil {
		return nil
	}
	if err := s.discoverAndQueueMemorySources(ctx); err != nil {
		return memoryWorkerStorageError(ctx, err)
	}
	job, err := s.m.pg.ClaimMemoryJob(ctx, 90*time.Second)
	if err != nil {
		return memoryWorkerStorageError(ctx, err)
	}
	if job == nil {
		return nil
	}
	return s.runClaimedMemoryJob(ctx, settings, provider, cfg, *job)
}

// These transitions deliberately use the parent worker context, never a
// detached context after shutdown. A changed/expired lease belongs to its new
// owner and must not be overwritten by a late result.
func (s *Server) skipMemoryOutcome(ctx context.Context, job db.MemoryJob, result string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	err := s.m.pg.SkipMemoryJobWithResult(ctx, job, result)
	if err == nil || errors.Is(err, db.ErrMemoryStale) {
		return nil
	}
	return memoryWorkerStorageError(ctx, err)
}

func (s *Server) failMemoryOutcome(ctx context.Context, job db.MemoryJob, result string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	err := s.m.pg.FailMemoryJobWithResult(ctx, job, 5*time.Minute, result)
	if err == nil || errors.Is(err, db.ErrMemoryStale) {
		return nil
	}
	return memoryWorkerStorageError(ctx, err)
}

func (s *Server) memoryStorageOutcome(ctx context.Context, job db.MemoryJob, cause error) error {
	if err := s.failMemoryOutcome(ctx, job, "storage_error"); err != nil {
		return err
	}
	// Even when the failure transition succeeds, storage trouble gets the
	// existing worker backoff instead of looking like an ordinary empty result.
	return memoryWorkerStorageError(ctx, cause)
}

func (s *Server) runClaimedMemoryJob(ctx context.Context, settings db.MemorySettings, provider llm.Provider, cfg agent.Config, job db.MemoryJob) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if job.SettingsEpoch != settings.Epoch {
		// Claim uses the authoritative settings row. If it admitted a newer
		// epoch than the earlier provider lookup, do not discard that new job or
		// send it through the old model. Resolve its explicit current route once.
		current, err := s.m.pg.GetMemorySettings(ctx)
		if err != nil {
			return s.memoryStorageOutcome(ctx, job, err)
		}
		if !current.Enabled || !current.AutoExtract || current.Epoch != job.SettingsEpoch {
			return s.skipMemoryOutcome(ctx, job, "settings_changed")
		}
		actualProvider, actualConfig, err := s.memoryModel(current)
		if err != nil {
			return s.failMemoryOutcome(ctx, job, "settings_changed")
		}
		settings, provider, cfg = current, actualProvider, actualConfig
	}
	// Re-read the versioned source instead of retaining a second plaintext
	// copy in the queue. Completion validates it again against its fingerprint.
	source, err := s.m.pg.GetMemorySource(ctx, job.Source.Kind, job.Source.ID)
	if err != nil {
		if errors.Is(err, db.ErrMemoryInput) || errors.Is(err, db.ErrMemoryUnavailable) {
			return s.skipMemoryOutcome(ctx, job, "input_invalid")
		}
		return s.memoryStorageOutcome(ctx, job, err)
	}
	if source == nil || source.Fingerprint != job.Source.Fingerprint || source.Version != job.Source.Version {
		return s.skipMemoryOutcome(ctx, job, "source_changed")
	}
	if _, err := longmemory.SanitizeSource(source.Text); err != nil {
		result := "input_invalid"
		if errors.Is(err, longmemory.ErrSensitive) {
			result = "input_sensitive"
		}
		return s.skipMemoryOutcome(ctx, job, result)
	}
	if s.m.egress != nil {
		if _, hit := s.m.egress.Check(source.Text); hit {
			return s.skipMemoryOutcome(ctx, job, "input_sensitive")
		}
	}
	callCtx, cancel := context.WithTimeout(ctx, longmemory.MaxDuration)
	defer cancel()
	callCtx = memcontext.WithMetadataOnly(callCtx)
	var admissionFailure atomic.Value // safe code only; no source/error text
	callCtx, release := memcontext.WithRecall(callCtx, nil, func(checkCtx context.Context) error {
		checkCtx, stop := context.WithTimeout(checkCtx, 2*time.Second)
		defer stop()
		// A profile may be edited without changing the memory settings epoch.
		// Pure rejection checks precede the actual-attempt ledger admission:
		// neither a changed model nor a newly protected secret sends HTTP.
		_, actual, err := s.memoryModel(settings)
		if err != nil || actual != cfg {
			admissionFailure.Store("settings_changed")
			return db.ErrMemoryStale
		}
		if s.m.egress != nil {
			if _, hit := s.m.egress.Check(source.Text); hit {
				admissionFailure.Store("input_sensitive")
				return db.ErrMemoryStale
			}
		}
		// This remains the final source/settings/lease and dual-budget gate.
		// Its transaction ends before HTTP and no earlier check consumes quota.
		if err := s.m.pg.ValidateMemoryExtractionJob(checkCtx, job); err != nil {
			code := "storage_error"
			if errors.Is(err, db.ErrMemoryBudget) {
				code = "budget_deferred"
			} else if errors.Is(err, db.ErrMemoryStale) {
				code = "source_changed"
			}
			admissionFailure.Store(code)
			return err
		}
		return nil
	})
	defer release()
	callCtx = llmrec.WithWorker(callCtx, "memory_extractor")
	if source.TaskID > 0 {
		callCtx = llmrec.WithTaskID(callCtx, strconv.FormatInt(source.TaskID, 10))
	}
	input := longmemory.Input{
		Kind: source.Kind, ID: source.ID, Version: strconv.FormatInt(source.Version, 10), AgentKey: source.AgentKey,
		TaskID: source.TaskID, ConversationID: source.ConversationID, Content: source.Text, Status: source.State,
	}
	// Compare the entire envelope against registered secrets as well as source
	// text. Metadata is host-selected but must not become a secret side channel.
	envelope, err := json.Marshal(input)
	if err != nil {
		return s.skipMemoryOutcome(ctx, job, "input_invalid")
	}
	if s.m.egress != nil {
		if _, hit := s.m.egress.Check(string(envelope)); hit {
			return s.skipMemoryOutcome(ctx, job, "input_sensitive")
		}
	}
	extracted, err := longmemory.ExtractDetailed(callCtx, provider, cfg.Model, input)
	if err != nil {
		if err := ctx.Err(); err != nil {
			return err
		}
		if reason, _ := admissionFailure.Load().(string); reason != "" {
			switch reason {
			case "source_changed", "input_sensitive":
				return s.skipMemoryOutcome(ctx, job, reason)
			case "settings_changed":
				return s.failMemoryOutcome(ctx, job, reason)
			case "storage_error":
				return s.memoryStorageOutcome(ctx, job, errors.New("memory request admission storage failure"))
			case "budget_deferred":
				return s.failMemoryOutcome(ctx, job, reason)
			}
		}
		switch reason := longmemory.DiagnosticCode(err); reason {
		case "input_invalid", "input_sensitive", "output_rejected", "output_sensitive":
			return s.skipMemoryOutcome(ctx, job, reason)
		case "output_invalid", "output_truncated", "provider_error", "timeout":
			return s.failMemoryOutcome(ctx, job, reason)
		default:
			// An unclassified local cancellation is not evidence of API failure.
			// Return it to the bounded worker loop; lease recovery remains guarded.
			if errors.Is(err, context.Canceled) {
				return context.Canceled
			}
			return s.failMemoryOutcome(ctx, job, "provider_error")
		}
	}
	if extracted.Outcome == "output_rejected" && len(extracted.Drafts) == 0 {
		return s.skipMemoryOutcome(ctx, job, "output_rejected")
	}
	if (extracted.Outcome != "empty" || len(extracted.Drafts) != 0) && (extracted.Outcome != "drafts" || len(extracted.Drafts) == 0) {
		return s.failMemoryOutcome(ctx, job, "output_invalid")
	}
	entries := make([]db.MemoryInput, 0, len(extracted.Drafts))
	rejected := "output_rejected"
	for _, draft := range extracted.Drafts {
		switch draft.Kind {
		case "environment", "procedure", "lesson":
		default:
			continue
		}
		if draft.Target != "" {
			continue
		}
		entry := db.MemoryInput{Title: draft.Title, Content: draft.Content, Kind: draft.Kind,
			Applicability: draft.Applicability, Tags: draft.Tags, EvidenceIDs: append([]string(nil), draft.EvidenceIDs...),
			Confidence: "unverified", State: "active", OriginTaskID: source.TaskID,
			OriginConversationID: source.ConversationID, AgentKey: source.AgentKey, SessionID: source.SessionID}
		if err := s.validateMemoryInput(entry); err != nil {
			if errors.Is(err, longmemory.ErrSensitive) {
				rejected = "output_sensitive"
			}
			continue
		}
		entries = append(entries, entry)
	}
	if len(extracted.Drafts) > 0 && len(entries) == 0 {
		return s.skipMemoryOutcome(ctx, job, rejected)
	}
	if _, err := s.m.pg.CompleteMemoryJobWithResult(ctx, job, entries, "empty"); err != nil {
		if errors.Is(err, db.ErrMemoryStale) {
			return nil
		}
		if errors.Is(err, db.ErrMemoryUnavailable) {
			return s.skipMemoryOutcome(ctx, job, "source_changed")
		}
		if errors.Is(err, db.ErrMemoryInput) {
			return s.skipMemoryOutcome(ctx, job, "output_rejected")
		}
		return s.memoryStorageOutcome(ctx, job, err)
	}
	return nil
}
