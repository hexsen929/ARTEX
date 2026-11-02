package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Autumn-27/artex/sidequestion"
	"github.com/jackc/pgx/v5/pgconn"
)

func newTaskDeleteCheckpointFixture(t *testing.T) (*Server, *Task, *Task) {
	t.Helper()
	m, err := NewManager(t.TempDir(), "")
	if err != nil {
		t.Skipf("postgres unavailable (%v) - skipping", err)
	}
	t.Cleanup(func() { _ = m.Close() })
	own, err := m.CreateTask("checkpoint delete fixture", "delete only this task", nil, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	other, err := m.CreateTask("unrelated checkpoint fixture", "keep this task", nil, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = m.DeleteTask(own.ID, DeleteTaskOptions{})
		_, _ = m.DeleteTask(other.ID, DeleteTaskOptions{})
	})
	return &Server{m: m, engine: NewEngine(m), side: &sideQuestionState{}}, own, other
}

func taskDeleteCheckpoint(t *testing.T, task *Task, runID int64) sidequestion.Snapshot {
	t.Helper()
	id, err := strconv.ParseInt(task.ID, 10, 64)
	if err != nil {
		t.Fatal(err)
	}
	return sidequestion.Snapshot{Parent: sidequestion.Parent{TaskID: id, ExplorationID: task.ExpID}, RunID: runID, Version: 3}
}

func quarantineDeleteCheckpoints(state *sideQuestionState, snapshots ...sidequestion.Snapshot) {
	for _, snapshot := range snapshots {
		state.publishCheckpoint(snapshot)
	}
	state.flushCheckpoints(time.Now, func(context.Context, sidequestion.Snapshot) error {
		return &pgconn.PgError{Code: "22P02", Message: "fixture invalid JSON", Detail: "private checkpoint fixture must not be logged"}
	}, nil)
}

func callCheckpointTaskDelete(s *Server, taskID string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodDelete, "/api/tasks/"+taskID, strings.NewReader("{}"))
	req.SetPathValue("id", taskID)
	rec := httptest.NewRecorder()
	s.pgDeleteTask(rec, req)
	return rec
}

func assertDeletedCheckpointAbsent(t *testing.T, state *sideQuestionState, key string) {
	t.Helper()
	state.mu.Lock()
	defer state.mu.Unlock()
	_, pending := state.pending[key]
	_, latest := state.latest[key]
	_, retry := state.checkpoints[key]
	_, seen := state.seen[key]
	if pending || latest || retry || seen {
		t.Fatalf("committed deletion retained or revived checkpoint: pending=%v latest=%v retry=%v seen=%v", pending, latest, retry, seen)
	}
}

func TestTaskDeleteCheckpointPermanentFailureDoesNotBlockExplicitDeletion(t *testing.T) {
	s, own, other := newTaskDeleteCheckpointFixture(t)
	mine, theirs := taskDeleteCheckpoint(t, own, 1), taskDeleteCheckpoint(t, other, 2)
	quarantineDeleteCheckpoints(s.side, mine, theirs)
	if !s.side.checkpoints[mine.Parent.Key()].Permanent {
		t.Fatal("fixture did not create a permanently unsaved checkpoint")
	}

	rec := callCheckpointTaskDelete(s, own.ID)
	if rec.Code != http.StatusOK {
		t.Fatalf("explicit deletion was blocked by an unsaved checkpoint: status=%d body=%s", rec.Code, rec.Body.String())
	}
	var result DeleteTaskResult
	if err := json.Unmarshal(rec.Body.Bytes(), &result); err != nil || result.Deleted != own.ID {
		t.Fatalf("unexpected deletion result: %+v %v", result, err)
	}
	if row, err := s.m.pg.GetTask(mine.Parent.TaskID); err != nil || row != nil {
		t.Fatalf("task database deletion did not commit: row=%+v err=%v", row, err)
	}
	if row, err := s.m.pg.GetTask(theirs.Parent.TaskID); err != nil || row == nil {
		t.Fatalf("unrelated task was removed: row=%+v err=%v", row, err)
	}
	assertDeletedCheckpointAbsent(t, s.side, mine.Parent.Key())
	if _, deleted := s.side.deletedTasks[mine.Parent.TaskID]; !deleted {
		t.Fatal("committed deletion did not reject future publications for this task")
	}
	if s.side.pending[theirs.Parent.Key()].RunID != theirs.RunID || !s.side.checkpoints[theirs.Parent.Key()].Permanent {
		t.Fatal("deleting one task changed another task's checkpoint")
	}

	late := mine
	late.RunID, late.Version = 99, 1
	s.side.publishCheckpoint(late)
	if failure := s.side.finishCheckpoint(mine.Parent.Key(), mine, &pgconn.PgError{Code: "22P02"}, time.Now()); failure != nil {
		t.Fatal("late save failure revived or reported a deleted task checkpoint")
	}
	assertDeletedCheckpointAbsent(t, s.side, mine.Parent.Key())
}

func TestTaskDeleteCheckpointDatabaseFailurePreservesUnsavedState(t *testing.T) {
	s, own, other := newTaskDeleteCheckpointFixture(t)
	mine, theirs := taskDeleteCheckpoint(t, own, 1), taskDeleteCheckpoint(t, other, 2)
	quarantineDeleteCheckpoints(s.side, mine, theirs)
	trigger := fmt.Sprintf("checkpoint_delete_failure_%d", mine.Parent.TaskID)
	function := trigger + "_fn"
	t.Cleanup(func() {
		_, _ = s.m.pg.Exec(fmt.Sprintf(`DROP TRIGGER IF EXISTS %s ON tasks`, trigger))
		_, _ = s.m.pg.Exec(fmt.Sprintf(`DROP FUNCTION IF EXISTS %s()`, function))
	})
	if _, err := s.m.pg.Exec(fmt.Sprintf(`CREATE FUNCTION %s() RETURNS trigger LANGUAGE plpgsql AS $body$
BEGIN RAISE EXCEPTION 'checkpoint fixture task deletion rejected'; END $body$`, function)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.m.pg.Exec(fmt.Sprintf(`CREATE TRIGGER %s BEFORE DELETE ON tasks
FOR EACH ROW WHEN (OLD.id=%d) EXECUTE FUNCTION %s()`, trigger, mine.Parent.TaskID, function)); err != nil {
		t.Fatal(err)
	}

	rec := callCheckpointTaskDelete(s, own.ID)
	if rec.Code != http.StatusInternalServerError || !strings.Contains(rec.Body.String(), "checkpoint fixture task deletion rejected") {
		t.Fatalf("did not reach the injected database failure: status=%d body=%s", rec.Code, rec.Body.String())
	}
	if row, err := s.m.pg.GetTask(mine.Parent.TaskID); err != nil || row == nil {
		t.Fatalf("failed deletion lost its database task: row=%+v err=%v", row, err)
	}
	if _, registered := s.m.Task(own.ID); !registered || s.engine.IsDeleting(own.ID) || s.engine.IsPaused(own.ID) {
		t.Fatal("failed deletion did not restore task registry/admission state")
	}
	for _, snapshot := range []sidequestion.Snapshot{mine, theirs} {
		key := snapshot.Parent.Key()
		if s.side.pending[key].Version != snapshot.Version || s.side.latest[key].RunID != snapshot.RunID || !s.side.checkpoints[key].Permanent {
			t.Fatal("failed deletion discarded unsaved checkpoint state")
		}
		if _, deleted := s.side.deletedTasks[snapshot.Parent.TaskID]; deleted {
			t.Fatal("failed deletion installed a permanent task tombstone")
		}
	}
	mine.Version++
	s.side.publishCheckpoint(mine)
	if s.side.pending[mine.Parent.Key()].Version != mine.Version {
		t.Fatal("failed deletion blocked subsequent checkpoint publication")
	}
}

func TestTaskDeleteCheckpointStopWaitsForActualSideRunExit(t *testing.T) {
	state := &sideQuestionState{runs: map[string]sideRun{}}
	mine := sidequestion.Snapshot{Parent: sidequestion.Parent{TaskID: 28, ExplorationID: 28}, RunID: 1, Version: 1}
	state.publishCheckpoint(mine)
	targetCtx, targetCancel := context.WithCancel(t.Context())
	defer targetCancel()
	otherCtx, otherCancel := context.WithCancel(t.Context())
	defer otherCancel()
	done := make(chan struct{})
	state.runs["target"] = sideRun{parent: mine.Parent, cancel: targetCancel, done: done}
	state.runs["other"] = sideRun{parent: sidequestion.Parent{TaskID: 29, ExplorationID: 29}, cancel: otherCancel, done: make(chan struct{})}
	s := &Server{side: state}
	ctx, cancel := context.WithTimeout(t.Context(), 25*time.Millisecond)
	defer cancel()
	if err := s.stopTaskSideQuestions(ctx, "28"); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("stop returned before the actual run exited: %v", err)
	}
	if targetCtx.Err() == nil || otherCtx.Err() != nil {
		t.Fatal("stop failed to cancel its target or cancelled an unrelated side run")
	}
	if state.pending[mine.Parent.Key()].Version != 1 || state.latest[mine.Parent.Key()].Version != 1 {
		t.Fatal("a timed-out drain discarded its task's unsaved checkpoint")
	}
	if _, deleted := state.deletedTasks[int64(28)]; deleted {
		t.Fatal("an uncommitted drain marked the task deleted")
	}
	close(done)
	if err := s.stopTaskSideQuestions(t.Context(), "28"); err != nil {
		t.Fatal("closed side run did not drain", err)
	}
	if len(state.pending) != 1 {
		t.Fatal("stopping a side run persisted or discarded its checkpoint")
	}
}

func TestTaskDeleteCheckpointLateInFlightSaveCannotReviveCommittedDeletion(t *testing.T) {
	state := &sideQuestionState{}
	mine := sidequestion.Snapshot{Parent: sidequestion.Parent{TaskID: 28, ExplorationID: 28}, RunID: 1, Version: 1}
	theirs := sidequestion.Snapshot{Parent: sidequestion.Parent{TaskID: 29, ExplorationID: 29}, RunID: 2, Version: 1}
	quarantineDeleteCheckpoints(state, theirs)
	state.publishCheckpoint(mine)
	started, release, flushed := make(chan struct{}), make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	releaseSave := func() { releaseOnce.Do(func() { close(release) }) }
	defer releaseSave()
	go func() {
		defer close(flushed)
		state.flushCheckpoints(time.Now, func(context.Context, sidequestion.Snapshot) error {
			close(started)
			<-release
			return &pgconn.PgError{Code: "22P02"}
		}, nil)
	}()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("checkpoint save did not enter the controlled in-flight state")
	}
	s := &Server{side: state}
	committed := make(chan struct{})
	go func() { s.commitTaskSideQuestionDelete("28"); close(committed) }()
	select {
	case <-committed:
	case <-time.After(time.Second):
		releaseSave()
		<-flushed
		t.Fatal("committed deletion waited on an unrelated persistence mutex")
	}
	mine.RunID = 99
	state.publishCheckpoint(mine)
	releaseSave()
	select {
	case <-flushed:
	case <-time.After(time.Second):
		t.Fatal("controlled late save did not finish")
	}
	assertDeletedCheckpointAbsent(t, state, mine.Parent.Key())
	if state.pending[theirs.Parent.Key()].RunID != theirs.RunID || !state.checkpoints[theirs.Parent.Key()].Permanent {
		t.Fatal("late completion or task cleanup changed another task's checkpoint")
	}
}
