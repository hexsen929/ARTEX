package server

import (
	"context"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Autumn-27/artex/agent"
	"github.com/Autumn-27/artex/db"
	"github.com/Autumn-27/norma/llm"
	actool "github.com/Autumn-27/norma/tool"
)

type findingCheckPauseFixture struct {
	s       *Server
	task    *Task
	finding int64
	node    int64
	entered chan context.Context
	calls   atomic.Int32
}

func newFindingCheckPauseFixture(t *testing.T) *findingCheckPauseFixture {
	t.Helper()
	pg := openIntranetTestDB(t)
	row := intranetFixtureTask(t, pg)
	task := &Task{ID: strconv.FormatInt(row.ID, 10), Store: pg.Exploration(row.ExplorationID), Status: "running"}
	ctx, cancel := context.WithCancel(context.Background())
	s := &Server{ctx: ctx, m: &Manager{pg: pg, dir: t.TempDir(), tasks: map[string]*Task{task.ID: task}},
		chatBusy: map[string]bool{}, chatCancel: map[string]context.CancelCauseFunc{}}
	s.engine = NewEngine(s.m)
	s.engine.started.Store(task.ID, true) // fixture loops: never start real workers
	if err := s.seedFindingVerifier(); err != nil {
		t.Fatal(err)
	}
	ag, err := pg.GetAgentByKey(db.FindingVerifierAgentKey)
	if err != nil || ag == nil {
		t.Fatal("fixture verifier missing")
	}
	if _, err := pg.Exec(`UPDATE agents SET enabled=true,llm_profile_id=NULL WHERE id=$1`, ag.ID); err != nil {
		t.Fatal(err)
	}
	oldMode, hadMode, err := pg.GetSetting(settingFindingVerifyMode)
	if err != nil {
		t.Fatal(err)
	}
	if err := pg.SetSetting(settingFindingVerifyMode, "auto"); err != nil {
		t.Fatal(err)
	}
	node, err := task.Store.AddNode(db.KindFinding, map[string]any{"summary": "pause fixture"}, 0, "confirmed", "fixture", nil)
	if err != nil {
		t.Fatal(err)
	}
	fid, err := pg.AddFinding(row.ID, node, "pause-fixture", "fixture finding", "low", "fixture summary", "original fixture evidence", "fixture", nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pg.Exec(`UPDATE findings SET verification_status='confirmed',verification_note='manual fixture decision' WHERE id=$1`, fid); err != nil {
		t.Fatal(err)
	}
	f := &findingCheckPauseFixture{s: s, task: task, finding: fid, node: node, entered: make(chan context.Context, 8)}
	oldAugment, oldResolve, oldPrompt := agent.ToolAugment, agent.ToolResolve, agent.PromptOverride
	agent.ToolAugment = func(context.Context, string) ([]actool.CoreTool, agent.DeferredInfo, func()) {
		return nil, agent.DeferredInfo{}, func() {}
	}
	agent.ToolResolve = nil
	agent.PromptOverride = func(string) (string, bool) { return "Fixture verifier; do not invoke tools.", true }
	setRetestProvider(s, retestProvider{complete: func(ctx context.Context, _ llm.CompletionRequest) (llm.Message, string, llm.Usage, error) {
		if err := ctx.Err(); err != nil {
			return llm.Message{}, "", llm.Usage{}, err
		}
		f.calls.Add(1)
		select {
		case f.entered <- ctx:
		default:
		}
		<-ctx.Done()
		return llm.Message{}, "", llm.Usage{}, ctx.Err()
	}})
	t.Cleanup(func() {
		cancel()
		waitRetestIdle(t, s)
		agent.ToolAugment, agent.ToolResolve, agent.PromptOverride = oldAugment, oldResolve, oldPrompt
		_, _ = pg.Exec(`DELETE FROM conversations WHERE id IN (SELECT conversation_id FROM finding_checks WHERE finding_id=$1)`, fid)
		_, _ = pg.DeleteFinding(fid)
		_, _ = pg.Exec(`UPDATE agents SET enabled=$2,llm_profile_id=$3 WHERE id=$1`, ag.ID, ag.Enabled, ag.LLMProfileID)
		if hadMode {
			_ = pg.SetSetting(settingFindingVerifyMode, oldMode)
		} else {
			_ = pg.DeleteSetting(settingFindingVerifyMode)
		}
	})
	return f
}

func (f *findingCheckPauseFixture) waitProvider(t *testing.T) context.Context {
	t.Helper()
	select {
	case ctx := <-f.entered:
		return ctx
	case <-time.After(3 * time.Second):
		t.Fatal("fixture provider was not entered")
		return nil
	}
}

func (f *findingCheckPauseFixture) pauseCommitted(t *testing.T) {
	t.Helper()
	f.s.concMu.Lock()
	defer f.s.concMu.Unlock()
	f.s.engine.Pause(f.task.ID, agent.AbortPausedByUser)
	if err := f.s.m.ApplyTaskPauseOrigin(f.task.ID, false); err != nil {
		t.Fatal(err)
	}
	if err := f.s.cancelTaskFindingChecks(f.task.ID, agent.AbortChatPausedWithTask); err != nil {
		t.Fatal(err)
	}
}

func TestAutomaticFindingChecksSkipInactiveParentTasks(t *testing.T) {
	f := newFindingCheckPauseFixture(t)
	for _, test := range []struct {
		name, status   string
		paused, queued bool
		started        bool
		enginePaused   bool
		deleting       bool
		missing        bool
	}{
		{name: "paused", status: "running", paused: true, started: true},
		{name: "queued", status: "running", queued: true, started: true},
		{name: "paused-status", status: "paused", started: true},
		{name: "not-started", status: "running"},
		{name: "engine-paused", status: "running", started: true, enginePaused: true},
		{name: "done", status: "done", started: true},
		{name: "failed", status: "failed", started: true},
		{name: "timeout", status: "timeout", started: true},
		{name: "deleting", status: "running", started: true, deleting: true},
		{name: "missing", status: "running", started: true, missing: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			f.task.updateLifecycle(func(state *taskLifecycleState) {
				state.Status, state.Paused, state.Queued = test.status, test.paused, test.queued
			})
			f.s.engine.started.Delete(f.task.ID)
			f.s.engine.paused.Delete(f.task.ID)
			if test.started {
				f.s.engine.started.Store(f.task.ID, true)
			}
			if test.enginePaused {
				f.s.engine.paused.Store(f.task.ID, true)
			}
			if test.deleting {
				f.s.engine.BeginDelete(f.task.ID)
				defer f.s.engine.AbortDelete(f.task.ID, true)
			}
			if test.missing {
				f.s.m.mu.Lock()
				delete(f.s.m.tasks, f.task.ID)
				f.s.m.mu.Unlock()
				defer func() { f.s.m.mu.Lock(); f.s.m.tasks[f.task.ID] = f.task; f.s.m.mu.Unlock() }()
			}
			check, created, err := f.s.requestAutomaticFindingCheck(f.finding, db.FindingCheckKindVerify)
			if err != nil || check != nil || created {
				t.Fatalf("inactive parent admitted automatic verification: created=%v err=%v", created, err)
			}
			f.s.onFindingMerged(f.finding)
			f.s.dispatchFindingVerify(db.TaskEvent{NodeID: f.node})
			checks, err := f.s.m.pg.ListFindingChecks(f.finding)
			if err != nil || len(checks) != 0 || f.calls.Load() != 0 {
				t.Fatal("automatic trigger created a check or invoked a provider for an inactive parent")
			}
		})
	}
}

func TestPauseCancelsRunningFindingCheckWithoutApplyingStagedVerdict(t *testing.T) {
	f := newFindingCheckPauseFixture(t)
	check, created, err := f.s.requestAutomaticFindingCheck(f.finding, db.FindingCheckKindVerify)
	if err != nil || !created || check == nil || check.ConversationID == nil {
		t.Fatalf("active fixture task did not admit verification: %v", err)
	}
	runContext := f.waitProvider(t)
	if err := f.s.m.pg.RecordFindingCheckResult(f.s.ctx, *check.ConversationID, "verified", "staged fixture result", "staged fixture proof"); err != nil {
		t.Fatal(err)
	}
	f.pauseCommitted(t)
	select {
	case <-runContext.Done():
	case <-time.After(time.Second):
		t.Fatal("parent pause left verification provider running")
	}
	waitRetestIdle(t, f.s)
	if err := f.s.m.pg.FinishFindingCheck(check.ID, "completed", "late completion fixture"); err != nil {
		t.Fatal(err)
	}
	checks, err := f.s.m.pg.ListFindingChecks(f.finding)
	if err != nil || len(checks) != 1 || checks[0].Status != "stopped" {
		t.Fatal("paused verification was not sealed as stopped")
	}
	finding, err := f.s.m.pg.GetFinding(f.finding)
	if err != nil || finding == nil || finding.VerificationStatus != "confirmed" || finding.VerificationNote != "manual fixture decision" || finding.Evidence != "original fixture evidence" || finding.Status != db.FindingPending {
		t.Fatal("pause or a late completion changed source evidence or the human decision")
	}
	if _, ok := f.s.findingCheckTasks.Load(*check.ConversationID); ok {
		t.Fatal("finished verification retained runtime task attribution")
	}
}

func TestFindingCheckPauseSealsOrphansButKeepsHistoryExplanations(t *testing.T) {
	f := newFindingCheckPauseFixture(t)
	check, conv, created, err := f.s.m.pg.CreateFindingCheck(f.s.ctx, f.finding, db.FindingCheckKindVerify)
	if err != nil || !created {
		t.Fatal("could not create orphan fixture")
	}
	f.pauseCommitted(t)
	checks, err := f.s.m.pg.ListFindingChecks(f.finding)
	if err != nil || len(checks) != 1 || checks[0].Status != "stopped" || checks[0].ID != check.ID {
		t.Fatal("pending check without a cancellation function was left active")
	}
	historyCtx, cancel := context.WithCancelCause(context.Background())
	defer cancel(agent.AbortChatTurnFinished)
	f.s.chatCancel[f.s.convBusyKey(conv.ID)] = cancel
	f.s.concMu.Lock()
	err = f.s.cancelTaskFindingChecks(f.task.ID, agent.AbortChatPausedWithTask)
	f.s.concMu.Unlock()
	if err != nil || historyCtx.Err() != nil {
		t.Fatal("pause cancelled an explicit explanation of an already sealed historical result")
	}
	delete(f.s.chatCancel, f.s.convBusyKey(conv.ID))
}

func TestAutomaticFindingCheckImmediatePauseFindsRegisteredCancellation(t *testing.T) {
	f := newFindingCheckPauseFixture(t)
	check, created, err := f.s.requestAutomaticFindingCheck(f.finding, db.FindingCheckKindVerify)
	if err != nil || !created || check == nil || check.ConversationID == nil {
		t.Fatalf("could not admit fixture check: %v", err)
	}
	f.s.chatMu.Lock()
	registered := f.s.chatCancel[f.s.convBusyKey(*check.ConversationID)] != nil
	f.s.chatMu.Unlock()
	if !registered {
		t.Fatal("automatic admission returned before cancellation was registered")
	}
	// Do not wait for the goroutine/provider: pause immediately after admission.
	f.pauseCommitted(t)
	waitRetestIdle(t, f.s)
	checks, err := f.s.m.pg.ListFindingChecks(f.finding)
	if err != nil || len(checks) != 1 || checks[0].Status != "stopped" {
		t.Fatal("create-first immediate pause left the admitted check active")
	}
}

func TestManualFindingCheckRemainsExplicitlyAvailableWhilePaused(t *testing.T) {
	f := newFindingCheckPauseFixture(t)
	f.pauseCommitted(t)
	check, created, err := f.s.RequestFindingCheck(f.finding, db.FindingCheckKindVerify)
	if err != nil || !created || check == nil {
		t.Fatalf("explicit manual verification was rejected on a paused task: %v", err)
	}
	f.waitProvider(t)
	f.s.concMu.Lock()
	err = f.s.cancelTaskFindingChecks(f.task.ID, agent.AbortChatPausedWithTask)
	f.s.concMu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	waitRetestIdle(t, f.s)
}

func TestFindingCheckAutomaticAdmissionSerializesWithPauseCommit(t *testing.T) {
	f := newFindingCheckPauseFixture(t)
	f.s.concMu.Lock()
	entered := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		close(entered)
		check, created, err := f.s.requestAutomaticFindingCheck(f.finding, db.FindingCheckKindVerify)
		if check != nil || created {
			done <- context.Canceled // fixture failure: creation bypassed committed pause
			return
		}
		done <- err
	}()
	<-entered
	select {
	case <-done:
		f.s.concMu.Unlock()
		t.Fatal("automatic admission bypassed the lifecycle lock")
	default:
	}
	f.s.engine.Pause(f.task.ID, agent.AbortPausedByUser)
	err := f.s.m.ApplyTaskPauseOrigin(f.task.ID, false)
	if err == nil {
		err = f.s.cancelTaskFindingChecks(f.task.ID, agent.AbortChatPausedWithTask)
	}
	f.s.concMu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatal("automatic creation slipped after committed pause")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("automatic admission did not leave the lifecycle lock")
	}
	checks, err := f.s.m.pg.ListFindingChecks(f.finding)
	if err != nil || len(checks) != 0 || f.calls.Load() != 0 {
		t.Fatal("pause-first ordering started automatic work")
	}
}

func TestFindingCheckPauseCancelsKnownRuntimeEvenWhenDatabaseFails(t *testing.T) {
	pg := openIntranetTestDB(t)
	if err := pg.Close(); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancelCause(context.Background())
	defer cancel(agent.AbortChatTurnFinished)
	s := &Server{m: &Manager{pg: pg}, chatCancel: map[string]context.CancelCauseFunc{}}
	s.findingCheckTasks.Store(int64(12), "7")
	s.chatCancel[s.convBusyKey(12)] = cancel
	if err := s.cancelTaskFindingChecks("7", agent.AbortChatPausedWithTask); err == nil {
		t.Fatal("closed database audit unexpectedly succeeded")
	}
	if ctx.Err() == nil {
		t.Fatal("database error left known verification work uncancelled")
	}
}
