package server

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"testing"

	"github.com/Autumn-27/artex/agent"
	"github.com/Autumn-27/artex/db"
)

func preserveRuntimeMergeWorkerSettings(t *testing.T, m *Manager) {
	t.Helper()
	for _, key := range []string{settingWorkerPools, settingWorkers} {
		value, exists, err := m.pg.GetSetting(key)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			if exists {
				_ = m.pg.SetSetting(key, value)
			} else {
				_ = m.pg.DeleteSetting(key)
			}
		})
	}
}

func TestRuntimeMergeCheckedWorkerSettingsDistinguishAbsentFromUnknown(t *testing.T) {
	m, err := NewManager(t.TempDir(), "")
	if err != nil {
		t.Skipf("postgres unavailable: %v", err)
	}
	t.Cleanup(func() { _ = m.Close() })
	preserveRuntimeMergeWorkerSettings(t, m)
	if err := m.pg.DeleteSetting(settingWorkerPools); err != nil {
		t.Fatal(err)
	}
	if err := m.pg.DeleteSetting(settingWorkers); err != nil {
		t.Fatal(err)
	}
	if pools, err := m.WorkerPoolsChecked(); err != nil || pools == nil || len(pools) != 0 {
		t.Fatalf("absent pools=%v err=%v", pools, err)
	}
	if workers, err := m.WorkersChecked(); err != nil || workers != defaultWorkers {
		t.Fatalf("absent workers=%d err=%v", workers, err)
	}
	for _, value := range []string{"", " ", "null", "{}", "not-json", `[{"profile_id":"7","workers":0}]`, `[{"profile_id":"0","workers":1}]`, `[{"profile_id":"7","workers":1},{"profile_id":"7","workers":1}]`} {
		t.Run("pool_"+value, func(t *testing.T) {
			if err := m.pg.SetSetting(settingWorkerPools, value); err != nil {
				t.Fatal(err)
			}
			if _, err := m.WorkerPoolsChecked(); err == nil {
				t.Fatal("unknown pool was treated as explicit legacy mode")
			}
			if _, err := m.WorkerSlotsForTaskChecked(&Task{}); err == nil {
				t.Fatal("routing silently fell back to legacy")
			}
		})
	}
	if err := m.pg.SetSetting(settingWorkerPools, "[]"); err != nil {
		t.Fatal(err)
	}
	for _, value := range []string{"", " ", "not-an-integer", "0", "-1", strconv.Itoa(db.MaxTaskWorkerCount + 1)} {
		if err := m.pg.SetSetting(settingWorkers, value); err != nil {
			t.Fatal(err)
		}
		if _, err := m.WorkersChecked(); err == nil {
			t.Fatalf("invalid workers %q defaulted", value)
		}
		if _, err := m.WorkerSlotsForTaskChecked(&Task{}); err == nil {
			t.Fatalf("routing defaulted invalid workers %q", value)
		}
	}
	// Only the selected configuration mode is relevant. A valid pinned pool
	// never needs an unrelated legacy count; a task override is explicit legacy.
	if err := m.pg.SetSetting(settingWorkerPools, `[{"profile_id":"7","workers":2}]`); err != nil {
		t.Fatal(err)
	}
	if slots, err := m.WorkerSlotsForTaskChecked(&Task{}); err != nil || len(slots) != 2 || slots[0].ProfileID != 7 {
		t.Fatalf("pinned mode consulted invalid legacy count: %+v %v", slots, err)
	}
	if err := m.pg.SetSetting(settingWorkerPools, "broken"); err != nil {
		t.Fatal(err)
	}
	if slots, err := m.WorkerSlotsForTaskChecked(&Task{WorkerCount: 2}); err != nil || len(slots) != 2 || slots[0].ProfileID != 0 {
		t.Fatalf("explicit task override lost precedence: %+v %v", slots, err)
	}
}

func TestRuntimeMergeInvalidWorkerSettingsStartNoDefaultRoutines(t *testing.T) {
	m, err := NewManager(t.TempDir(), "")
	if err != nil {
		t.Skipf("postgres unavailable: %v", err)
	}
	t.Cleanup(func() { _ = m.Close() })
	preserveRuntimeMergeWorkerSettings(t, m)
	task, err := m.CreateTask("checked worker fixture", "fixture", nil, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = m.DeleteTask(task.ID, DeleteTaskOptions{}) })
	id, _ := strconv.ParseInt(task.ID, 10, 64)
	before, err := m.pg.GetTask(id)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ pool, workers string }{
		{"broken", "3"}, {"null", "3"}, {"[]", "broken"}, {`[{"profile_id":"7","workers":0}]`, "3"},
	} {
		t.Run(tc.pool+tc.workers, func(t *testing.T) {
			if err := m.pg.SetSetting(settingWorkerPools, tc.pool); err != nil {
				t.Fatal(err)
			}
			if err := m.pg.SetSetting(settingWorkers, tc.workers); err != nil {
				t.Fatal(err)
			}
			e := NewEngine(m)
			t.Cleanup(func() { e.StopTask(task.ID) })
			var legacy, pinned int
			e.SetAuthoritativeAgentResolver(func(*Task) (*agent.Planner, *agent.Worker) { legacy++; return nil, nil })
			e.SetWorkerResolver(func(*Task, WorkerSlot) (*agent.Worker, error) {
				pinned++
				return nil, errors.New("fixture should not run")
			})
			if e.ReadyFor(task) {
				t.Fatal("unknown routing configuration reported ready")
			}
			e.Run(context.Background(), task)
			if legacy != 0 || pinned != 0 || e.Started(task.ID) {
				t.Fatalf("invalid settings invoked model resolvers or started task: legacy=%d pinned=%d started=%v", legacy, pinned, e.Started(task.ID))
			}
			e.runtimeMu.Lock()
			routines := len(e.runtimes)
			e.runtimeMu.Unlock()
			if routines != 0 {
				t.Fatal("invalid settings registered runtime routines")
			}
			if _, exists := e.slotPlans.Load(task.ID); exists {
				t.Fatal("invalid settings registered a fallback slot plan")
			}
			after, err := m.pg.GetTask(id)
			if err != nil || after.Status != before.Status || after.Paused != before.Paused || after.Queued != before.Queued {
				t.Fatalf("error path silently changed task lifecycle: before=%+v after=%+v err=%v", before, after, err)
			}
		})
	}
}

func TestRuntimeMergeWorkerSettingsDBFailureIsNotLegacyMode(t *testing.T) {
	m, err := NewManager(t.TempDir(), "")
	if err != nil {
		t.Skipf("postgres unavailable: %v", err)
	}
	t.Cleanup(func() { _ = m.Close() })
	if err := m.pg.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := m.WorkerPoolsChecked(); err == nil {
		t.Fatal("database error became empty pools")
	}
	if _, err := m.WorkersChecked(); err == nil {
		t.Fatal("database error became default worker count")
	}
	e := NewEngine(m)
	t.Cleanup(func() { e.StopTask("fixture") })
	var resolved int
	e.SetAuthoritativeAgentResolver(func(*Task) (*agent.Planner, *agent.Worker) { resolved++; return nil, nil })
	task := &Task{ID: "fixture"}
	if e.ReadyFor(task) {
		t.Fatal("unreadable settings reported ready")
	}
	e.Run(context.Background(), task)
	if resolved != 0 || e.Started(task.ID) {
		t.Fatal("database failure invoked a default model or worker")
	}
}

func TestRuntimeMergeDetachedWorkerDoesNotDefaultUnknownModelPool(t *testing.T) {
	m, err := NewManager(t.TempDir(), "")
	if err != nil {
		t.Skipf("postgres unavailable: %v", err)
	}
	t.Cleanup(func() { _ = m.Close() })
	preserveRuntimeMergeWorkerSettings(t, m)
	task, err := m.CreateTask("detached checked fixture", "fixture", nil, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = m.DeleteTask(task.ID, DeleteTaskOptions{}) })
	intent, err := task.Store.AddIntent(map[string]any{"text": "fixture"}, 0, nil, "fixture")
	if err != nil {
		t.Fatal(err)
	}
	if ok, err := task.Store.ClaimIntent(intent, "work#4"); err != nil || !ok {
		t.Fatalf("claim: %v %v", ok, err)
	}
	if ok, err := task.Store.CompareAndSetIntentState(intent, "running", "paused"); err != nil || !ok {
		t.Fatalf("pause: %v %v", ok, err)
	}
	if err := m.pg.SetSetting(settingWorkerPools, "broken"); err != nil {
		t.Fatal(err)
	}
	e := NewEngine(m)
	var resolved int
	e.SetAuthoritativeAgentResolver(func(*Task) (*agent.Planner, *agent.Worker) { resolved++; return nil, nil })
	if err := e.runDetachedIntent(context.Background(), task, intent, "fixture", "fixture", "fixture"); err == nil || !strings.Contains(err.Error(), "worker configuration") {
		t.Fatalf("detached run did not reject unreadable pool: %v", err)
	}
	if resolved != 0 {
		t.Fatal("detached run used the default resolver")
	}
	// A valid pinned plan with an unknown owner must not choose a different
	// pool member or fall back to an unpinned task/global model.
	e.slotPlans.Store(task.ID, map[string]int64{"work#1": 7})
	if err := e.runDetachedIntent(context.Background(), task, intent, "fixture", "fixture", "fixture"); err == nil {
		t.Fatal("unknown owner defaulted to another model")
	}
	node, err := task.Store.GetNode(intent)
	if err != nil || node.State != "paused" || resolved != 0 {
		t.Fatalf("detached rejection changed paused intent or model: %+v %v resolved=%d", node, err, resolved)
	}
}

func TestRuntimeMergeClearingPoolCannotHideInvalidLegacyCount(t *testing.T) {
	m, err := NewManager(t.TempDir(), "")
	if err != nil {
		t.Skipf("postgres unavailable: %v", err)
	}
	t.Cleanup(func() { _ = m.Close() })
	preserveRuntimeMergeWorkerSettings(t, m)
	const oldPools = "broken"
	if err := m.pg.SetSetting(settingWorkerPools, oldPools); err != nil {
		t.Fatal(err)
	}
	if err := m.pg.SetSetting(settingWorkers, "invalid"); err != nil {
		t.Fatal(err)
	}
	if err := m.SetWorkerPools(nil); err == nil {
		t.Fatal("clear silently replaced invalid legacy count")
	}
	if raw, _, _ := m.pg.GetSetting(settingWorkerPools); raw != oldPools {
		t.Fatal("failed clear rewrote pool settings")
	}
	if err := m.SetWorkerPools(nil, 2); err != nil {
		t.Fatal(err)
	}
	if pools, err := m.WorkerPoolsChecked(); err != nil || len(pools) != 0 {
		t.Fatalf("explicit repair failed: %+v %v", pools, err)
	}
	if workers, err := m.WorkersChecked(); err != nil || workers != 2 {
		t.Fatalf("explicit repair count=%d err=%v", workers, err)
	}
	for _, n := range []int{0, -1, db.MaxTaskWorkerCount + 1} {
		if err := m.SetWorkers(n); err == nil {
			t.Fatalf("invalid worker count %d saved", n)
		}
	}
}
