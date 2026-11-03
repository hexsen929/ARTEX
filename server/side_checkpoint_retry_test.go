package server

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log"
	"strings"
	"sync"
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

func TestSideCheckpointShutdownAttemptsDeferredTransientButNotPermanentOrGone(t *testing.T) {
	state := &sideQuestionState{}
	now := time.Unix(100, 0)
	attempts := map[int64]int{}
	for _, id := range []int64{41, 42, 43} {
		snap := checkpointRetryFixture(1, 1)
		snap.Parent.ConversationID = id
		state.publishCheckpoint(snap)
	}
	save := func(_ context.Context, snap sidequestion.Snapshot) error {
		attempts[snap.Parent.ConversationID]++
		switch snap.Parent.ConversationID {
		case 41:
			return context.DeadlineExceeded
		case 42:
			return &pgconn.PgError{Code: "23514"}
		case 43:
			return db.ErrSideParentGone
		}
		t.Fatal("unexpected checkpoint fixture")
		return nil
	}
	state.flushCheckpoints(func() time.Time { return now }, save, nil)
	now = now.Add(250 * time.Millisecond)
	latest := checkpointRetryFixture(1, 2)
	state.publishCheckpoint(latest)
	state.flushCheckpoints(func() time.Time { return now }, save, nil)
	if attempts[41] != 1 || attempts[42] != 1 || attempts[43] != 1 {
		t.Fatal("normal flush ignored transient backoff or retried a permanent failure")
	}
	state.flushCheckpoints(func() time.Time { return now }, func(_ context.Context, snap sidequestion.Snapshot) error {
		if snap.Parent.ConversationID != 41 || snap.Version != 2 {
			t.Fatalf("shutdown attempted a blocked parent or stale checkpoint: %+v", snap.Parent)
		}
		attempts[41]++
		return nil
	}, nil, true)
	if attempts[41] != 2 || len(state.pending) != 1 || len(state.latest) != 1 || !state.checkpoints[(sidequestion.Parent{ConversationID: 42}).Key()].Permanent {
		t.Fatal("shutdown did not preserve blocked state while persisting the latest transient checkpoint")
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

func taskCheckpointRetryFixture(taskID, intentID int64) sidequestion.Snapshot {
	return sidequestion.Snapshot{Parent: sidequestion.Parent{TaskID: taskID, ExplorationID: taskID + 100, IntentID: intentID}, RunID: 11, Version: 3}
}

func TestSideCheckpointTaskScopedFlushDoesNotWaitForUnrelatedSave(t *testing.T) {
	state := &sideQuestionState{}
	other, target := taskCheckpointRetryFixture(1, 0), taskCheckpointRetryFixture(17, 0)
	state.publishCheckpoint(other)
	state.publishCheckpoint(target)
	started, release, done := make(chan struct{}), make(chan struct{}), make(chan struct{})
	var once sync.Once
	t.Cleanup(func() { once.Do(func() { close(release) }) })
	go func() {
		defer close(done)
		state.flushCheckpoints(time.Now, func(ctx context.Context, snap sidequestion.Snapshot) error {
			if snap.Parent.TaskID != other.Parent.TaskID {
				return errors.New("background must not claim the task while it is queued behind another save")
			}
			close(started)
			select {
			case <-release:
				return context.DeadlineExceeded
			case <-ctx.Done():
				return ctx.Err()
			}
		}, nil)
	}()
	<-started
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	s := &Server{side: state}
	if err := s.stopTaskSideQuestions(ctx, "17"); err != nil {
		t.Fatalf("unrelated persistence held task admission: %v", err)
	}
	calls := 0
	if err := state.flushCheckpointsMatching(ctx, time.Now, func(saveCtx context.Context, snap sidequestion.Snapshot) error {
		calls++
		if snap.Parent != target.Parent {
			t.Fatal("targeted flush touched another task")
		}
		deadline, ok := saveCtx.Deadline()
		parentDeadline, _ := ctx.Deadline()
		if !ok || deadline.After(parentDeadline) {
			t.Fatal("target save escaped caller deadline")
		}
		return nil
	}, nil, func(p sidequestion.Parent) bool { return p.TaskID == 17 }, true); err != nil {
		t.Fatal(err)
	}
	state.mu.Lock()
	_, targetPending := state.pending[target.Parent.Key()]
	_, otherPending := state.pending[other.Parent.Key()]
	state.mu.Unlock()
	if calls != 1 || targetPending || !otherPending {
		t.Fatal("scoped flush did not preserve unrelated context")
	}
	once.Do(func() { close(release) })
	<-done
	if _, pending := state.pending[target.Parent.Key()]; pending {
		t.Fatal("the old global key list reintroduced the task after its scoped save")
	}
}

func TestSideCheckpointScopedFlushUsesRemainingDeadline(t *testing.T) {
	state := &sideQuestionState{}
	for _, iid := range []int64{1, 2} {
		state.publishCheckpoint(taskCheckpointRetryFixture(17, iid))
	}
	ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer cancel()
	calls := 0
	err := state.flushCheckpointsMatching(ctx, time.Now, func(saveCtx context.Context, _ sidequestion.Snapshot) error {
		calls++
		ownDeadline, _ := saveCtx.Deadline()
		callerDeadline, _ := ctx.Deadline()
		if ownDeadline.After(callerDeadline) {
			t.Fatal("database save ignored the archive deadline")
		}
		<-saveCtx.Done()
		return saveCtx.Err()
	}, nil, func(p sidequestion.Parent) bool { return p.TaskID == 17 }, true)
	if !errors.Is(err, context.DeadlineExceeded) || calls != 1 || len(state.pending) != 2 || len(state.saving) != 0 {
		t.Fatalf("deadline did not retain both attempted and unattempted snapshots: err=%v calls=%d pending=%d saving=%d", err, calls, len(state.pending), len(state.saving))
	}
}

func TestSideCheckpointArchiveDoesNotMistakeInFlightSaveForPersistence(t *testing.T) {
	state := &sideQuestionState{}
	snap := taskCheckpointRetryFixture(17, 0)
	state.publishCheckpoint(snap)
	started, release, done := make(chan struct{}), make(chan struct{}), make(chan struct{})
	var once sync.Once
	t.Cleanup(func() { once.Do(func() { close(release) }) })
	go func() {
		defer close(done)
		state.flushCheckpoints(time.Now, func(context.Context, sidequestion.Snapshot) error { close(started); <-release; return nil }, nil)
	}()
	<-started
	s := &Server{side: state, m: &Manager{}}
	ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer cancel()
	if err := s.drainTaskSideQuestions(ctx, "17"); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("archive passed a still-running save: %v", err)
	}
	state.mu.Lock()
	_, pending := state.pending[snap.Parent.Key()]
	_, saving := state.saving[snap.Parent.Key()]
	state.mu.Unlock()
	if !pending || !saving {
		t.Fatal("in-flight checkpoint became invisible to archive")
	}
	once.Do(func() { close(release) })
	<-done
	if len(state.pending) != 0 || len(state.saving) != 0 {
		t.Fatal("successful save did not release pending/in-flight state")
	}
}

func TestSideCheckpointPermanentArchiveErrorIsActionableAndRedacted(t *testing.T) {
	state := &sideQuestionState{}
	snap := taskCheckpointRetryFixture(17, 0)
	state.publishCheckpoint(snap)
	state.flushCheckpoints(time.Now, func(context.Context, sidequestion.Snapshot) error {
		return &pgconn.PgError{Code: "22P02", Message: "private-request-canary", Detail: "secret-credential-canary"}
	}, nil)
	s := &Server{side: state, m: &Manager{}}
	err := s.drainTaskSideQuestions(t.Context(), "17")
	if err == nil || !strings.Contains(err.Error(), "22P02") || !strings.Contains(err.Error(), "已停止自动重试") || !strings.Contains(err.Error(), "归档未执行") || strings.Contains(err.Error(), "请重试") || strings.Contains(err.Error(), "canary") {
		t.Fatalf("permanent failure remained an unhelpful retry instruction or leaked content: %v", err)
	}
}

func TestSideCheckpointFailureLogsSafeParentRunAndVersion(t *testing.T) {
	var output bytes.Buffer
	previous := log.Writer()
	log.SetOutput(&output)
	defer log.SetOutput(previous)
	s := &Server{}
	snap := taskCheckpointRetryFixture(17, 19)
	s.reportSideCheckpointFailure(sideCheckpointFailure{Err: &pgconn.PgError{Code: "22P02", Message: "private-request-canary", Detail: "secret-credential-canary"}, Parent: snap.Parent, RunID: snap.RunID, Version: snap.Version, Permanent: true})
	text := output.String()
	for _, wanted := range []string{snap.Parent.Key(), "task=17", "intent=19", "run=11", "version=3", "SQLSTATE 22P02", "retries stopped"} {
		if !strings.Contains(text, wanted) {
			t.Fatalf("diagnostic lacks safe identity %q: %s", wanted, text)
		}
	}
	if strings.Contains(text, "canary") {
		t.Fatal("request/credential content leaked into checkpoint log")
	}
	if strings.Contains(sideCheckpointErrorLabel(&pgconn.PgError{Code: "22P02-secret-canary"}), "canary") {
		t.Fatal("malformed SQLSTATE leaked arbitrary content")
	}
}

func TestSideCheckpointSyntheticContextsNeverEnterPublisherState(t *testing.T) {
	state := &sideQuestionState{}
	synthetic := checkpointRetryFixture(0, 100)
	state.publishCheckpoint(synthetic)
	if len(state.pending) != 0 || len(state.latest) != 0 || len(state.seen) != 0 || len(state.checkpoints) != 0 {
		t.Fatal("synthetic snapshot entered checkpoint ordering or persistence state")
	}
	real := checkpointRetryFixture(1, 1)
	state.publishCheckpoint(real)
	state.publishCheckpoint(synthetic)
	key := real.Parent.Key()
	if state.pending[key].RunID != 1 || state.latest[key].RunID != 1 || state.seen[key] != [2]int64{1, 1} {
		t.Fatal("synthetic snapshot displaced the real checkpoint")
	}
}
