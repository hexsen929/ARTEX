package db

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"
	"time"
)

// This driver models an archive/restore commit immediately after the hot query
// completes. A repeatable-read transaction retains its initial storage view;
// independent queries (or a read-committed transaction) see the moved row twice
// during archive, or not at all during restore. No real database is required.
type usageSnapshotState struct {
	archived    bool
	failQuery   string
	badArchive  bool
	begins      int
	rollbacks   int
	readOnly    bool
	isolation   driver.IsolationLevel
	queriesInTx int
}

type usageSnapshotConnector struct{ state *usageSnapshotState }

func (c *usageSnapshotConnector) Connect(context.Context) (driver.Conn, error) {
	return &usageSnapshotConn{state: c.state}, nil
}
func (c *usageSnapshotConnector) Driver() driver.Driver { return usageSnapshotDriver{} }

type usageSnapshotDriver struct{}

func (usageSnapshotDriver) Open(string) (driver.Conn, error) {
	return nil, errors.New("use the usage snapshot connector")
}

type usageSnapshotConn struct {
	state    *usageSnapshotState
	active   bool
	snapshot bool
}

func (*usageSnapshotConn) Prepare(string) (driver.Stmt, error) {
	return nil, errors.New("unexpected prepared statement")
}
func (*usageSnapshotConn) Close() error { return nil }
func (c *usageSnapshotConn) Begin() (driver.Tx, error) {
	return c.BeginTx(context.Background(), driver.TxOptions{})
}
func (c *usageSnapshotConn) BeginTx(_ context.Context, opts driver.TxOptions) (driver.Tx, error) {
	c.active = true
	c.snapshot = c.state.archived
	c.state.begins++
	c.state.readOnly, c.state.isolation = opts.ReadOnly, opts.Isolation
	return &usageSnapshotTx{conn: c}, nil
}

type usageSnapshotTx struct{ conn *usageSnapshotConn }

func (tx *usageSnapshotTx) Commit() error {
	tx.conn.active = false
	return nil
}
func (tx *usageSnapshotTx) Rollback() error {
	tx.conn.active = false
	tx.conn.state.rollbacks++
	return nil
}

func (c *usageSnapshotConn) QueryContext(_ context.Context, query string, _ []driver.NamedValue) (driver.Rows, error) {
	if c.active {
		c.state.queriesInTx++
	}
	archived := c.state.archived
	if c.active && c.state.readOnly && c.state.isolation == driver.IsolationLevel(sql.LevelRepeatableRead) {
		archived = c.snapshot
	}
	if strings.Contains(query, "FROM tool_usage") || strings.Contains(query, "FROM skill_usage") {
		if c.state.failQuery == "hot" {
			return nil, errors.New("fixture hot statistics query failed")
		}
		rows := &usageSnapshotRows{onClose: func() { c.state.archived = !c.state.archived }}
		if strings.Contains(query, "FROM tool_usage") {
			rows.columns = []string{"tool_key", "calls"}
			if !archived {
				rows.values = [][]driver.Value{{"fixture_tool", int64(1)}}
			}
		} else {
			rows.columns = []string{"skill", "calls", "tasks", "agents", "last_used"}
			if !archived {
				rows.values = [][]driver.Value{{"fixture-skill", int64(1), int64(1), "worker", time.Unix(100, 0)}}
			}
		}
		return rows, nil
	}
	if strings.Contains(query, "FROM task_archives") {
		if c.state.failQuery == "cold" {
			return nil, errors.New("fixture archived statistics query failed")
		}
		rows := &usageSnapshotRows{columns: []string{"aggregate_stats"}}
		if archived {
			raw, _ := json.Marshal(taskArchiveAggregate{
				Tools:      map[string]int{"fixture_tool": 1},
				SkillStats: []SkillStat{{Skill: "fixture-skill", Calls: 1, Tasks: 1, Agents: []string{"worker"}}},
			})
			if c.state.badArchive {
				raw = []byte(`{`)
			}
			rows.values = [][]driver.Value{{raw}}
		}
		return rows, nil
	}
	return nil, errors.New("unexpected usage snapshot query")
}

type usageSnapshotRows struct {
	columns []string
	values  [][]driver.Value
	index   int
	onClose func()
	closed  bool
}

func (r *usageSnapshotRows) Columns() []string { return r.columns }
func (r *usageSnapshotRows) Close() error {
	if !r.closed && r.onClose != nil {
		r.onClose()
	}
	r.closed = true
	return nil
}
func (r *usageSnapshotRows) Next(dest []driver.Value) error {
	if r.index == len(r.values) {
		return io.EOF
	}
	copy(dest, r.values[r.index])
	r.index++
	return nil
}

func usageSnapshotDB(t *testing.T, state *usageSnapshotState) *DB {
	t.Helper()
	d := sql.OpenDB(&usageSnapshotConnector{state: state})
	d.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = d.Close() })
	return &DB{DB: d}
}

func TestUsageTotalsKeepOneSnapshotDuringArchiveAndRestore(t *testing.T) {
	for _, kind := range []string{"tool", "skill"} {
		for _, initiallyArchived := range []bool{false, true} {
			operation := "archive"
			if initiallyArchived {
				operation = "restore"
			}
			t.Run(kind+"/"+operation, func(t *testing.T) {
				state := &usageSnapshotState{archived: initiallyArchived}
				d := usageSnapshotDB(t, state)
				count := 0
				if kind == "tool" {
					counts, err := d.ToolUsageCounts()
					if err != nil {
						t.Fatal(err)
					}
					count = counts["fixture_tool"]
				} else {
					stats, err := d.SkillStats()
					if err != nil {
						t.Fatal(err)
					}
					for _, stat := range stats {
						if stat.Skill == "fixture-skill" {
							count = stat.Calls
						}
					}
				}
				if count != 1 {
					t.Fatalf("%s counted one moved invocation %d times", operation, count)
				}
				if state.begins != 1 || state.rollbacks != 1 || !state.readOnly || state.isolation != driver.IsolationLevel(sql.LevelRepeatableRead) || state.queriesInTx != 2 {
					t.Fatalf("statistics escaped their read-only repeatable-read snapshot: %+v", state)
				}
			})
		}
	}
}

func TestUsageTotalsDoNotTurnQueryOrArchiveDecodeFailuresIntoZero(t *testing.T) {
	for _, kind := range []string{"tool", "skill"} {
		for _, failure := range []string{"hot", "cold", "malformed_archive"} {
			t.Run(kind+"/"+failure, func(t *testing.T) {
				state := &usageSnapshotState{archived: true, failQuery: failure, badArchive: failure == "malformed_archive"}
				d := usageSnapshotDB(t, state)
				var err error
				if kind == "tool" {
					_, err = d.ToolUsageCounts()
				} else {
					_, err = d.SkillStats()
				}
				if err == nil {
					t.Fatal("statistics failure was silently reported as an empty count")
				}
				if state.rollbacks != 1 {
					t.Fatal("failed statistics read did not release its snapshot")
				}
			})
		}
	}
}
