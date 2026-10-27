package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/Autumn-27/artex/db"
	"github.com/Autumn-27/artex/sidequestion"
	"github.com/jackc/pgx/v5/pgconn"
)

type sideCheckpointRetry struct {
	RunID     int64
	Parent    sidequestion.Parent
	Failures  int
	Next      time.Time
	Permanent bool
	Gone      bool
}

type sideCheckpointFailure struct {
	Err        error
	Permanent  bool
	RetryAfter time.Duration
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
}

func (s *sideQuestionState) publishCheckpoint(snap sidequestion.Snapshot) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ensureCheckpointMaps()
	key := snap.Parent.Key()
	old, ok := s.seen[key]
	if ok && (old[0] > snap.RunID || old[0] == snap.RunID && old[1] >= snap.Version) {
		return
	}
	s.seen[key] = [2]int64{snap.RunID, snap.Version}
	if retry, exists := s.checkpoints[key]; exists {
		if retry.RunID != snap.RunID || retry.Parent != snap.Parent {
			// A genuinely new run or corrected parent metadata may recover. A
			// stream's next version alone must not defeat backoff or quarantine.
			delete(s.checkpoints, key)
		} else if retry.Gone {
			return // Do not revive a parent already deleted/archived in this run.
		}
	}
	s.latest[key] = snap
	s.pending[key] = snap
}

// flushCheckpoints keeps blocked/temporarily deferred entries in pending. The
// archive barrier therefore still sees unsaved checkpoints and never mistakes a
// stopped retry for a successful persistence operation.
func (s *sideQuestionState) flushCheckpoints(now func() time.Time, save func(context.Context, sidequestion.Snapshot) error, report func(sideCheckpointFailure)) {
	s.commands.Lock()
	defer s.commands.Unlock()
	s.mu.Lock()
	s.ensureCheckpointMaps()
	due := map[string]sidequestion.Snapshot{}
	currentTime := now()
	for key, snap := range s.pending {
		retry, exists := s.checkpoints[key]
		if exists && retry.RunID == snap.RunID && retry.Parent == snap.Parent && (retry.Permanent || retry.Gone || currentTime.Before(retry.Next)) {
			continue
		}
		due[key] = snap
		delete(s.pending, key)
	}
	s.mu.Unlock()
	for key, snap := range due {
		err := snap.Parent.Validate()
		if err == nil {
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			err = save(ctx, snap)
			cancel()
		}
		failure := s.finishCheckpoint(key, snap, err, now())
		if failure != nil && report != nil {
			report(*failure)
		}
	}
}

func (s *sideQuestionState) finishCheckpoint(key string, snap sidequestion.Snapshot, err error, now time.Time) *sideCheckpointFailure {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ensureCheckpointMaps()
	if seen, ok := s.seen[key]; ok && seen[0] > snap.RunID {
		return nil // An old write result must not quarantine or replace a new run.
	}
	if latest, ok := s.latest[key]; ok && latest.RunID == snap.RunID && latest.Parent != snap.Parent {
		return nil // Corrected parent metadata already superseded this attempt.
	}
	if err == nil {
		delete(s.checkpoints, key)
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
	// Retain the newest version that arrived during the save, not the failed
	// old snapshot. This includes permanent failures: they remain visibly unsaved.
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
	s.checkpoints[key] = sideCheckpointRetry{RunID: snap.RunID, Parent: snap.Parent, Failures: failures, Next: now.Add(delay), Permanent: permanent}
	return &sideCheckpointFailure{Err: err, Permanent: permanent, RetryAfter: delay}
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
		return "database SQLSTATE " + pg.Code
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return "checkpoint database timeout"
	}
	if errors.Is(err, context.Canceled) {
		return "checkpoint save canceled"
	}
	return fmt.Sprintf("checkpoint storage error (%T)", err)
}
