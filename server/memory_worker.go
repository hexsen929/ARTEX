package server

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"strconv"
	"time"

	"github.com/Autumn-27/artex/db"
	"github.com/Autumn-27/artex/llmrec"
	"github.com/Autumn-27/artex/memcontext"
	longmemory "github.com/Autumn-27/artex/memory"
)

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
				log.Printf("[memory] discovery deferred after failure; retry in %s", delay)
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
	settings, err := s.m.pg.GetMemorySettings(ctx)
	if err != nil {
		return err
	}
	if !settings.Enabled || !settings.AutoExtract {
		return nil
	}
	provider, cfg, err := s.memoryModel(settings)
	if err != nil {
		return nil
	}
	if err := s.discoverAndQueueMemorySources(ctx); err != nil {
		return err
	}
	job, err := s.m.pg.ClaimMemoryJob(ctx, 90*time.Second)
	if err != nil || job == nil {
		return nil
	}
	if job.SettingsEpoch != settings.Epoch {
		_ = s.m.pg.FailMemoryJob(ctx, *job, time.Minute)
		return nil
	}
	// Re-read the versioned source instead of retaining a second plaintext
	// copy in the queue. Completion validates it again against its fingerprint.
	source, err := s.m.pg.GetMemorySource(ctx, job.Source.Kind, job.Source.ID)
	if err != nil || source == nil || source.Fingerprint != job.Source.Fingerprint {
		_, _ = s.m.pg.CompleteMemoryJob(ctx, *job, nil)
		return nil
	}
	if _, err := longmemory.SanitizeSource(source.Text); err != nil {
		_, _ = s.m.pg.CompleteMemoryJob(ctx, *job, nil)
		return nil
	}
	if s.m.egress != nil {
		if _, hit := s.m.egress.Check(source.Text); hit {
			_, _ = s.m.pg.CompleteMemoryJob(ctx, *job, nil)
			return nil
		}
	}
	callCtx, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()
	callCtx = memcontext.WithMetadataOnly(callCtx)
	callCtx, release := memcontext.WithRecall(callCtx, nil, func(checkCtx context.Context) error {
		checkCtx, stop := context.WithTimeout(checkCtx, 2*time.Second)
		defer stop()
		if err := s.m.pg.ValidateMemoryExtractionJob(checkCtx, *job); err != nil {
			return err
		}
		// A profile may be edited without changing the memory settings epoch.
		_, actual, err := s.memoryModel(settings)
		if err != nil || actual != cfg {
			return db.ErrMemoryStale
		}
		if s.m.egress != nil {
			if _, hit := s.m.egress.Check(source.Text); hit {
				return db.ErrMemoryStale
			}
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
		_, _ = s.m.pg.CompleteMemoryJob(ctx, *job, nil)
		return nil
	}
	if s.m.egress != nil {
		if _, hit := s.m.egress.Check(string(envelope)); hit {
			_, _ = s.m.pg.CompleteMemoryJob(ctx, *job, nil)
			return nil
		}
	}
	drafts, err := longmemory.Extract(callCtx, provider, cfg.Model, input)
	if err != nil {
		if errors.Is(err, longmemory.ErrSensitive) || errors.Is(err, longmemory.ErrInvalid) {
			_, _ = s.m.pg.CompleteMemoryJob(ctx, *job, nil)
			return nil
		}
		if ctx.Err() == nil {
			_ = s.m.pg.FailMemoryJob(ctx, *job, 5*time.Minute)
		}
		return nil
	}
	entries := make([]db.MemoryInput, 0, len(drafts))
	for _, draft := range drafts {
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
			continue
		}
		entries = append(entries, entry)
	}
	if _, err := s.m.pg.CompleteMemoryJob(ctx, *job, entries); err != nil {
		log.Printf("[memory] learning result not admitted; source or settings may have changed")
	}
	return nil
}
