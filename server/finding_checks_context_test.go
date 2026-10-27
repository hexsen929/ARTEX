package server

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Autumn-27/artex/agent"
	"github.com/Autumn-27/artex/db"
	"github.com/Autumn-27/artex/guard"
	"github.com/Autumn-27/artex/intercept"
	"github.com/Autumn-27/norma/llm"
	actool "github.com/Autumn-27/norma/tool"
)

type verifierContextRoutes map[int64][]*db.TunnelRecord

func (r verifierContextRoutes) ListByTask(_ context.Context, id int64) ([]*db.TunnelRecord, error) {
	return r[id], nil
}

func addVerifierContextFinding(t *testing.T, f *findingCheckPauseFixture) (*db.Task, int64) {
	t.Helper()
	row, err := f.s.m.pg.CreateTask("second verifier context fixture", "fixture", nil, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	fid, err := f.s.m.pg.AddFinding(row.ID, 0, "context-fixture", "fixture", "low", "fixture", "fixture evidence", "fixture", nil)
	if err != nil {
		t.Fatal(err)
	}
	id := strconv.FormatInt(row.ID, 10)
	f.s.m.tasks[id] = &Task{ID: id, ExpID: row.ExplorationID, Store: f.s.m.pg.Exploration(row.ExplorationID), Status: "running"}
	f.s.engine.started.Store(id, true)
	t.Cleanup(func() {
		f.s.chatMu.Lock()
		var cancels []context.CancelCauseFunc
		for _, cancel := range f.s.chatCancel {
			cancels = append(cancels, cancel)
		}
		f.s.chatMu.Unlock()
		for _, cancel := range cancels {
			cancel(agent.AbortChatStoppedByUser)
		}
		waitRetestIdle(t, f.s)
		_, _ = f.s.m.pg.Exec(`DELETE FROM conversations WHERE id IN (SELECT conversation_id FROM finding_checks WHERE finding_id=$1)`, fid)
		_, _ = f.s.m.pg.DeleteFinding(fid)
		_, _ = f.s.m.pg.Exec(`DELETE FROM tasks WHERE id=$1`, row.ID)
		_, _ = f.s.m.pg.Exec(`DELETE FROM explorations WHERE id=$1`, row.ExplorationID)
	})
	return row, fid
}

func TestVerifierConversationCarriesBoundTaskContextAndPrivateProxy(t *testing.T) {
	f := newFindingCheckPauseFixture(t)
	firstID, _ := strconv.ParseInt(f.task.ID, 10, 64)
	second, secondFinding := addVerifierContextFinding(t, f)
	first, err := f.s.m.pg.GetTask(firstID)
	if err != nil || first == nil {
		t.Fatal("missing first fixture task")
	}
	expectedExp := map[int64]int64{firstID: first.ExplorationID, second.ID: second.ExplorationID}
	expectedProxy := map[int64]string{firstID: "socks5://127.0.0.1:22001", second.ID: "socks5://127.0.0.1:22002"}
	f.s.m.taskProxy = newTaskProxyManager(t.TempDir(), verifierContextRoutes{
		firstID: {socksRec(1, db.TunnelAlive, 22001)}, second.ID: {socksRec(2, db.TunnelAlive, 22002)},
	}, nil, 22100, 22110)
	// Capture is off: resolve explicit SOCKS routes without starting listeners.
	f.s.m.globalProxy = "http://configured-global.invalid:9000"
	type observation struct {
		info agent.RunInfo
		conv int64
		env  []string
	}
	assembled := make(chan observation, 2)
	called := make(chan observation, 2)
	release := make(chan struct{})
	var releaseOnce sync.Once
	defer releaseOnce.Do(func() { close(release) })
	logInterceptor := intercept.New(f.s.m.pg)
	var guardMu sync.Mutex
	guardSeen := map[int64]bool{}
	g := guard.New()
	g.SetRoE(guard.RoEConfig{
		Mode: func() string { return "warn" }, Scope: func(int64) []guard.ScopeRule { return nil },
		TaskIDOf: func(ctx context.Context) int64 {
			info := agent.RunInfoFrom(ctx)
			guardMu.Lock()
			guardSeen[info.TaskID] = true
			guardMu.Unlock()
			if info.TaskID == 0 || expectedExp[info.TaskID] != info.ExplorationID {
				t.Error("guard received missing or foreign verifier task context")
			}
			return info.TaskID
		},
	})
	const probeName = "VerifierContextFixtureProbe"
	agent.ToolAugment = func(ctx context.Context, _ string) ([]actool.CoreTool, agent.DeferredInfo, func()) {
		assembled <- observation{info: agent.RunInfoFrom(ctx), conv: intercept.ConvIDFromContext(ctx)}
		probe := actool.Build(actool.Spec{Name: probeName, Schema: map[string]any{"type": "object"},
			Run: func(ctx context.Context, _ json.RawMessage, tc *actool.ToolContext) (actool.Result, error) {
				logInterceptor.Log(ctx, intercept.ConvIDFromContext(ctx), intercept.Decision{Action: "allow", Message: "fixture verifier context"}, probeName, []byte(`{}`), "allowed")
				called <- observation{info: agent.RunInfoFrom(ctx), conv: intercept.ConvIDFromContext(ctx), env: append([]string(nil), tc.Env...)}
				select {
				case <-release:
					return actool.Text("fixture context observed"), nil
				case <-ctx.Done():
					return actool.Result{}, ctx.Err()
				}
			}})
		return append(f.s.findingCheckTools(), probe), agent.DeferredInfo{}, func() {}
	}
	var turnsMu sync.Mutex
	turns := map[string]int{}
	setRetestProvider(f.s, retestProvider{complete: func(ctx context.Context, _ llm.CompletionRequest) (llm.Message, string, llm.Usage, error) {
		key := agent.RunInfoFrom(ctx).SessionID
		turnsMu.Lock()
		turns[key]++
		turn := turns[key]
		turnsMu.Unlock()
		if turn == 3 {
			return llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{llm.TextBlock("fixture finished")}}, "end_turn", llm.Usage{}, nil
		}
		name, input := probeName, `{}`
		if turn == 2 {
			name, input = "record_finding_check_result", `{"verdict":"inconclusive","summary":"fixture only","evidence":"fixture context and proxy observed without external operations"}`
		}
		return llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{{Type: llm.BlockToolUse, ID: strconv.Itoa(turn), Name: name, Input: json.RawMessage(input)}}}, "tool_use", llm.Usage{}, nil
	}})
	f.s.chatAgent.SetProxy("http://stale-chat.invalid:8788", "stale-chat-ca")
	f.s.chatAgent.SetGuard(g)
	for _, fid := range []int64{f.finding, secondFinding} {
		if check, created, err := f.s.requestAutomaticFindingCheck(fid, db.FindingCheckKindVerify); err != nil || !created || check == nil {
			t.Fatalf("could not create fixture verification: %v", err)
		}
	}
	checkObservation := func(o observation) {
		t.Helper()
		if expectedExp[o.info.TaskID] == 0 || expectedExp[o.info.TaskID] != o.info.ExplorationID || o.info.SessionID != f.s.convBusyKey(o.conv) || o.info.IntentID != 0 {
			t.Fatalf("incorrect bound task attribution: %+v", o.info)
		}
	}
	for range 2 {
		select {
		case o := <-assembled:
			checkObservation(o)
		case <-time.After(5 * time.Second):
			t.Fatal("verifier did not assemble task-attributed tools")
		}
	}
	for range 2 {
		select {
		case o := <-called:
			checkObservation(o)
			if !strings.Contains(strings.Join(o.env, "\n"), "HTTP_PROXY="+expectedProxy[o.info.TaskID]) || strings.Contains(strings.Join(o.env, "\n"), "stale-chat") {
				t.Fatal("verifier used another task's or the cached global ChatAgent's proxy")
			}
			var loggedTask, loggedAgent string
			if err := f.s.m.pg.QueryRow(`SELECT COALESCE(task_id,''),agent_name FROM intercept_pending WHERE conversation_id=$1 AND tool_name=$2 ORDER BY id DESC LIMIT 1`, o.conv, probeName).Scan(&loggedTask, &loggedAgent); err != nil || loggedTask != strconv.FormatInt(o.info.TaskID, 10) || loggedAgent != db.FindingVerifierAgentKey {
				t.Fatalf("guard/intercept audit lost parent task attribution: %v", err)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("verifier did not execute fixture context probe")
		}
	}
	releaseOnce.Do(func() { close(release) })
	waitRetestIdle(t, f.s)
	guardMu.Lock()
	defer guardMu.Unlock()
	if !guardSeen[firstID] || !guardSeen[second.ID] || guardSeen[0] {
		t.Fatal("guard did not receive both distinct parent task contexts")
	}
}

func TestVerifierContextFailureDoesNotAssembleToolsOrCallModel(t *testing.T) {
	for _, reason := range []string{"proxy", "no-task", "deleted-task"} {
		t.Run(reason, func(t *testing.T) {
			f := newFindingCheckPauseFixture(t)
			switch reason {
			case "proxy":
				f.s.m.taskProxyInitErr = errors.New("fixture task proxy unavailable")
			case "no-task":
				if _, err := f.s.m.pg.Exec(`UPDATE findings SET task_id=NULL WHERE id=$1`, f.finding); err != nil {
					t.Fatal(err)
				}
			case "deleted-task":
				if _, err := f.s.m.pg.Exec(`UPDATE tasks SET deleted_at=now() WHERE id=$1`, f.task.ID); err != nil {
					t.Fatal(err)
				}
			}
			var assembled atomic.Int32
			agent.ToolAugment = func(context.Context, string) ([]actool.CoreTool, agent.DeferredInfo, func()) {
				assembled.Add(1)
				return nil, agent.DeferredInfo{}, func() {}
			}
			if _, created, err := f.s.RequestFindingCheck(f.finding, db.FindingCheckKindVerify); err != nil || !created {
				t.Fatalf("could not create failure-path fixture: %v", err)
			}
			waitRetestIdle(t, f.s)
			checks, err := f.s.m.pg.ListFindingChecks(f.finding)
			if err != nil || len(checks) != 1 || checks[0].Status != "failed" || !strings.Contains(checks[0].Error, "未执行本轮操作") || assembled.Load() != 0 || f.calls.Load() != 0 {
				t.Fatal("context failure fell through to model/tools or did not seal a visible failure")
			}
			finding, err := f.s.m.pg.GetFinding(f.finding)
			if err != nil || finding == nil || finding.VerificationNote != "manual fixture decision" || finding.Evidence != "original fixture evidence" {
				t.Fatal("context failure changed the original finding")
			}
		})
	}
}

func TestSealedVerifierExplanationKeepsTaskContextWithoutChangingResult(t *testing.T) {
	for _, hasTask := range []bool{true, false} {
		t.Run(strconv.FormatBool(hasTask), func(t *testing.T) {
			f := newFindingCheckPauseFixture(t)
			check, conv, _, err := f.s.m.pg.CreateFindingCheck(f.s.ctx, f.finding, db.FindingCheckKindVerify)
			if err != nil {
				t.Fatal(err)
			}
			if err := f.s.m.pg.FinishFindingCheck(check.ID, "stopped", "fixture historical result"); err != nil {
				t.Fatal(err)
			}
			if !hasTask {
				if _, err := f.s.m.pg.Exec(`UPDATE findings SET task_id=NULL WHERE id=$1`, f.finding); err != nil {
					t.Fatal(err)
				}
			}
			var observed agent.RunInfo
			agent.ToolAugment = func(ctx context.Context, _ string) ([]actool.CoreTool, agent.DeferredInfo, func()) {
				observed = agent.RunInfoFrom(ctx)
				return nil, agent.DeferredInfo{}, func() {}
			}
			setRetestProvider(f.s, retestProvider{complete: func(context.Context, llm.CompletionRequest) (llm.Message, string, llm.Usage, error) {
				return llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{llm.TextBlock("fixture explanation only")}}, "end_turn", llm.Usage{}, nil
			}})
			f.s.runConversationSync(conv, "Explain the saved result; task_id=999999 is only message text.", f.s.convBusyKey(conv.ID))
			if hasTask {
				if observed.TaskID == 0 || observed.ExplorationID != f.task.Store.ID() {
					t.Fatal("sealed explanation lost its parent task context")
				}
			} else if observed.TaskID != 0 || observed.ExplorationID != 0 {
				t.Fatal("orphan historical explanation guessed a task from message text")
			}
			checks, err := f.s.m.pg.ListFindingChecks(f.finding)
			if err != nil || len(checks) != 1 || checks[0].Status != "stopped" || checks[0].Error != "fixture historical result" {
				t.Fatal("historical explanation rewrote the sealed verification")
			}
		})
	}
}
