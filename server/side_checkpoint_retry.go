package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/Autumn-27/artex/db"
	"github.com/Autumn-27/artex/sidequestion"
	"github.com/jackc/pgx/v5/pgconn"
)

type sideCheckpointRetry struct {
	RunID      int64
	Parent     sidequestion.Parent
	Failures   int
	Next       time.Time
	Permanent  bool
	Gone       bool
	ErrorLabel string // redacted category, never a raw driver/provider error
}

type sideCheckpointSave struct {
	Snapshot sidequestion.Snapshot
	Done     chan struct{}
}

type sideCheckpointFailure struct {
	Err        error
	Parent     sidequestion.Parent
	RunID      int64
	Version    int64
	Permanent  bool
	RetryAfter time.Duration
}

func (f sideCheckpointFailure) identityLabel() string {
	// Every value comes from numeric host identities, not request or model text.
	return fmt.Sprintf("parent=%s task=%d exploration=%d intent=%d conversation=%d approval=%d run=%d version=%d", f.Parent.Key(), f.Parent.TaskID, f.Parent.ExplorationID, f.Parent.IntentID, f.Parent.ConversationID, f.Parent.ApprovalID, f.RunID, f.Version)
}

func (s *sideQuestionState) ensureCheckpointMaps() {
	if s.pending == nil {
		s.pending = map[string]sidequestion.Snapshot{}
	}
	if s.latest == nil {
		s.latest = map[string]sidequestion.Snapshot{}
	}
	if s.seen == nil {
		s.seen = map[string][2]int64{}
	}
	if s.checkpoints == nil {
		s.checkpoints = map[string]sideCheckpointRetry{}
	}
	if s.saving == nil {
		s.saving = map[string]sideCheckpointSave{}
	}
	if s.deletedTasks == nil {
		s.deletedTasks = map[int64]struct{}{}
	}
}

func (s *sideQuestionState) publishCheckpoint(snap sidequestion.Snapshot) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ensureCheckpointMaps()
	if _, deleted := s.deletedTasks[snap.Parent.TaskID]; snap.Parent.TaskID > 0 && deleted {
		return // A committed hard delete also rejects delayed/new-run publishers.
	}
	key := snap.Parent.Key()
	old, ok := s.seen[key]
	if ok && (old[0] > snap.RunID || old[0] == snap.RunID && old[1] >= snap.Version) {
		return
	}
	s.seen[key] = [2]int64{snap.RunID, snap.Version}
	if retry, exists := s.checkpoints[key]; exists {
		if retry.RunID != snap.RunID || retry.Parent != snap.Parent {
			// A genuinely new run or corrected metadata may recover, but not a
			// stream version change alone.
			delete(s.checkpoints, key)
		} else if retry.Gone {
			return
		}
	}
	s.latest[key] = snap
	s.pending[key] = snap
}

// flushCheckpoints retains the background/shutdown API. Persistence never holds
// commands across database I/O: unrelated task admission/drains must remain free.
func (s *sideQuestionState) flushCheckpoints(now func() time.Time, save func(context.Context, sidequestion.Snapshot) error, report func(sideCheckpointFailure), forceTransient ...bool) {
	force := len(forceTransient) > 0 && forceTransient[0]
	_ = s.flushCheckpointsMatching(context.Background(), now, save, report, nil, force)
}

// A manual archive uses its own deadline and only its task's checkpoints. A key
// is claimed immediately before its actual save, not while queued behind saves
// belonging to other tasks. Pending remains visible throughout in-flight I/O.
func (s *sideQuestionState) flushCheckpointsMatching(ctx context.Context, now func() time.Time, save func(context.Context, sidequestion.Snapshot) error, report func(sideCheckpointFailure), match func(sidequestion.Parent) bool, forceTransient bool) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.mu.Lock()
	s.ensureCheckpointMaps()
	keys := make([]string, 0, len(s.pending))
	for key, snap := range s.pending {
		if match == nil || match(snap.Parent) {
			keys = append(keys, key)
		}
	}
	s.mu.Unlock()
	sort.Strings(keys)
	for _, key := range keys {
		if err := ctx.Err(); err != nil {
			return err
		}
		s.mu.Lock()
		snap, exists := s.pending[key]
		_, deleted := s.deletedTasks[snap.Parent.TaskID]
		_, saving := s.saving[key]
		retry, retrying := s.checkpoints[key]
		blocked := retrying && retry.RunID == snap.RunID && retry.Parent == snap.Parent && (retry.Permanent || retry.Gone || !forceTransient && now().Before(retry.Next))
		if !exists || match != nil && !match(snap.Parent) || snap.Parent.TaskID > 0 && deleted || saving || blocked {
			s.mu.Unlock()
			continue
		}
		s.saving[key] = sideCheckpointSave{Snapshot: snap, Done: make(chan struct{})}
		s.mu.Unlock()
		err := snap.Parent.Validate()
		if err == nil {
			saveCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
			err = save(saveCtx, snap)
			cancel()
		}
		if failure := s.finishCheckpoint(key, snap, err, now()); failure != nil && report != nil {
			report(*failure)
		}
	}
	return ctx.Err()
}

func (s *sideQuestionState) finishCheckpoint(key string, snap sidequestion.Snapshot, err error, now time.Time) *sideCheckpointFailure {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ensureCheckpointMaps()
	if inFlight, ok := s.saving[key]; ok && inFlight.Snapshot.Parent == snap.Parent && inFlight.Snapshot.RunID == snap.RunID && inFlight.Snapshot.Version == snap.Version {
		delete(s.saving, key)
		close(inFlight.Done)
	}
	if _, deleted := s.deletedTasks[snap.Parent.TaskID]; snap.Parent.TaskID > 0 && deleted {
		return nil
	}
	if seen, ok := s.seen[key]; ok && seen[0] > snap.RunID {
		return nil
	}
	if latest, ok := s.latest[key]; ok && latest.RunID == snap.RunID && latest.Parent != snap.Parent {
		return nil
	}
	if err == nil {
		delete(s.checkpoints, key)
		if pending, ok := s.pending[key]; ok && pending.Parent == snap.Parent && pending.RunID == snap.RunID && pending.Version <= snap.Version {
			delete(s.pending, key)
		}
		if latest, ok := s.latest[key]; ok && latest.RunID == snap.RunID && latest.Version == snap.Version {
			delete(s.latest, key)
		}
		return nil
	}
	if errors.Is(err, db.ErrSideParentGone) {
		delete(s.pending, key)
		delete(s.latest, key)
		s.checkpoints[key] = sideCheckpointRetry{RunID: snap.RunID, Parent: snap.Parent, Gone: true}
		return nil
	}
	if pending, ok := s.pending[key]; !ok || pending.RunID < snap.RunID || pending.RunID == snap.RunID && pending.Version < snap.Version {
		s.pending[key] = snap
	}
	previous := s.checkpoints[key]
	failures := 1
	if previous.RunID == snap.RunID && previous.Parent == snap.Parent {
		failures = previous.Failures + 1
	}
	if failures > 32 {
		failures = 32
	}
	permanent := permanentSideCheckpointError(err)
	delay := sideCheckpointBackoff(failures)
	s.checkpoints[key] = sideCheckpointRetry{RunID: snap.RunID, Parent: snap.Parent, Failures: failures, Next: now.Add(delay), Permanent: permanent, ErrorLabel: sideCheckpointErrorLabel(err)}
	return &sideCheckpointFailure{Err: err, Parent: snap.Parent, RunID: snap.RunID, Version: snap.Version, Permanent: permanent, RetryAfter: delay}
}

func sideCheckpointBackoff(failures int) time.Duration {
	delay := time.Second
	for attempt := 1; attempt < failures && delay < 30*time.Second; attempt++ {
		delay *= 2
	}
	if delay > 30*time.Second {
		delay = 30 * time.Second
	}
	return delay
}

func permanentSideCheckpointError(err error) bool {
	if errors.Is(err, sidequestion.ErrInvalidParent) {
		return true
	}
	var pg *pgconn.PgError
	if errors.As(err, &pg) && len(pg.Code) >= 2 {
		switch pg.Code[:2] {
		case "22", "23", "42", "0A": // invalid data, integrity, schema/syntax, unsupported operation
			return true
		}
	}
	var unsupportedType *json.UnsupportedTypeError
	var unsupportedValue *json.UnsupportedValueError
	var syntax *json.SyntaxError
	var marshaler *json.MarshalerError
	return errors.As(err, &unsupportedType) || errors.As(err, &unsupportedValue) || errors.As(err, &syntax) || errors.As(err, &marshaler)
}

// Do not log snapshots, driver Detail fields, query parameters or error strings
// that may contain request content. SQLSTATE/error category is sufficient here.
func sideCheckpointErrorLabel(err error) string {
	if errors.Is(err, sidequestion.ErrInvalidParent) {
		return "invalid checkpoint parent"
	}
	var pg *pgconn.PgError
	if errors.As(err, &pg) {
		valid := len(pg.Code) == 5
		for _, c := range pg.Code {
			if !(c >= '0' && c <= '9' || c >= 'A' && c <= 'Z') {
				valid = false
			}
		}
		if valid {
			return "database SQLSTATE " + pg.Code
		}
		return "database error (SQLSTATE unavailable)"
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return "checkpoint database timeout"
	}
	if errors.Is(err, context.Canceled) {
		return "checkpoint save canceled"
	}
	return fmt.Sprintf("checkpoint storage error (%T)", err)
}
