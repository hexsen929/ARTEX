package server

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/Autumn-27/artex/db"
	"github.com/Autumn-27/artex/sidequestion"
	"github.com/jackc/pgx/v5/pgconn"
)

func checkpointRetryFixture(run, version int64) sidequestion.Snapshot {
	return sidequestion.Snapshot{Parent: sidequestion.Parent{ConversationID: 41}, RunID: run, Version: version}
}

func TestSideCheckpointPermanentFailureIsOncePerRunAndRetainsLatest(t *testing.T) {
	state := &sideQuestionState{}
	now := time.Unix(100, 0)
	attempts, reports := 0, 0
	save := func(context.Context, sidequestion.Snapshot) error {
		attempts++
		return &pgconn.PgError{Code: "23514", Message: "check failed", Detail: "private fixture content"}
	}
	report := func(failure sideCheckpointFailure) {
		reports++
		if !failure.Permanent {
			t.Fatal("constraint failure should stop automatic retry")
		}
	}
	state.publishCheckpoint(checkpointRetryFixture(1, 1))
	state.flushCheckpoints(func() time.Time { return now }, save, report)
	for version := int64(2); version <= 12; version++ {
		now = now.Add(time.Second)
		state.publishCheckpoint(checkpointRetryFixture(1, version))
		state.flushCheckpoints(func() time.Time { return now }, save, report)
	}
	key := checkpointRetryFixture(1, 1).Parent.Key()
	if attempts != 1 || reports != 1 || state.latest[key].Version != 12 || state.pending[key].Version != 12 {
		t.Fatalf("attempts=%d reports=%d latest=%+v pending=%+v", attempts, reports, state.latest[key], state.pending[key])
	}
	// A new main run retries, rather than permanently disabling the conversation.
	state.publishCheckpoint(checkpointRetryFixture(2, 1))
	state.flushCheckpoints(func() time.Time { return now }, save, report)
	if attempts != 2 || reports != 2 {
		t.Fatalf("new run was not attempted: attempts=%d reports=%d", attempts, reports)
	}
}

func TestSideCheckpointTransientBackoffCoalescesNewVersionsAndRecovers(t *testing.T) {
	state := &sideQuestionState{}
	now := time.Unix(100, 0)
	var written []sidequestion.Snapshot
	fail := true
	save := func(_ context.Context, snap sidequestion.Snapshot) error {
		written = append(written, snap)
		if fail {
			return context.DeadlineExceeded
		}
		return nil
	}
	state.publishCheckpoint(checkpointRetryFixture(1, 1))
	state.flushCheckpoints(func() time.Time { return now }, save, nil)
	key := checkpointRetryFixture(1, 1).Parent.Key()
	if state.checkpoints[key].Next != now.Add(time.Second) {
		t.Fatal("first transient retry should wait one second")
	}
	now = now.Add(250 * time.Millisecond)
	state.publishCheckpoint(checkpointRetryFixture(1, 7))
	state.flushCheckpoints(func() time.Time { return now }, save, nil)
	if len(written) != 1 {
		t.Fatal("new stream version bypassed backoff")
	}
	now = now.Add(750 * time.Millisecond)
	state.flushCheckpoints(func() time.Time { return now }, save, nil)
	if len(written) != 2 || written[1].Version != 7 || state.checkpoints[key].Next != now.Add(2*time.Second) {
		t.Fatalf("retry did not coalesce or back off: writes=%+v retry=%+v", written, state.checkpoints[key])
	}
	fail = false
	state.publishCheckpoint(checkpointRetryFixture(1, 8))
	now = now.Add(2 * time.Second)
	state.flushCheckpoints(func() time.Time { return now }, save, nil)
	if len(written) != 3 || written[2].Version != 8 || len(state.pending) != 0 || len(state.latest) != 0 || len(state.checkpoints) != 0 {
		t.Fatal("recovered save did not clear its retry state")
	}
	for _, failures := range []int{6, 32, 100} {
		if got := sideCheckpointBackoff(failures); got != 30*time.Second {
			t.Fatalf("backoff must be bounded: failures=%d delay=%s", failures, got)
		}
	}
}

func TestSideCheckpointOldRunFailureCannotOverwriteNewRun(t *testing.T) {
	for _, failure := range []error{&pgconn.PgError{Code: "23514"}, context.DeadlineExceeded, db.ErrSideParentGone} {
		t.Run(fmt.Sprintf("%T/%v", failure, failure), func(t *testing.T) {
			state := &sideQuestionState{}
			now := time.Unix(100, 0)
			state.publishCheckpoint(checkpointRetryFixture(1, 1))
			state.flushCheckpoints(func() time.Time { return now }, func(context.Context, sidequestion.Snapshot) error {
				// The publisher does not hold commands, so a new run can start
				// while a slow database save from the old run is in progress.
				state.publishCheckpoint(checkpointRetryFixture(2, 1))
				return failure
			}, nil)
			key := checkpointRetryFixture(1, 1).Parent.Key()
			if state.pending[key].RunID != 2 || state.latest[key].RunID != 2 || len(state.checkpoints) != 0 {
				t.Fatal("old completion overwrote, deleted or blocked a new run")
			}
			attempts := 0
			state.flushCheckpoints(func() time.Time { return now }, func(context.Context, sidequestion.Snapshot) error { attempts++; return nil }, nil)
			if attempts != 1 || len(state.latest) != 0 || len(state.pending) != 0 {
				t.Fatal("new run did not persist immediately")
			}
		})
	}
}

func TestSideCheckpointConcurrentPublicationSurvivesSlowOldSave(t *testing.T) {
	state := &sideQuestionState{}
	now := time.Unix(100, 0)
	state.publishCheckpoint(checkpointRetryFixture(1, 1))
	started, release, done := make(chan struct{}), make(chan struct{}), make(chan struct{})
	go func() {
		defer close(done)
		state.flushCheckpoints(func() time.Time { return now }, func(context.Context, sidequestion.Snapshot) error {
			close(started)
			<-release
			return &pgconn.PgError{Code: "23514"}
		}, nil)
	}()
	<-started
	state.publishCheckpoint(checkpointRetryFixture(2, 1))
	close(release)
	<-done
	key := checkpointRetryFixture(1, 1).Parent.Key()
	if state.pending[key].RunID != 2 || state.latest[key].RunID != 2 || len(state.checkpoints) != 0 {
		t.Fatal("concurrent old failure changed new-run state")
	}
	attempts := 0
	state.flushCheckpoints(func() time.Time { return now }, func(context.Context, sidequestion.Snapshot) error { attempts++; return nil }, nil)
	if attempts != 1 || len(state.pending) != 0 || len(state.latest) != 0 {
		t.Fatal("new run did not persist after the old slow write")
	}
}

func TestSideCheckpointDeletedParentDoesNotReviveFromLaterVersions(t *testing.T) {
	state := &sideQuestionState{}
	now := time.Unix(100, 0)
	attempts := 0
	save := func(context.Context, sidequestion.Snapshot) error { attempts++; return db.ErrSideParentGone }
	state.publishCheckpoint(checkpointRetryFixture(1, 1))
	state.flushCheckpoints(func() time.Time { return now }, save, func(sideCheckpointFailure) { t.Fatal("deleted parents should not log persistence failures") })
	for version := int64(2); version < 8; version++ {
		state.publishCheckpoint(checkpointRetryFixture(1, version))
		state.flushCheckpoints(func() time.Time { return now }, save, nil)
	}
	if attempts != 1 || len(state.pending) != 0 || len(state.latest) != 0 {
		t.Fatal("later versions revived a deleted parent")
	}
}

func TestSideCheckpointNewerVersionArrivingDuringSaveIsRetained(t *testing.T) {
	for _, failure := range []error{nil, context.DeadlineExceeded, &pgconn.PgError{Code: "23514"}} {
		state := &sideQuestionState{}
		now := time.Unix(100, 0)
		state.publishCheckpoint(checkpointRetryFixture(1, 1))
		state.flushCheckpoints(func() time.Time { return now }, func(context.Context, sidequestion.Snapshot) error {
			state.publishCheckpoint(checkpointRetryFixture(1, 3))
			return failure
		}, nil)
		key := checkpointRetryFixture(1, 1).Parent.Key()
		if state.pending[key].Version != 3 || state.latest[key].Version != 3 {
			t.Fatal("an in-flight result replaced the newer checkpoint")
		}
		state.publishCheckpoint(checkpointRetryFixture(1, 2))
		if state.pending[key].Version != 3 {
			t.Fatal("an out-of-order publication regressed the checkpoint")
		}
	}
}

func TestInvalidSideCheckpointCanRecoverAfterParentCorrection(t *testing.T) {
	state := &sideQuestionState{}
	now := time.Unix(100, 0)
	invalid := checkpointRetryFixture(1, 1)
	invalid.Parent.TaskID, invalid.Parent.ExplorationID = 17, 18
	state.publishCheckpoint(invalid)
	attempts, reports := 0, 0
	save := func(context.Context, sidequestion.Snapshot) error { attempts++; return nil }
	state.flushCheckpoints(func() time.Time { return now }, save, func(failure sideCheckpointFailure) {
		reports++
		if !failure.Permanent || !errors.Is(failure.Err, sidequestion.ErrInvalidParent) {
			t.Fatal("invalid parent did not produce a permanent metadata failure")
		}
	})
	if attempts != 0 || reports != 1 || len(state.pending) != 1 || len(state.latest) != 1 {
		t.Fatal("invalid metadata was attempted or hidden as persisted")
	}
	corrected := checkpointRetryFixture(1, 2)
	state.publishCheckpoint(corrected)
	state.flushCheckpoints(func() time.Time { return now }, save, nil)
	if attempts != 1 || len(state.pending) != 0 || len(state.checkpoints) != 0 {
		t.Fatal("corrected metadata in the same run failed to recover")
	}
}

func TestPermanentSideCheckpointStillBlocksTaskArchive(t *testing.T) {
	state := &sideQuestionState{}
	now := time.Unix(100, 0)
	snap := sidequestion.Snapshot{Parent: sidequestion.Parent{TaskID: 17, ExplorationID: 18}, RunID: 1, Version: 1}
	state.publishCheckpoint(snap)
	state.flushCheckpoints(func() time.Time { return now }, func(context.Context, sidequestion.Snapshot) error {
		return &pgconn.PgError{Code: "23514"}
	}, nil)
	s := &Server{side: state, m: &Manager{}}
	if err := s.drainTaskSideQuestions(t.Context(), "17"); err == nil {
		t.Fatal("archive barrier treated a permanently failed checkpoint as saved")
	}
}

func TestSideCheckpointClassificationAndLogRedaction(t *testing.T) {
	for _, code := range []string{"23514", "23503", "22023", "42P01", "0A000"} {
		err := fmt.Errorf("outer: %w", &pgconn.PgError{Code: code, Message: "private fixture", Detail: "secret request content"})
		if !permanentSideCheckpointError(err) {
			t.Fatalf("permanent SQLSTATE %s was treated as temporary", code)
		}
		label := sideCheckpointErrorLabel(err)
		if !strings.Contains(label, code) || strings.Contains(label, "private") || strings.Contains(label, "secret") {
			t.Fatalf("unsafe or uninformative failure label: %q", label)
		}
	}
	for _, err := range []error{context.DeadlineExceeded, context.Canceled, &pgconn.PgError{Code: "40001"}, &pgconn.PgError{Code: "40P01"}, &pgconn.PgError{Code: "08006"}, errors.New("temporary private connection detail")} {
		if permanentSideCheckpointError(err) {
			t.Fatalf("temporary failure was quarantined: %T", err)
		}
		if strings.Contains(sideCheckpointErrorLabel(err), "private") {
			t.Fatal("raw failure content entered the diagnostic label")
		}
	}
}
