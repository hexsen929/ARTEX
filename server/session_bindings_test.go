package server

import (
	"context"
	"strconv"
	"testing"
	"time"

	"github.com/Autumn-27/artex/agent"
	"github.com/Autumn-27/artex/db"
	"github.com/Autumn-27/artex/session"
)

func intranetFixtureTask(t *testing.T, pg *db.DB, sources ...int64) *db.Task {
	t.Helper()
	task, err := pg.CreateTaskWithOptions("intranet fixture", "fixture", db.TaskCreateOptions{SourceTaskIDs: sources})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = pg.Exec(`DELETE FROM tasks WHERE id=$1`, task.ID)
		_, _ = pg.Exec(`DELETE FROM explorations WHERE id=$1`, task.ExplorationID)
	})
	return task
}

type taskSessionFixture struct{ id int64 }

func (s *taskSessionFixture) ID() int64                  { return s.id }
func (s *taskSessionFixture) Kind() string               { return "fixture" }
func (s *taskSessionFixture) Test(context.Context) error { return nil }
func (s *taskSessionFixture) Exec(context.Context, string, time.Duration) (string, string, error) {
	return "", "", nil
}
func (s *taskSessionFixture) ReadFile(context.Context, string) ([]byte, error) { return nil, nil }
func (s *taskSessionFixture) WriteFile(context.Context, string, []byte) error  { return nil }
func (s *taskSessionFixture) Close(context.Context) error                      { return nil }

func TestSessionControlRequiresExplicitTaskBinding(t *testing.T) {
	pg := openIntranetTestDB(t)
	parent := intranetFixtureTask(t, pg)
	child, other := intranetFixtureTask(t, pg, parent.ID), intranetFixtureTask(t, pg)
	if err := pg.SetParentRef(child.ID, strconv.FormatInt(parent.ID, 10)); err != nil {
		t.Fatal(err)
	}
	store := db.NewSessionStore(pg, []byte("fixture-session-binding-key"))
	ctx := context.Background()
	id, err := store.Create(ctx, &db.SessionRecord{Kind: "http_php", URL: "http://fixture.invalid/s.php", Lang: "php", Status: db.SessionAlive, CreatedByTask: parent.ID})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Delete(context.Background(), id) })
	s := &Server{ctx: ctx, sessStore: store, sessReg: session.NewRegistry()}
	s.sessReg.Add(&taskSessionFixture{id: id})
	for _, task := range []*db.Task{child, other} {
		if _, err = s.lookupTaskSession(agent.WithRunInfo(ctx, agent.RunInfo{TaskID: task.ID}), id); err == nil {
			t.Fatal("a parent reference or guessed session ID granted remote control")
		}
	}
	if _, err = s.lookupTaskSession(agent.WithRunInfo(ctx, agent.RunInfo{TaskID: parent.ID}), id); err != nil {
		t.Fatal(err)
	}
	if err = store.BindTask(ctx, id, child.ID); err != nil {
		t.Fatal(err)
	}
	if _, err = s.lookupTaskSession(agent.WithRunInfo(ctx, agent.RunInfo{TaskID: child.ID}), id); err != nil {
		t.Fatal(err)
	}
	if !s.taskIntranet(child.ID) {
		t.Fatal("handoff child did not recognize its explicitly bound session")
	}
	if items := doListItems(t, s.intranetListSessions, "/api/sessions?task_id="+strconv.FormatInt(child.ID, 10)); len(items) != 1 {
		t.Fatal("bound session missing from child workbench")
	}
	if err = s.checkTaskIntranetArchive(ctx, child.ID); err == nil {
		t.Fatal("live shared session did not block archive")
	}
	if err = store.UpdateStatus(ctx, id, db.SessionDead); err != nil {
		t.Fatal(err)
	}
	if err = s.checkTaskIntranetArchive(ctx, child.ID); err != nil {
		t.Fatal(err)
	}
}

func TestReverseListenerCannotBeReassignedToAnotherTask(t *testing.T) {
	h := &PenelopeHandler{running: true, taskID: 1, listenPort: 22000, listenIface: "127.0.0.1"}
	if err := h.ensure(context.Background(), 22000, "127.0.0.1", 2, 3); err == nil {
		t.Fatal("listener ownership changed between tasks")
	}
	if h.taskID != 1 {
		t.Fatal("listener owner was modified")
	}
}
