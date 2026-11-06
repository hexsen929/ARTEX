package server

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/Autumn-27/artex/agent"
)

const (
	settingProxyReconnectMax      = "proxy_reconnect_max_attempts"
	settingProxyReconnectInterval = "proxy_reconnect_interval_seconds"
	defaultProxyReconnectMax      = 5
	defaultProxyReconnectInterval = 30
)

// isProxyFailure is used for LLM/planner errors, where the error has no trusted
// indication that it came from the global target proxy. Keep this deliberately
// narrow: generic EOF/reset/refused errors belong to the model_error retry path.
func isProxyFailure(err error) bool {
	if err == nil {
		return false
	}
	s := strings.ToLower(err.Error())
	for _, part := range []string{"proxyconnect", "proxy connection", "proxy error", "proxy handshake", "socks handshake", "failed to connect to proxy", "connect to proxy"} {
		if strings.Contains(s, part) {
			return true
		}
	}
	return false
}

// isGlobalProxyDialFailure is only used by the MITM upstream callback. That
// callback is on the actual configured global-proxy path; the saved proxy is
// still probed before any task is paused, so a target-side RequestError cannot
// pause tasks while the proxy itself remains healthy.
func isGlobalProxyDialFailure(err error) bool {
	if err == nil {
		return false
	}
	s := strings.ToLower(err.Error())
	for _, part := range []string{"proxyconnect", "proxy connection", "proxy error", "proxy handshake", "socks", "connect to proxy", "upstream proxy", "connection refused", "connection reset", "network is unreachable", "broken pipe", "unexpected eof", "eof"} {
		if strings.Contains(s, part) {
			return true
		}
	}
	return false
}

type proxyReconnectSupervisor struct {
	mu     sync.Mutex
	wake   chan struct{}
	wg     sync.WaitGroup
	cancel context.CancelFunc
	closed bool
}

type proxyReconnectGuard struct {
	generation   uint64
	attempt      int
	checkAttempt bool
}

func (s *Server) snapshotProxyReconnectGuards(checkAttempt bool) map[string]proxyReconnectGuard {
	return s.snapshotProxyReconnectGuardsWithExhausted(checkAttempt, false)
}

func (s *Server) snapshotProxyReconnectGuardsWithExhausted(checkAttempt, includeExhausted bool) map[string]proxyReconnectGuard {
	guards := make(map[string]proxyReconnectGuard)
	if s == nil || s.m == nil {
		return guards
	}
	for _, t := range s.m.List() {
		st := t.lifecycleSnapshot()
		if st.PauseReason != "paused_by_proxy" {
			continue
		}
		if st.ReconnectExhausted && !includeExhausted {
			continue
		}
		guards[t.ID] = proxyReconnectGuard{
			generation:   st.ReconnectGeneration,
			attempt:      st.ReconnectAttempt,
			checkAttempt: checkAttempt,
		}
	}
	return guards
}

func proxyReconnectGuardMatches(st taskLifecycleState, guard proxyReconnectGuard) bool {
	return st.PauseReason == "paused_by_proxy" && st.ReconnectGeneration == guard.generation &&
		(!guard.checkAttempt || st.ReconnectAttempt == guard.attempt)
}

func (s *Server) ensureProxySupervisor() {
	s.proxyReconnect.mu.Lock()
	defer s.proxyReconnect.mu.Unlock()
	if s.proxyReconnect.closed {
		return
	}
	if s.proxyReconnect.wake == nil {
		ctx, cancel := context.WithCancel(serverContext(s))
		s.proxyReconnect.cancel = cancel
		s.proxyReconnect.wake = make(chan struct{}, 1)
		s.proxyReconnect.wg.Add(1)
		go func() { defer s.proxyReconnect.wg.Done(); s.runProxyReconnect(ctx) }()
	}
}

func (s *Server) signalProxySupervisor() {
	s.ensureProxySupervisor()
	select {
	case s.proxyReconnect.wake <- struct{}{}:
	default:
	}
}

// Traffic reports and Engine callbacks may arrive after cancellation. Serialize
// callback admission with Close so no WaitGroup.Add races its final Wait, and
// a late callback cannot use PG after Manager has closed it.
func (s *Server) beginProxyCallback() bool {
	s.proxyReconnect.mu.Lock()
	defer s.proxyReconnect.mu.Unlock()
	if s.proxyReconnect.closed || (s.proxyCtx != nil && s.proxyCtx.Err() != nil) {
		return false
	}
	s.proxyCallbackWG.Add(1)
	return true
}

func (s *Server) proxyShuttingDown() bool {
	s.proxyReconnect.mu.Lock()
	defer s.proxyReconnect.mu.Unlock()
	return s.proxyReconnect.closed || (s.proxyCtx != nil && s.proxyCtx.Err() != nil)
}

func (s *Server) proxyReconnectConfig() (int, time.Duration) {
	max := defaultProxyReconnectMax
	interval := defaultProxyReconnectInterval
	if s == nil || s.m == nil || s.m.pg == nil {
		return max, time.Duration(interval) * time.Second
	}
	if v, ok, _ := s.m.pg.GetSetting(settingProxyReconnectMax); ok {
		if n, err := fmt.Sscanf(strings.TrimSpace(v), "%d", &max); err != nil || n != 1 || max <= 0 {
			max = defaultProxyReconnectMax
		}
	}
	if v, ok, _ := s.m.pg.GetSetting(settingProxyReconnectInterval); ok {
		if n, err := fmt.Sscanf(strings.TrimSpace(v), "%d", &interval); err != nil || n != 1 || interval <= 0 {
			interval = defaultProxyReconnectInterval
		}
	}
	return max, time.Duration(interval) * time.Second
}

func (s *Server) runProxyProbe(ctx context.Context, proxyRaw, caPEMPath string, probes []string, timeout time.Duration) egressProbeResult {
	if s != nil && s.proxyProbe != nil {
		return s.proxyProbe(ctx, proxyRaw, caPEMPath, probes, timeout)
	}
	return probeEgress(ctx, proxyRaw, caPEMPath, probes, timeout)
}

func (s *Server) handleProxyFailure(taskID string, cause error) {
	if !s.beginProxyCallback() {
		return
	}
	defer s.proxyCallbackWG.Done()
	if strings.TrimSpace(s.m.GlobalProxy()) == "" || !s.taskUsesGlobalProxy(taskID) {
		return
	}
	if s.handleProxyPoolFailure(cause) {
		return
	}
	probes, err := s.m.egressProbesChecked()
	if err != nil {
		return
	}
	if probe := s.runProxyProbe(serverContext(s), s.m.GlobalProxy(), "", probes, 10*time.Second); probe.OK {
		return
	}
	if s.proxyShuttingDown() || !s.m.egressProbesMatch(probes) {
		return
	}
	s.pauseProxyTask(taskID, cause)
}

func (s *Server) pauseProxyTask(taskID string, cause error) {
	if s.proxyShuttingDown() {
		return
	}
	if strings.TrimSpace(s.m.GlobalProxy()) == "" || !s.taskUsesGlobalProxy(taskID) {
		return
	}
	if s.engine != nil && s.engine.IsDeleting(taskID) {
		return
	}
	max, interval := s.proxyReconnectConfig()
	now := time.Now().Add(interval).Unix()
	lastError := truncateProxyError(cause)
	transitioned, err := s.m.ApplyProxyPauseWithCallback(taskID, func() {
		if s.engine != nil {
			s.engine.Pause(taskID, agent.AbortPausedByProxy)
		}
		if t, ok := s.m.Task(taskID); ok {
			t.updateLifecycle(func(st *taskLifecycleState) {
				// A successful admission clears the prior round's counters. This
				// transition starts a fresh round, so make that reset atomic with
				// the persisted proxy pause and install a new generation.
				st.ReconnectGeneration++
				st.ReconnectAttempt = 0
				st.ReconnectMax = max
				st.ReconnectNextAt = now
				st.ReconnectExhausted = false
				st.ReconnectLastError = lastError
			})
		}
		// Keep cancellation inside the same admission critical section as the
		// committed pause: a concurrent manual resume must not start a new turn
		// only for this older proxy callback to cancel it afterwards.
		s.cancelTaskChat(taskID, agent.AbortChatPausedWithTask)
		if err := s.cancelTaskFindingChecks(taskID, agent.AbortPausedByProxy); err != nil {
			log.Printf("[proxy] task %s finding checks cancelled; audit finalization failed: %v", taskID, err)
		}
	})
	if err != nil {
		log.Printf("[proxy] pause task %s: %v", taskID, err)
		return
	}
	if !transitioned {
		// A no-op means the task was already paused (possibly by a user,
		// schedule, or an exhausted proxy attempt). Preserve its reason and
		// reconnect state exactly as-is.
		return
	}
	s.ensureProxySupervisor()
	s.signalProxySupervisor()
}

func (s *Server) handleProxyTrafficFailure(cause error) {
	if !s.beginProxyCallback() {
		return
	}
	defer s.proxyCallbackWG.Done()
	if s.handleProxyPoolFailure(cause) {
		return
	}
	if !isGlobalProxyDialFailure(cause) || strings.TrimSpace(s.m.GlobalProxy()) == "" {
		return
	}
	// Confirm the saved global proxy itself before pausing. MITM RequestError can
	// also represent a target-side failure, and those must not pause a task.
	probes, err := s.m.egressProbesChecked()
	if err != nil {
		return
	}
	if probe := s.runProxyProbe(serverContext(s), s.m.GlobalProxy(), "", probes, 10*time.Second); probe.OK {
		return
	}
	if s.proxyShuttingDown() || !s.m.egressProbesMatch(probes) {
		return
	}
	for _, t := range s.m.List() {
		st := t.lifecycleSnapshot()
		if isTerminalStatus(st.Status) || st.Paused || s.engine.IsDeleting(t.ID) {
			continue
		}
		s.pauseProxyTask(t.ID, cause)
	}
}

func (s *Server) handleProxyToolFailure(ctx context.Context, cause error) {
	if !s.beginProxyCallback() {
		return
	}
	defer s.proxyCallbackWG.Done()
	info := agent.RunInfoFrom(ctx)
	if info.TaskID > 0 && !s.taskUsesGlobalProxy(fmt.Sprint(info.TaskID)) {
		return
	}
	if s.handleProxyPoolFailure(cause) {
		return
	}
	if !isGlobalProxyDialFailure(cause) || strings.TrimSpace(s.m.GlobalProxy()) == "" {
		return
	}
	probes, err := s.m.egressProbesChecked()
	if err != nil {
		return
	}
	if probe := s.runProxyProbe(serverContext(s), s.m.GlobalProxy(), "", probes, 10*time.Second); probe.OK {
		return
	}
	if s.proxyShuttingDown() || !s.m.egressProbesMatch(probes) {
		return
	}
	if info.TaskID > 0 {
		s.pauseProxyTask(fmt.Sprint(info.TaskID), cause)
	}
}

func truncateProxyError(err error) string {
	if err == nil {
		return ""
	}
	s := maskSecrets(err.Error())
	if len(s) > 240 {
		return s[:240] + "…"
	}
	return s
}

func (s *Server) runProxyReconnect(ctx context.Context) {
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.proxyReconnectTickContext(ctx)
		case <-s.proxyReconnect.wake:
			s.proxyReconnectTickContext(ctx)
		}
	}
}

func (s *Server) proxyReconnectTick() {
	s.proxyReconnectTickContext(context.Background())
}

func (s *Server) proxyReconnectTickContext(ctx context.Context) {
	if ctx.Err() != nil {
		return
	}
	if strings.TrimSpace(s.m.GlobalProxy()) == "" {
		// Clearing the runtime route is a successful direct transition. Reuse the
		// generation gate so only eligible, non-exhausted proxy pauses resume.
		s.proxyReconnectSuccess()
		return
	}
	probes, err := s.m.egressProbesChecked()
	if err != nil {
		return
	}
	if s.m.proxyPool != nil && len(s.m.proxyPoolEntries()) > 0 {
		// A pool tick may confirm the active route or switch to another available
		// member. If it cannot confirm either, retain the original #106 per-task
		// attempt/backoff/exhaustion loop (especially for a one-member legacy
		// pool).
		outcome := s.proxyPoolTick()
		if outcome.confirmed || outcome.switched {
			return
		}
	}
	now := time.Now().Unix()
	max, interval := s.proxyReconnectConfig()
	for _, t := range s.m.List() {
		st := t.lifecycleSnapshot()
		if isTerminalStatus(st.Status) {
			t.updateLifecycle(func(v *taskLifecycleState) {
				if v.PauseReason == "paused_by_proxy" {
					v.PauseReason = ""
				}
				v.ReconnectAttempt, v.ReconnectMax, v.ReconnectNextAt = 0, 0, 0
				v.ReconnectExhausted, v.ReconnectLastError = false, ""
			})
			continue
		}
		if st.PauseReason != "paused_by_proxy" || st.ReconnectExhausted || (st.ReconnectNextAt > 0 && st.ReconnectNextAt > now) {
			continue
		}
		attempt := st.ReconnectAttempt + 1
		round := st.ReconnectGeneration
		claimed := false
		t.updateLifecycle(func(v *taskLifecycleState) {
			if v.PauseReason != "paused_by_proxy" || v.ReconnectGeneration != round || v.ReconnectAttempt != st.ReconnectAttempt {
				return
			}
			v.ReconnectAttempt = attempt
			v.ReconnectMax = max
			v.ReconnectNextAt = 0
			claimed = true
		})
		if !claimed {
			continue
		}
		res := s.runProxyProbe(ctx, s.m.GlobalProxy(), "", probes, 10*time.Second)
		if ctx.Err() != nil {
			return
		}
		if !s.m.egressProbesMatch(probes) {
			// A settings edit/read failure is not a failed reconnect attempt.
			// Undo only this exact claim, never a newer pause generation.
			t.updateLifecycle(func(v *taskLifecycleState) {
				if v.PauseReason == "paused_by_proxy" && v.ReconnectGeneration == round && v.ReconnectAttempt == attempt {
					v.ReconnectAttempt = st.ReconnectAttempt
					v.ReconnectNextAt = st.ReconnectNextAt
				}
			})
			return
		}
		if res.OK {
			s.proxyReconnectSuccessWithGuards(map[string]proxyReconnectGuard{
				t.ID: {generation: round, attempt: attempt, checkAttempt: true},
			}, nil)
			return
		}
		err := res.Error
		exhausted := attempt >= max
		next := time.Now().Add(interval).Unix()
		t.updateLifecycle(func(v *taskLifecycleState) {
			if v.PauseReason != "paused_by_proxy" || v.ReconnectGeneration != round || v.ReconnectAttempt != attempt {
				return
			}
			v.ReconnectNextAt = next
			v.ReconnectExhausted = exhausted
			v.ReconnectLastError = err
		})
		log.Printf("[proxy] reconnect attempt %d/%d failed: %s", attempt, max, err)
	}
}

func (s *Server) proxyReconnectSuccess() {
	s.proxyReconnectSuccessWithGuards(s.snapshotProxyReconnectGuards(false), nil)
}

func (s *Server) proxyReconnectSuccessFor(taskID string, generation uint64, attempt int) {
	s.proxyReconnectSuccessWithGuards(map[string]proxyReconnectGuard{
		taskID: {generation: generation, attempt: attempt, checkAttempt: true},
	}, nil)
}

// proxyReconnectSuccessWithHook is kept small and private so tests can
// deterministically place a competing pause immediately after admission has
// released concMu. Production callers use proxyReconnectSuccess without a
// hook.
func (s *Server) proxyReconnectSuccessWithHook(afterAdmission func()) {
	s.proxyReconnectSuccessWithGuards(s.snapshotProxyReconnectGuards(false), afterAdmission)
}

func (s *Server) proxyReconnectSuccessWithGuard(expectedID string, expectedGeneration uint64, expectedAttempt int, afterAdmission func()) {
	guards := s.snapshotProxyReconnectGuards(false)
	if expectedID != "" {
		guards = map[string]proxyReconnectGuard{
			expectedID: {generation: expectedGeneration, attempt: expectedAttempt, checkAttempt: true},
		}
	}
	s.proxyReconnectSuccessWithGuards(guards, afterAdmission)
}

func (s *Server) proxyReconnectSuccessWithGuards(guards map[string]proxyReconnectGuard, afterAdmission func()) {
	if s.proxyShuttingDown() {
		return
	}
	max, interval := s.proxyReconnectConfig()
	s.scheduleMu.Lock()
	defer s.scheduleMu.Unlock()
	if s.proxyShuttingDown() {
		return
	}
	for _, t := range s.m.List() {
		if s.proxyShuttingDown() {
			return
		}
		guard, ok := guards[t.ID]
		if !ok {
			continue
		}
		if !proxyReconnectGuardMatches(t.lifecycleSnapshot(), guard) {
			continue
		}
		if s.m.pg != nil {
			inside, scheduleErr := s.proxyResumeInsideSchedule(t.ID, time.Now())
			if scheduleErr != nil {
				log.Printf("[proxy] schedule gate for task %s: %v", t.ID, scheduleErr)
				continue
			}
			if !inside {
				if err := s.m.ConvertProxyPauseToSchedule(t.ID); err != nil {
					log.Printf("[proxy] schedule pause task %s: %v", t.ID, err)
				}
				continue
			}
		}
		// Check before admitting a lifecycle transaction, not after commit:
		// accepted transactions must still publish their matching runtime state.
		if s.proxyShuttingDown() {
			return
		}
		var err error
		if s.engine != nil {
			_, err = s.admitTaskWhenScheduledWithGenerationChecked(t, "resume", true, false, "paused_by_proxy", true, guard.generation, true)
			if afterAdmission != nil {
				afterAdmission()
			}
		} else {
			err = s.m.ClearPauseReason(t.ID)
		}
		if err != nil {
			// Admission re-checks the persisted reason under concMu. A user pause,
			// terminal transition, delete, or archive therefore cannot be revived;
			// only keep probing if the proxy pause is still eligible.
			st := t.lifecycleSnapshot()
			if proxyReconnectGuardMatches(st, guard) && !isTerminalStatus(st.Status) {
				t.updateLifecycle(func(v *taskLifecycleState) {
					v.ReconnectMax = max
					v.ReconnectNextAt = time.Now().Add(interval).Unix()
				})
			}
			log.Printf("[proxy] resume task %s: %v", t.ID, err)
			continue
		}
		// The non-queued path's startAdmittedTask already resumes the Engine
		// while concMu is held. Do not Resume again after releasing the lock:
		// a concurrent user/orchestrator pause could have installed a newer
		// barrier in the meantime.
		t.updateLifecycle(func(v *taskLifecycleState) {
			if !proxyReconnectGuardMatches(*v, guard) {
				return
			}
			v.ReconnectAttempt = 0
			v.ReconnectMax = 0
			v.ReconnectNextAt = 0
			v.ReconnectExhausted = false
			v.ReconnectLastError = ""
		})
	}
}

func (s *Server) testTaskProxy(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	t, ok := s.m.Task(id)
	if !ok {
		writeErr(w, 404, "task not found")
		return
	}
	proxy := strings.TrimSpace(s.m.GlobalProxy())
	guards := s.snapshotProxyReconnectGuardsWithExhausted(false, true)
	if proxy == "" {
		writeJSON(w, 200, map[string]any{"ok": true, "direct": true})
		s.proxyReconnectSuccessWithGuards(guards, nil)
		return
	}
	probes, err := s.m.egressProbesChecked()
	if err != nil {
		writeErr(w, http.StatusServiceUnavailable, err.Error())
		return
	}
	res := s.runProxyProbe(r.Context(), proxy, "", probes, 10*time.Second)
	if r.Context().Err() != nil || s.proxyShuttingDown() || !s.m.egressProbesMatch(probes) {
		writeErr(w, http.StatusConflict, "请求已取消或出口探测服务已变更，结果已作废")
		return
	}
	if res.OK {
		s.proxyReconnectSuccessWithGuards(guards, nil)
	}
	if !res.OK && res.Error == "" {
		res.Error = "代理探测失败"
	}
	writeJSON(w, 200, map[string]any{"ok": res.OK, "error": res.Error, "task_id": t.ID})
}

func (s *Server) setProxyReconnectSettings(max, interval *int) error {
	if max != nil {
		if *max < 1 || *max > 1000 {
			return errors.New("proxy reconnect max must be 1..1000")
		}
		if err := s.m.pg.SetSetting(settingProxyReconnectMax, fmt.Sprint(*max)); err != nil {
			return err
		}
	}
	if interval != nil {
		if *interval < 1 || *interval > 86400 {
			return errors.New("proxy reconnect interval must be 1..86400 seconds")
		}
		if err := s.m.pg.SetSetting(settingProxyReconnectInterval, fmt.Sprint(*interval)); err != nil {
			return err
		}
	}
	return nil
}
