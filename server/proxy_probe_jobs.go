package server

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"time"
)

type proxyProbeItem struct {
	State     string `json:"state"`
	OK        bool   `json:"ok"`
	Error     string `json:"error,omitempty"`
	IP        string `json:"ip,omitempty"`
	Protocol  string `json:"protocol,omitempty"`
	LatencyMS int64  `json:"latency_ms,omitempty"`
	revision  uint64
}

type proxyProbeRound struct {
	ID          uint64                    `json:"id"`
	State       string                    `json:"state"`
	Total       int                       `json:"total"`
	Completed   int                       `json:"completed"`
	Running     int                       `json:"running"`
	Succeeded   int                       `json:"succeeded"`
	Failed      int                       `json:"failed"`
	Concurrency int                       `json:"concurrency"`
	StartedAt   time.Time                 `json:"started_at"`
	FinishedAt  time.Time                 `json:"finished_at,omitempty"`
	Items       map[string]proxyProbeItem `json:"items"`
	generation  uint64
	cancel      context.CancelFunc
}

func publicProxyEntryURL(e proxyPoolEntry) string {
	if e.ProtocolAuto && e.DetectedProtocol == "" {
		u, err := url.Parse(e.RawURL)
		if err == nil {
			u.Scheme = "auto"
			return maskSecrets(u.String())
		}
	}
	return maskSecrets(e.RawURL)
}

// Interactive probes have two reserved slots and never wait behind the batch.
func (s *Server) beginInteractiveProxyProbe(w http.ResponseWriter) (func(), bool) {
	if !s.beginProxyCallback() {
		writeErr(w, 503, "服务正在停止")
		return nil, false
	}
	s.proxyInteractiveOnce.Do(func() { s.proxyInteractiveSem = make(chan struct{}, 2) })
	select {
	case s.proxyInteractiveSem <- struct{}{}:
		return func() { <-s.proxyInteractiveSem; s.proxyCallbackWG.Done() }, true
	default:
		s.proxyCallbackWG.Done()
		writeErr(w, 429, "已有2条单独测试或诊断正在执行，请稍后重试")
		return nil, false
	}
}

// Progress contains only visible IDs (at most one UI page). It never contains
// endpoint credentials or the full pool and is not a durable health snapshot.
func (s *Server) getProxyProbeProgress(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	out := proxyProbeRound{State: "idle", Items: map[string]proxyProbeItem{}, Concurrency: proxyPoolMaxProbeConcurrency}
	if s.m != nil && s.m.proxyPool != nil {
		pool := s.m.proxyPool
		pool.mu.Lock()
		if current := pool.probeRound; current != nil {
			out = *current
			out.Items = map[string]proxyProbeItem{}
			if current.generation != pool.generation {
				out.State = "superseded"
			} else {
				ids := strings.Split(r.URL.Query().Get("ids"), ",")
				if len(ids) > 50 {
					pool.mu.Unlock()
					writeErr(w, 400, "一次最多查询50条代理进度")
					return
				}
				for _, id := range ids {
					if item, ok := current.Items[id]; ok {
						out.Items[id] = item
					}
				}
			}
		}
		pool.mu.Unlock()
	}
	writeJSON(w, 200, out)
}

func (s *Server) cancelProxyPoolTests(w http.ResponseWriter, r *http.Request) {
	if s.m != nil && s.m.proxyPool != nil {
		pool := s.m.proxyPool
		pool.mu.Lock()
		pool.forceHealth = false
		pool.probeCancelGeneration++
		pool.lastHealth = time.Now()
		if round := pool.probeRound; round != nil && (round.State == "queued" || round.State == "running") {
			round.State = "cancelled"
			round.FinishedAt = time.Now()
			if round.cancel != nil {
				round.cancel()
			}
		}
		pool.mu.Unlock()
	}
	writeJSON(w, 200, map[string]any{"ok": true})
}

func (s *Server) startProxyProbeRound(ctx context.Context, entries []proxyPoolEntry, generation, cancelGeneration uint64) (context.Context, *proxyProbeRound, []proxyPoolEntry) {
	ctx, cancel := context.WithCancel(ctx)
	pool := s.m.proxyPool
	pool.mu.Lock()
	defer pool.mu.Unlock()
	// Cancellation can arrive after eligibility was captured but before workers
	// start. That earlier snapshot cannot replace the cancelled round.
	if pool.generation != generation || pool.probeCancelGeneration != cancelGeneration {
		cancel()
		return ctx, &proxyProbeRound{State: "cancelled", cancel: cancel}, nil
	}
	pool.roundSequence++
	round := &proxyProbeRound{ID: pool.roundSequence, State: "running", Total: len(entries), Concurrency: proxyPoolMaxProbeConcurrency, StartedAt: time.Now(), Items: map[string]proxyProbeItem{}, generation: generation, cancel: cancel}
	index := map[string]int{}
	for i, e := range pool.entries {
		index[e.ID] = i
	}
	for i := range entries {
		pool.probeSequence++
		entries[i].probeRevision = pool.probeSequence
		if n, ok := index[entries[i].ID]; ok {
			pool.entries[n].probeRevision = pool.probeSequence
		}
		round.Items[entries[i].ID] = proxyProbeItem{State: "queued", revision: pool.probeSequence}
	}
	pool.probeRound = round
	return ctx, round, entries
}

func (s *Server) updateProxyProbeProgress(round *proxyProbeRound, e proxyPoolEntry, res *egressProbeResult) {
	pool := s.m.proxyPool
	pool.mu.Lock()
	defer pool.mu.Unlock()
	if pool.probeRound != round || round.State != "running" {
		return
	}
	item := round.Items[e.ID]
	superseded := item.State == "superseded"
	if res == nil {
		item.State = "running"
		round.Running++
	} else {
		item.State = "done"
		item.OK = res.OK
		item.Error = res.Error
		item.IP = res.ExitIP
		item.Protocol = res.Via
		item.LatencyMS = res.LatencyMS
		round.Running--
		round.Completed++
		if res.OK {
			round.Succeeded++
		} else {
			round.Failed++
		}
	}
	if superseded {
		item.State = "superseded"
	}
	round.Items[e.ID] = item
}

func (s *Server) finishProxyProbeRound(round *proxyProbeRound, state string) {
	pool := s.m.proxyPool
	pool.mu.Lock()
	defer pool.mu.Unlock()
	if round.State == "running" {
		round.State = state
		round.FinishedAt = time.Now()
	}
	round.Running = 0
	if round.cancel != nil {
		round.cancel()
	}
}

// Each entry has one wall-clock budget including echo fallback. For an inferred
// protocol, race only the supported protocols and accept a verified HTTPS echo.
// An explicit protocol is never silently changed and TLS validation stays on.
func (s *Server) probePoolEntry(parent context.Context, e proxyPoolEntry) (egressProbeResult, string) {
	ctx, cancel := context.WithTimeout(parent, proxyPoolProbeTimeout)
	defer cancel()
	if !e.ProtocolAuto || e.DetectedProtocol != "" {
		return s.runProxyProbe(ctx, e.RawURL, "", defaultEgressProbes, 5*time.Second), ""
	}
	type answer struct {
		result egressProbeResult
		raw    string
	}
	answers := make(chan answer, 3)
	u, err := url.Parse(e.RawURL)
	if err != nil {
		return egressProbeResult{Error: "代理地址格式无效", ErrorCode: "invalid_proxy_url", Probes: []egressAttempt{}}, ""
	}
	for _, scheme := range []string{"http", "https", "socks5"} {
		candidate := *u
		candidate.Scheme = scheme
		go func(raw string) {
			answers <- answer{s.runProxyProbe(ctx, raw, "", defaultEgressProbes, 5*time.Second), raw}
		}(candidate.String())
	}
	var winner *answer
	for range 3 {
		a := <-answers
		if a.result.OK && winner == nil {
			winner = &a
			cancel()
		}
	}
	if winner != nil {
		return winner.result, winner.raw
	}
	return egressProbeResult{Error: "协议尚未确认：HTTP、HTTPS、SOCKS5 均未完成有效的 HTTPS 出口验证；可能离线、需要认证或使用其它协议", ErrorCode: "protocol_unidentified", Probes: []egressAttempt{}}, ""
}
