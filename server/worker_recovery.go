package server

import (
	"context"
	"errors"
	"os"
	"sync"
	"time"

	"github.com/Autumn-27/artex/db"
)

type pendingWorkerOutcome struct {
	incomplete                                  bool
	mu                                          sync.Mutex
	taskID, token, state, code, message, worker string
	next                                        time.Time
	journal                                     string
}

type workerRecoverySchedule struct {
	mu   sync.Mutex
	next time.Time
}

// Reconcile persistence independently of model execution. Never reopen an
// orphan automatically: a missing final result may follow a completed side effect.
func (e *Engine) reconcileWorkerOutcomes(parent context.Context, t *Task) {
	if t == nil {
		return
	}
	v, _ := e.recovery.LoadOrStore(t.ID, &workerRecoverySchedule{})
	schedule := v.(*workerRecoverySchedule)
	if !schedule.mu.TryLock() {
		return
	}
	defer schedule.mu.Unlock()
	if time.Now().Before(schedule.next) {
		return
	}
	schedule.next = time.Now().Add(5 * time.Second)
	e.reconcileWorkerOutcomesAt(parent, t, time.Now())
}

func (e *Engine) reconcileWorkerOutcomesAt(parent context.Context, t *Task, now time.Time) {
	if t == nil || t.Store == nil || parent.Err() != nil || e.IsDeleting(t.ID) {
		return
	}
	ctx, cancel := context.WithTimeout(parent, 5*time.Second)
	defer cancel()
	e.outcomes.Range(func(key, value any) bool {
		out := value.(*pendingWorkerOutcome)
		if out.taskID != t.ID {
			return true
		}
		out.mu.Lock()
		defer out.mu.Unlock()
		if time.Now().Before(out.next) {
			return true
		}
		out.next = time.Now().Add(5 * time.Second)
		id := key.(int64)
		err := t.Store.SetIntentRunOutcome(ctx, id, out.token, out.state, out.code, out.message, out.incomplete)
		if err == nil || errors.Is(err, db.ErrIntentStateConflict) {
			if out.journal != "" {
				_ = os.Remove(out.journal)
			}
			e.outcomes.CompareAndDelete(key, out)
			if err == nil {
				e.emitActivity(t, db.Activity{NodeID: &id, Worker: out.worker, Kind: "status", Summary: out.message, Detail: "termination_code=" + out.code})
				t.NotifyDone(id)
			}
		}
		return ctx.Err() == nil
	})
	if ctx.Err() != nil {
		return
	}
	if !e.IsPaused(t.ID) && !e.isSettling(t.ID) && !e.OutcomeRecoveryBlocked(t.ID) {
		limit, delay := e.modelErrorRetryPolicy()
		if ids, err := t.Store.RecoverTransientIntents(ctx, now, limit, delay); err == nil && len(ids) > 0 {
			t.Notify()
		}
	}
	runs, err := t.Store.RunningIntentRuns(ctx)
	if err != nil {
		return
	}
	for _, run := range runs {
		if ctx.Err() != nil || e.IsDeleting(t.ID) {
			return
		}
		if now.Sub(run.UpdatedAt) < 2*time.Minute {
			continue
		}
		if _, pending := e.outcomes.Load(run.ID); pending {
			continue
		}
		e.workMu.Lock()
		live := e.work[run.ID] != nil
		var persistErr error
		if !live && ctx.Err() == nil && !e.IsDeleting(t.ID) {
			persistErr = t.Store.SetIntentRunOutcome(ctx, run.ID, run.Token, "blocked", "execution_state_lost", "未找到运行中的 Worker；执行记录已保留，请核对最后结果后手动继续，系统未重新执行")
		}
		e.workMu.Unlock()
		if !live && persistErr == nil && ctx.Err() == nil {
			id := run.ID
			e.emitActivity(t, db.Activity{NodeID: &id, Worker: "system", Kind: "status", Summary: "已纠正失联 Worker 的运行状态；操作结果须核对后继续", Detail: "termination_code=execution_state_lost"})
		}
	}
}
