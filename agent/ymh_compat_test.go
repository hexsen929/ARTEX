package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/Autumn-27/artex/db"
	"github.com/Autumn-27/norma/harness"
)

func TestWorkerCheckedTaskProxyFailureStopsBeforeAssembly(t *testing.T) {
	w := NewWorker(nil, "fixture", t.TempDir(), nil, 0, 0)
	w.SetProxy("http://global.invalid:1", "global-ca")
	problem := errors.New("task tunnel unavailable")
	w.SetTaskProxyResolverChecked(func(context.Context, int64) (string, string, error) { return "", "", problem })
	reason, writes, err := w.Execute(t.Context(), "fixture", 1, nil, nil, &db.Node{ID: 1}, nil, nil, nil, nil)
	if reason != harness.ReasonHookStopped || !errors.Is(err, problem) || writes.Total() != 0 {
		t.Fatalf("reason=%s writes=%+v err=%v", reason, writes, err)
	}
	if !strings.Contains(err.Error(), "任务代理未就绪") {
		t.Fatal("proxy failure was not explicit")
	}
}

func TestWorkerCheckedTaskProxyOnlyExplicitEmptySuccessFallsBack(t *testing.T) {
	w := NewWorker(nil, "fixture", t.TempDir(), nil, 0, 0)
	w.SetProxy("global", "ca")
	w.SetTaskProxyResolverChecked(func(context.Context, int64) (string, string, error) { return "", "", nil })
	addr, ca, err := w.resolveTaskProxy(t.Context(), 1)
	if addr != "global" || ca != "ca" || err != nil {
		t.Fatalf("successful empty resolution: %s %s %v", addr, ca, err)
	}
	w.SetTaskProxyResolverChecked(func(context.Context, int64) (string, string, error) { return "", "", errors.New("failed") })
	addr, ca, err = w.resolveTaskProxy(t.Context(), 1)
	if addr != "" || ca != "" || err == nil {
		t.Fatal("failed task proxy silently fell back")
	}
}

func TestModelCallTimeoutDoesNotBecomeWorkerBudgetTimeout(t *testing.T) {
	term := &harness.Terminal{Reason: harness.ReasonTimeout, Err: &CallTimeoutError{Provider: "fixture", Model: "fixture", Timeout: time.Second, Elapsed: time.Second}}
	normalized := normalizeCallTimeoutTerminal(t.Context(), term)
	if normalized.Reason != harness.ReasonModelError || term.Reason != harness.ReasonTimeout {
		t.Fatal("call timeout was misclassified or mutated source terminal")
	}
	summary, _ := terminalText(t.Context(), term, &runTrace{})
	if strings.Contains(summary, "运行预算上限") || !strings.Contains(summary, "model_error") {
		t.Fatal(summary)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if got := normalizeCallTimeoutTerminal(ctx, term); got.Reason != harness.ReasonTimeout {
		t.Fatal("caller cancellation was hidden")
	}
}

func TestIntranetPromptRetainsVerificationQualityGuidance(t *testing.T) {
	for _, intranet := range []bool{false, true} {
		prompt := workerSystem("", "", t.TempDir(), t.TempDir(), intranet)
		if !strings.Contains(prompt, executionQualityGuidance("worker")) {
			t.Fatal("worker quality contract lost")
		}
		if !strings.Contains(prompt, "反 AI 蜜罐红线") {
			t.Fatal("new anti-trap guidance missing")
		}
	}
}

func TestGoalProofUsesIndependentVerificationAndPreservesUncertainFindings(t *testing.T) {
	d := testDB(t)
	defer d.Close()
	task, err := d.CreateTask("verification goal gate fixture", "fixture", nil, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer d.DeleteTask(task.ID)
	es := d.Exploration(task.ExplorationID)
	goalID, err := es.AddNode(db.KindGoal, map[string]any{"text": "fixture goal"}, 0, "open", "fixture", nil)
	if err != nil {
		t.Fatal(err)
	}
	tools := NewToolSet(es, "fixture")
	tools.SetTaskID(task.ID)
	tools.SetAssetStore(d.Assets(), d.Companies())
	var confirmedNode int64
	for _, status := range []string{db.VerificationUnverified, db.VerificationBlocked, db.VerificationRejected, db.VerificationConfirmed} {
		recorded, e := es.RecordFinding(t.Context(), db.RecordFindingInput{TaskID: task.ID, Summary: "fixture " + status, Severity: "high", Evidence: "actual fixture observation", VerificationStatus: status, VerificationNote: "fixture basis"})
		if e != nil {
			t.Fatal(e)
		}
		if status == db.VerificationConfirmed {
			confirmedNode = recorded.NodeID
			continue
		}
		result, e := tools.proveGoal().Call(t.Context(), json.RawMessage(fmt.Sprintf(`{"goal_id":%d,"evidence_id":%d,"reason":"fixture"}`, goalID, recorded.NodeID)), nil)
		if e != nil || !result.IsError {
			t.Fatalf("unconfirmed %s accepted as proof: %v %s", status, e, result.Flatten())
		}
		if f, e := d.GetFinding(recorded.FindingID); e != nil || f == nil || f.VerificationStatus != status {
			t.Fatalf("uncertain record changed: %+v %v", f, e)
		}
	}
	overview := tools.graphOverviewData()
	if overview["findings"] != 1 || overview["findings_total"] != 4 {
		t.Fatalf("confirmation count mixed with all findings: %+v", overview)
	}
	result, err := tools.proveGoal().Call(t.Context(), json.RawMessage(fmt.Sprintf(`{"goal_id":%d,"evidence_id":%d,"reason":"confirmed fixture"}`, goalID, confirmedNode)), nil)
	if err != nil || result.IsError {
		t.Fatalf("confirmed finding not accepted: %v %s", err, result.Flatten())
	}
}
