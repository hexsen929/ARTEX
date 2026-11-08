package agent

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"encoding/json"
	"github.com/Autumn-27/artex/db"
	"github.com/Autumn-27/artex/guard"
	"github.com/Autumn-27/artex/intercept"
	"github.com/Autumn-27/norma/agentcore"
	"github.com/Autumn-27/norma/harness"
	"github.com/Autumn-27/norma/llm"
	"github.com/Autumn-27/norma/permission"
	"github.com/Autumn-27/norma/tool"
)

func TestExecutionApprovalBudgetIsNotReportedAsExplorationFailure(t *testing.T) {
	dsn, _, err := db.DSN()
	if err != nil {
		t.Skip("database not configured")
	}
	d, err := db.Open(dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.Close() })
	ic := intercept.New(d)
	prior, err := ic.GetEnabledTools()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ic.SetEnabledTools(prior) })
	if err := ic.SetEnabledTools([]string{"ApprovalBudgetProbe"}); err != nil {
		t.Fatal(err)
	}
	rule, err := d.CreateInterceptRule("approval-budget-fixture", "tool_name", "string", "ApprovalBudgetProbe", "ask", "fixture", 999, true, false, 60, "deny")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = d.DeleteInterceptRule(rule.ID)
		_, _ = d.Exec(`DELETE FROM intercept_pending WHERE task_id='approval-budget-fixture'`)
	})
	var executed atomic.Int32
	probe := tool.Build(tool.Spec{Name: "ApprovalBudgetProbe", Schema: map[string]any{"type": "object"}, Run: func(context.Context, json.RawMessage, *tool.ToolContext) (tool.Result, error) {
		executed.Add(1)
		return tool.Text("ran"), nil
	}})
	p := captureUsageProvider{stream: func(ctx context.Context, yield func(llm.StreamEvent, error) bool) {
		for _, ev := range []llm.StreamEvent{{Type: llm.SEToolUseStart, ToolID: "wait-call", ToolName: "ApprovalBudgetProbe"}, {Type: llm.SEToolInputJSON, Text: `{}`}, {Type: llm.SEMessageDelta, StopReason: "tool_use"}} {
			if !yield(ev, nil) {
				return
			}
		}
	}}
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	ctx = intercept.WithTaskContext(ctx, "approval-budget-fixture", "worker", nil)
	_, reason, err := captureRun(ctx, agentcore.Options{Provider: p, Tools: []tool.CoreTool{probe}, Hooks: guard.NewWithInterceptor(ic).Hooks(), PermissionMode: permission.ModeBypass, MaxDuration: 150 * time.Millisecond}, "fixture", nil)
	if reason != harness.ReasonHookStopped || !errors.Is(err, intercept.ErrApprovalWait) || executed.Load() != 0 {
		t.Fatalf("reason=%s err=%v executions=%d", reason, err, executed.Load())
	}
}

func TestExecutionCompactionCanceledAndBoundedRounds(t *testing.T) {
	calls := 0
	p := captureUsageProvider{stream: func(ctx context.Context, yield func(llm.StreamEvent, error) bool) {
		calls++
		yield(llm.StreamEvent{Type: llm.SETextDelta, Text: "summary"}, nil)
		yield(llm.StreamEvent{Type: llm.SEMessageDelta, StopReason: "end_turn"}, nil)
	}}
	c := NewCompactor(p, "fixture")
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := c.compress(ctx, nil, block{}, nil); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	ctx = context.WithValue(t.Context(), compactionRoundKey{}, new(compactionRound))
	for n := 0; n < 8; n++ {
		if _, err := c.compress(ctx, &coldGraph{children: map[int64][]int64{}}, block{}, nil); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := c.compress(ctx, nil, block{}, nil); !errors.Is(err, errCompactionRoundBudget) {
		t.Fatal(err)
	}
	if calls != 8 {
		t.Fatalf("provider calls=%d", calls)
	}
	c.deferCompaction(42, errors.New("anthropic: status 403 用户额度不足"))
	if c.tryStart(42) || time.Until(c.nextRun[42]) < 29*time.Minute {
		t.Fatal("quota failure retried without cooldown")
	}
	var members []int64
	nodes := map[int64]*db.Node{}
	for n := int64(1); n <= 101; n++ {
		members = append(members, n)
		raw, _ := json.Marshal(map[string]any{"summary": strings.Repeat("中", 5000)})
		nodes[n] = &db.Node{ID: n, Kind: "fact", State: "confirmed", Payload: raw}
	}
	chunks := boundedCompactionBlocks([]block{{Members: members}})
	count := 0
	for _, chunk := range chunks {
		count += len(chunk.Members)
		if len(chunk.Members) < 2 || len(chunk.Members) > 24 || len(buildCompressionInput(&coldGraph{children: map[int64][]int64{}}, chunk, nodes)) > 128*1024 {
			t.Fatal("oversized or singleton compression chunk")
		}
	}
	if count != len(members) {
		t.Fatal("chunking lost source members")
	}
}

func TestExecutionCompactionFailurePreservesPreviousCoverage(t *testing.T) {
	dsn, _, err := db.DSN()
	if err != nil {
		t.Skip("database not configured")
	}
	d, err := db.Open(dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.Close() })
	exp, err := d.CreateExploration("compaction failure fixture", "fixture")
	if err != nil {
		t.Fatal(err)
	}
	defer d.Exec(`DELETE FROM explorations WHERE id=$1`, exp)
	s := d.Exploration(exp)
	a, err := s.AddIntent(map[string]any{"summary": "source intent"}, 5, nil, "test")
	if err != nil {
		t.Fatal(err)
	}
	b, err := s.AddNode("fact", map[string]any{"summary": "source fact"}, 0, "confirmed", "test", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SetIntentState(a, "done"); err != nil {
		t.Fatal(err)
	}
	if err := s.Link(a, db.RelYields, b); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Exec(`UPDATE explorations SET round_no=10 WHERE id=$1`, exp); err != nil {
		t.Fatal(err)
	}
	if err := s.ApplyStampOps([]db.StampOp{{ID: a, Set: true, Round: 1}, {ID: b, Set: true, Round: 1}}); err != nil {
		t.Fatal(err)
	}
	old, err := s.AddDigest(map[string]any{"body": "previous usable summary", "signature": "stale", "generation": 1, "member_ids": []int64{a, b}}, []int64{a, b})
	if err != nil {
		t.Fatal(err)
	}
	calls := 0
	p := captureUsageProvider{stream: func(ctx context.Context, yield func(llm.StreamEvent, error) bool) {
		calls++
		yield(llm.StreamEvent{}, errors.New("status 403: 用户额度不足"))
	}}
	c := NewCompactor(p, "fixture")
	c.major(t.Context(), s)
	active, err := s.ActiveDigests()
	if err != nil {
		t.Fatal(err)
	}
	covered, err := s.CoveredMembers()
	if err != nil {
		t.Fatal(err)
	}
	if calls != 1 || len(active) != 1 || active[0].ID != old || covered[a] != old || covered[b] != old {
		t.Fatal("model failure erased previous coverage", calls, active, covered)
	}
}
