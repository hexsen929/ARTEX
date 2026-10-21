package harness

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Autumn-27/norma/tool"
)

// SettlementController requests a one-way transition from ordinary execution to
// checkpointing. It belongs to one host run, not a reusable chat session. The host
// must give a resumed run a fresh controller.
type SettlementController struct {
	requested atomic.Bool
	toolMu    sync.Mutex
	toolCount int
	toolsIdle chan struct{}
}

func NewSettlementController() *SettlementController { return &SettlementController{} }

// Request closes admission to ordinary tools immediately. An already admitted
// model/tool call may finish, then Query enters its configured settlement at the
// next turn boundary. Use the parent context to bound that in-flight work or to
// cancel immediately; Request deliberately does not cancel it.
func (c *SettlementController) Request() {
	if c != nil {
		c.requested.Store(true)
	}
}

func (c *SettlementController) Requested() bool { return c != nil && c.requested.Load() }

// WaitForTools waits for actual dispatch goroutines, including queued semaphore
// waiters and post-tool hooks. A Query terminal event can precede this point when
// a tool ignores cancellation. Call this AFTER stopping/joining every Query that
// shares the controller; otherwise a live Query can still schedule checkpoint
// tools after an idle observation. A timeout is not proof that tools have exited.
func (c *SettlementController) WaitForTools(ctx context.Context) error {
	if c == nil {
		return nil
	}
	c.toolMu.Lock()
	idle, count := c.toolsIdle, c.toolCount
	c.toolMu.Unlock()
	if count == 0 {
		return nil
	}
	select {
	case <-idle:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (c *SettlementController) trackTool() func() {
	if c == nil {
		return func() {}
	}
	c.toolMu.Lock()
	if c.toolCount == 0 {
		c.toolsIdle = make(chan struct{})
	}
	c.toolCount++
	c.toolMu.Unlock()
	return func() {
		c.toolMu.Lock()
		c.toolCount--
		if c.toolCount == 0 {
			close(c.toolsIdle)
		}
		c.toolMu.Unlock()
	}
}

type settlementControllerKey struct{}

// WithSettlementController attaches a controller to this run. Query, SDK Session
// prompts, and subagents inherit it through their context. Other sessions are not
// affected. An inherited tool restriction is retained even with a new controller.
func WithSettlementController(ctx context.Context, c *SettlementController) context.Context {
	return context.WithValue(ctx, settlementControllerKey{}, c)
}

func settlementControllerFrom(ctx context.Context) *SettlementController {
	c, _ := ctx.Value(settlementControllerKey{}).(*SettlementController)
	return c
}

var errSettlementTool = errors.New("run is settling; only configured checkpoint tools may execute")
var errSettlementClosed = errors.New("controlled run has ended")

// settlementPolicy is independent of loop's mutable turn state because dispatch
// happens concurrently, and an interrupted tool may outlive the loop itself.
type settlementPolicy struct {
	controller *SettlementController
	active     atomic.Bool
	closed     atomic.Bool
	allow      map[string]bool // nil preserves legacy budget-settlement policy
	deny       map[string]bool
}

func newSettlementPolicy(c *SettlementController, s *Settlement) *settlementPolicy {
	p := &settlementPolicy{controller: c}
	if s != nil {
		if s.AllowedTools != nil {
			p.allow = make(map[string]bool, len(s.AllowedTools))
			for _, name := range s.AllowedTools {
				p.allow[name] = true
			}
		}
		p.deny = make(map[string]bool, len(s.DisabledTools))
		for _, name := range s.DisabledTools {
			p.deny[name] = true
		}
	}
	return p
}

func (p *settlementPolicy) check(name string) error {
	if p.closed.Load() {
		return errSettlementClosed
	}
	requested := p.controller.Requested()
	if !p.active.Load() && !requested {
		return nil
	}
	// An external close request without an explicit allowlist is fail-closed.
	// Legacy budget-only settlement retains its existing denylist behavior.
	if (requested && p.allow == nil) || (p.allow != nil && !p.allow[name]) || p.deny[name] {
		return errSettlementTool
	}
	return nil
}

func (l *loop) phaseContext() context.Context {
	if l.settleCtx != nil {
		return l.settleCtx
	}
	return l.ctx
}

func (l *loop) settlementRequested() bool {
	return l.settlementControl.Requested()
}

func (l *loop) initSettlementPolicy() {
	l.settlementControl = settlementControllerFrom(l.ctx)
	if l.in.Settlement == nil && l.settlementControl == nil {
		return
	}
	l.settlementPolicy = newSettlementPolicy(l.settlementControl, l.in.Settlement)
	l.ctx = tool.WithExecutionGuard(l.ctx, l.settlementPolicy.check)
}

func (l *loop) closeSettlementPolicy() {
	if l.settleCancel != nil {
		l.settleCancel()
	}
	if p := l.settlementPolicy; p != nil && (p.controller != nil || p.active.Load() || p.allow != nil) {
		p.closed.Store(true)
	}
}

func (l *loop) startSettlementDeadline(s *Settlement, reason TerminalReason) {
	duration := s.MaxDuration
	if duration <= 0 && (reason == ReasonSettlementRequested || s.AllowedTools != nil) {
		duration = 30 * time.Second
	}
	l.settleCtx = l.ctx
	if duration > 0 {
		l.settleCtx, l.settleCancel = context.WithTimeout(l.ctx, duration)
	}
	l.toolCtx = l.settleCtx
}
