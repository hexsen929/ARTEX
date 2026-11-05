package intercept

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/Autumn-27/artex/db"
)

func TestExecutionApprovalWaitCanceledRunNeverExecutesLateApproval(t *testing.T) {
	d := openTestDB(t)
	ic := New(d)
	var mu sync.Mutex
	var events []db.Activity
	ctx, wait := WithApprovalWait(t.Context(), func(a db.Activity) { mu.Lock(); events = append(events, a); mu.Unlock() })
	defer wait.Close()
	ctx = WithTaskContext(ctx, "approval-wait-fixture", "worker", nil)
	t.Cleanup(func() { _, _ = d.Exec(`DELETE FROM intercept_pending WHERE task_id='approval-wait-fixture'`) })
	done := make(chan bool, 1)
	go func() {
		done <- ic.HandleAsk(ctx, 0, Decision{Action: "ask", Message: "fixture"}, "Bash", []byte(`{"command":"fixture"}`))
	}()
	deadline := time.Now().Add(3 * time.Second)
	for !wait.Pending() && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if !wait.Pending() {
		t.Fatal("wait not tracked")
	}
	wait.Close()
	select {
	case allowed := <-done:
		if allowed {
			t.Fatal("canceled wait approved")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("wait did not cancel")
	}
	rows, err := d.ListTaskIntercepts("approval-wait-fixture")
	if err != nil || len(rows) != 1 {
		t.Fatal(rows, err)
	}
	if err := ic.Decide(rows[0].ID, true); err == nil {
		t.Fatal("late approval revived canceled work")
	}
	detail, err := d.GetInterceptDetail(rows[0].ID)
	if err != nil || detail.Audit == nil || detail.Audit.ExecutionStatus != "not_executed" {
		t.Fatal(detail, err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(events) != 2 || events[0].Kind != "approval_wait" || events[1].Kind != "approval_resolved" {
		t.Fatal("missing wait lifecycle", events)
	}
}
