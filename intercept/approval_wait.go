package intercept

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"time"

	"github.com/Autumn-27/artex/db"
)

var ErrApprovalWait = errors.New("approval_wait: 运行窗口结束时仍在等待人工审批；相应工具未执行，请处理审批并核对记录后继续")

type approvalWaitKey struct{}

// ApprovalWait tracks a run's approval waits independently of tool execution.
// Close cancels only outstanding approval waits, never approves or replays them.
type ApprovalWait struct {
	mu      sync.Mutex
	ctx     context.Context
	cancel  context.CancelFunc
	pending map[int64]time.Time
	emit    func(db.Activity)
}

func WithApprovalWait(ctx context.Context, emit func(db.Activity)) (context.Context, *ApprovalWait) {
	waitCtx, cancel := context.WithCancel(ctx)
	w := &ApprovalWait{ctx: waitCtx, cancel: cancel, pending: map[int64]time.Time{}, emit: emit}
	return context.WithValue(ctx, approvalWaitKey{}, w), w
}

func (w *ApprovalWait) Pending() bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return len(w.pending) != 0
}

func (w *ApprovalWait) Close() { w.cancel() }

func beginApprovalWait(ctx context.Context, id int64, tool string) (context.Context, func(bool)) {
	w, _ := ctx.Value(approvalWaitKey{}).(*ApprovalWait)
	if w == nil {
		return ctx, func(bool) {}
	}
	started := time.Now()
	w.mu.Lock()
	w.pending[id] = started
	w.mu.Unlock()
	emit := func(kind, summary string, allowed bool) {
		if w.emit == nil {
			return
		}
		meta, _ := json.Marshal(map[string]any{"approval_id": id, "tool": tool, "wait_ms": time.Since(started).Milliseconds(), "allowed": allowed})
		w.emit(db.Activity{Kind: kind, Tool: tool, Summary: summary, Metadata: meta})
	}
	emit("approval_wait", "等待人工审批；工具尚未执行", false)
	// Preserve tool-call audit values; link both the run and current hook context.
	waitCtx, cancel := context.WithCancel(ctx)
	stop := context.AfterFunc(w.ctx, cancel)
	return waitCtx, func(allowed bool) {
		stop()
		cancel()
		w.mu.Lock()
		delete(w.pending, id)
		w.mu.Unlock()
		emit("approval_resolved", "人工审批等待结束；是否执行以工具结果为准", allowed)
	}
}
