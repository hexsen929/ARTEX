//go:build ignore

package server

import (
	"context"
	"database/sql"
	"errors"
	"net/url"
	"os"
	"strconv"
	"testing"
	"time"

	"github.com/Autumn-27/artex/db"
	"github.com/jackc/pgx/v5/pgconn"
)

// Connect without NewManager/Open so concurrent suites keep their settings,
// builtin seeds and schema untouched. Every mutation is scoped to this task.
func p1AdmissionProgressFixture(t *testing.T, paused, queued bool) (*Manager, *Task, *db.Task) {
	t.Helper()
	dsn := os.Getenv("ARTEX_PG_DSN")
	if dsn == "" {
		t.Skip("ARTEX_PG_DSN is required for this isolated integration test")
	}
	conn, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	d := &db.DB{DB: conn}
	t.Cleanup(func() { _ = d.Close() })
	if err := d.PingContext(t.Context()); err != nil {
		t.Fatal(err)
	}
	stored, err := d.CreateTask(t.Name(), "resume a bounded check", nil, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if _, err := d.ExecContext(ctx, `DELETE FROM tasks WHERE id=$1`, stored.ID); err != nil {
			t.Errorf("delete fixture task: %v", err)
			return
		}
		if _, err := d.ExecContext(ctx, `DELETE FROM explorations WHERE id=$1`, stored.ExplorationID); err != nil {
			t.Errorf("delete fixture exploration: %v", err)
		}
	})
	if _, err := d.ExecContext(t.Context(), `UPDATE tasks SET status='running',paused=$2,queued=$3,queued_at=CASE WHEN $3 THEN now() ELSE NULL END,queue_mode=CASE WHEN $3 THEN 'resume' ELSE '' END WHERE id=$1`, stored.ID, paused, queued); err != nil {
		t.Fatal(err)
	}
	es := d.Exploration(stored.ExplorationID)
	digest, err := es.ProgressDigest(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	for round := 1; round <= 3; round++ {
		if _, err := d.RecordNoProgressRound(t.Context(), stored.ID, digest, digest, false); err != nil {
			t.Fatal(err)
		}
	}
	if reached, err := d.NoProgressLimitReached(t.Context(), stored.ID, digest); err != nil || !reached {
		t.Fatalf("fixture did not reach durable no-progress limit: reached=%v err=%v", reached, err)
	}
	live := &Task{ID: strconv.FormatInt(stored.ID, 10), ExpID: stored.ExplorationID, Store: es, Status: "running", Paused: paused, Queued: queued}
	m := &Manager{pg: d, tasks: map[string]*Task{live.ID: live}}
	return m, live, stored
}

func p1AssertAdmissionProgress(t *testing.T, m *Manager, task *Task, stored *db.Task, paused, queued, latched bool) {
	t.Helper()
	current, err := m.pg.GetTask(stored.ID)
	if err != nil || current == nil {
		t.Fatalf("read admitted task: task=%v err=%v", current, err)
	}
	live := task.lifecycleSnapshot()
	if current.Paused != paused || current.Queued != queued || live.Paused != paused || live.Queued != queued {
		t.Fatalf("admission state diverged: persisted paused=%v queued=%v; live paused=%v queued=%v", current.Paused, current.Queued, live.Paused, live.Queued)
	}
	digest, err := task.Store.ProgressDigest(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if reached, err := m.pg.NoProgressLimitReached(t.Context(), stored.ID, digest); err != nil || reached != latched {
		t.Fatalf("durable no-progress latch: reached=%v want=%v err=%v", reached, latched, err)
	}
}

func TestP1TaskAdmissionResetsOnlyPausedProgress(t *testing.T) {
	for _, tc := range []struct {
		name        string
		paused      bool
		wasQueued   bool
		queue       bool
		wantLatched bool
	}{
		{name: "user resume starts immediately", paused: true},
		{name: "user resume joins FIFO", paused: true, queue: true},
		{name: "running wakeup preserves budget", wantLatched: true},
		{name: "FIFO promotion preserves budget", wasQueued: true, wantLatched: true},
		{name: "repeated FIFO admission preserves budget", wasQueued: true, queue: true, wantLatched: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m, task, stored := p1AdmissionProgressFixture(t, tc.paused, tc.wasQueued)
			if err := m.ApplyTaskAdmission(task.ID, "running", "running", tc.queue, "resume", tc.wasQueued); err != nil {
				t.Fatal(err)
			}
			p1AssertAdmissionProgress(t, m, task, stored, false, tc.queue, tc.wantLatched)
			if !tc.wantLatched {
				digest, err := task.Store.ProgressDigest(t.Context())
				if err != nil {
					t.Fatal(err)
				}
				if rounds, err := m.pg.RecordNoProgressRound(t.Context(), stored.ID, digest, digest, false); err != nil || rounds != 1 {
					t.Fatalf("resumed work did not get a fresh bounded allowance: rounds=%d err=%v", rounds, err)
				}
			}
		})
	}
}

func TestP1TaskAdmissionGuardPreservesProgress(t *testing.T) {
	m, task, stored := p1AdmissionProgressFixture(t, true, false)
	if err := m.ApplyTaskAdmission(task.ID, "done", "running", false, "resume", false); err == nil {
		t.Fatal("stale admission accepted")
	}
	p1AssertAdmissionProgress(t, m, task, stored, true, false, true)
}

func TestP1TaskAdmissionProgressResetFailureRollsBack(t *testing.T) {
	m, task, stored := p1AdmissionProgressFixture(t, true, false)
	blocker, err := m.pg.BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer blocker.Rollback()
	var lockedID int64
	if err := blocker.QueryRowContext(t.Context(), `SELECT task_id FROM task_progress WHERE task_id=$1 FOR UPDATE`, stored.ID).Scan(&lockedID); err != nil {
		t.Fatal(err)
	}
	boundedDSN, err := url.Parse(os.Getenv("ARTEX_PG_DSN"))
	if err != nil {
		t.Fatal(err)
	}
	params := boundedDSN.Query()
	params.Set("statement_timeout", "250ms")
	boundedDSN.RawQuery = params.Encode()
	boundedConn, err := sql.Open("pgx", boundedDSN.String())
	if err != nil {
		t.Fatal(err)
	}
	defer boundedConn.Close()
	originalDB := m.pg
	m.pg = &db.DB{DB: boundedConn}
	err = m.ApplyTaskAdmission(task.ID, "running", "running", false, "resume", false)
	m.pg = originalDB
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "57014" {
		t.Fatalf("expected the blocked reset to time out admission, got %v", err)
	}
	// The implicit statement transaction must roll back admission too, and no
	// in-memory state may publish before the reset has committed.
	p1AssertAdmissionProgress(t, m, task, stored, true, false, true)
}
