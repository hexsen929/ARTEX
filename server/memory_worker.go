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
		// Discover only completed native stages, never individual fact/hint/chat
		// writes. Historical backfill is explicit and bounded in the API.
		for {
			select {
			case <-s.ctx.Done():
				return
			case <-ticker.C:
			case <-s.memoryWake:
			}
			s.memoryWorkerTick(s.ctx)
		}
	}()
}

func (s *Server) memoryWorkerTick(ctx context.Context) {
	// Metadata retention is independent of whether new model calls are enabled.
	if err := s.m.pg.PruneMemory(ctx); err != nil {
		return
	}
	settings, err := s.m.pg.GetMemorySettings(ctx)
	if err != nil || !settings.Enabled || !settings.AutoExtract {
		return
	}
	provider, cfg, err := s.memoryModel(settings)
	if err != nil {
		return
	}
	sources, err := s.m.pg.FindMemorySources(ctx, 20)
	if err != nil {
		log.Printf("[memory] source discovery unavailable; retry on next tick")
		return
	}
	for _, source := range sources {
		if _, err := s.m.pg.QueueMemoryExtraction(ctx, source); err != nil {
			return
		}
	}
	job, err := s.m.pg.ClaimMemoryJob(ctx, 90*time.Second)
	if err != nil || job == nil {
		return
	}
	if job.SettingsEpoch != settings.Epoch {
		_ = s.m.pg.FailMemoryJob(ctx, *job, time.Minute)
		return
	}
	// Re-read the versioned source instead of retaining a second plaintext
	// copy in the queue. Completion validates it again against its fingerprint.
	source, err := s.m.pg.GetMemorySource(ctx, job.Source.Kind, job.Source.ID)
	if err != nil || source == nil || source.Fingerprint != job.Source.Fingerprint {
		_, _ = s.m.pg.CompleteMemoryJob(ctx, *job, nil)
		return
	}
	if _, err := longmemory.SanitizeSource(source.Text); err != nil {
		_, _ = s.m.pg.CompleteMemoryJob(ctx, *job, nil)
		return
	}
	if s.m.egress != nil {
		if _, hit := s.m.egress.Check(source.Text); hit {
			_, _ = s.m.pg.CompleteMemoryJob(ctx, *job, nil)
			return
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
		return
	}
	if s.m.egress != nil {
		if _, hit := s.m.egress.Check(string(envelope)); hit {
			_, _ = s.m.pg.CompleteMemoryJob(ctx, *job, nil)
			return
		}
	}
	drafts, err := longmemory.Extract(callCtx, provider, cfg.Model, input)
	if err != nil {
		if errors.Is(err, longmemory.ErrSensitive) || errors.Is(err, longmemory.ErrInvalid) {
			_, _ = s.m.pg.CompleteMemoryJob(ctx, *job, nil)
			return
		}
		if ctx.Err() == nil {
			_ = s.m.pg.FailMemoryJob(ctx, *job, 5*time.Minute)
		}
		return
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
}
