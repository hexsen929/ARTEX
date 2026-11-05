package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/Autumn-27/artex/agent"
	"github.com/Autumn-27/artex/db"
)

type workerOutcomeRecord struct {
	IntentID int64  `json:"intent_id"`
	TaskID   string `json:"task_id"`
	Token    string `json:"run_token"`
	State    string `json:"state"`
	Code     string `json:"code"`
	Message  string `json:"message"`
	Worker   string `json:"worker"`
}

func (e *Engine) OutcomeRecoveryBlocked(taskID string) bool {
	_, blocked := e.recoveryBlocked.Load(taskID)
	return blocked
}

// Startup and explicit resume share the same guard. A failed journal replay
// cannot be bypassed by the second startup loop, scheduler, or Engine.Resume.
func (e *Engine) recoverWorkerOutcomesBeforeStart(t *Task) error {
	if err := e.restoreWorkerOutcomeJournal(t); err != nil {
		e.recoveryBlocked.Store(t.ID, true)
		e.Pause(t.ID, agent.AbortPausedOnReload)
		if e.m != nil && !t.lifecycleSnapshot().Paused {
			_ = e.m.ApplyTaskPauseOriginReason(t.ID, "outcome_recovery", false)
		}
		return err
	}
	e.recoveryBlocked.Delete(t.ID)
	return nil
}

func (e *Engine) outcomeJournalDir(taskID string) string {
	if e.m == nil || e.m.dir == "" {
		return ""
	}
	id, err := strconv.ParseInt(taskID, 10, 64)
	if err != nil || id <= 0 {
		return ""
	}
	return filepath.Join(e.m.dir, "worker-outcomes", taskID)
}

func (e *Engine) journalWorkerOutcome(id int64, out *pendingWorkerOutcome) error {
	dir := e.outcomeJournalDir(out.taskID)
	if dir == "" {
		return nil
	}
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	raw, err := json.Marshal(workerOutcomeRecord{id, out.taskID, out.token, out.state, out.code, out.message, out.worker})
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, ".pending-")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err = f.Write(raw); err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	target := filepath.Join(dir, fmt.Sprintf("%d-%s.json", id, out.token))
	if err := os.Rename(f.Name(), target); err != nil {
		return err
	}
	out.journal = target
	return nil
}

// Before startup resets legacy running intents, restore exact pending outcomes.
// Failed writes leave these intents parked, rather than replaying their tools.
func (e *Engine) restoreWorkerOutcomeJournal(t *Task) error {
	dir := e.outcomeJournalDir(t.ID)
	if dir == "" {
		return nil
	}
	files, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	for _, file := range files {
		if file.IsDir() || filepath.Ext(file.Name()) != ".json" {
			continue
		}
		path := filepath.Join(dir, file.Name())
		raw, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		var saved workerOutcomeRecord
		if len(raw) > 16384 || json.Unmarshal(raw, &saved) != nil || saved.TaskID != t.ID || saved.IntentID <= 0 || saved.Token == "" || file.Name() != fmt.Sprintf("%d-%s.json", saved.IntentID, saved.Token) || (saved.State != "done" && saved.State != "blocked" && saved.State != "exhausted") {
			return errors.New("invalid worker outcome journal; runtime not restored")
		}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		err = t.Store.SetIntentRunOutcome(ctx, saved.IntentID, saved.Token, saved.State, saved.Code, saved.Message)
		cancel()
		if err == nil || errors.Is(err, db.ErrIntentStateConflict) {
			_ = os.Remove(path)
			if value, ok := e.outcomes.Load(saved.IntentID); ok && value.(*pendingWorkerOutcome).token == saved.Token {
				e.outcomes.CompareAndDelete(saved.IntentID, value)
			}
			continue
		}
		e.outcomes.Store(saved.IntentID, &pendingWorkerOutcome{taskID: t.ID, token: saved.Token, state: saved.State, code: saved.Code, message: saved.Message, worker: saved.Worker, journal: path})
		return err
	}
	return nil
}

// Archive runs after all writers drained and before its database snapshot. Never
// compact a task while its only final result is still in a local recovery file.
func (e *Engine) prepareWorkerOutcomesForArchive(t *Task) error {
	if err := e.restoreWorkerOutcomeJournal(t); err != nil {
		return err
	}
	var failed error
	e.outcomes.Range(func(key, value any) bool {
		out := value.(*pendingWorkerOutcome)
		if out.taskID != t.ID {
			return true
		}
		out.mu.Lock()
		defer out.mu.Unlock()
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		err := t.Store.SetIntentRunOutcome(ctx, key.(int64), out.token, out.state, out.code, out.message)
		cancel()
		if err != nil && !errors.Is(err, db.ErrIntentStateConflict) {
			failed = err
			return false
		}
		if out.journal != "" {
			_ = os.Remove(out.journal)
		}
		e.outcomes.CompareAndDelete(key, out)
		return true
	})
	return failed
}
