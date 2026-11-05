package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"math/rand"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/Autumn-27/artex/traffic"
)

const (
	settingProxyPool             = "proxy_pool_entries"
	settingProxyPoolActive       = "proxy_pool_active_id"
	settingProxyRotateTimed      = "proxy_rotate_timed_enabled"
	settingProxyRotateInterval   = "proxy_rotate_interval_seconds"
	settingProxyRotateFailure    = "proxy_rotate_on_failure"
	settingProxyRotateStrategy   = "proxy_rotate_strategy"
	settingProxyHealthThreshold  = "proxy_health_fail_threshold"
	settingProxyHealthCooldown   = "proxy_health_cooldown_seconds"
	settingProxyHealthInterval   = "proxy_health_check_interval_seconds"
	defaultProxyRotateInterval   = 30
	minProxyRotateInterval       = 3
	defaultProxyHealthThreshold  = 3
	defaultProxyHealthCooldown   = 60
	defaultProxyHealthInterval   = 30
	proxyPoolProbeTimeout        = 10 * time.Second
	proxyPoolMaxProbeConcurrency = 4
)

// ProxyPoolEntry is the public shape. URL is always masked in API responses.
type ProxyPoolEntry struct {
	ID                  string    `json:"id"`
	URL                 string    `json:"url"`
	Name                string    `json:"name,omitempty"`
	Remark              string    `json:"remark,omitempty"`
	Enabled             bool      `json:"enabled"`
	EgressIP            string    `json:"egress_ip,omitempty"`
	LatencyMS           int64     `json:"latency_ms,omitempty"`
	LastError           string    `json:"last_error,omitempty"`
	LastCheckedAt       time.Time `json:"last_checked_at,omitempty"`
	ConsecutiveFailures int       `json:"consecutive_failures"`
	Health              string    `json:"health"`
}

type proxyPoolEntry struct {
	ProxyPoolEntry
	RawURL string `json:"raw_url"`
}

type ProxySwitchLog struct {
	At     time.Time `json:"at"`
	From   string    `json:"from"`
	To     string    `json:"to"`
	Reason string    `json:"reason"`
}

type proxyPoolPersist struct {
	Entries []proxyPoolEntry `json:"entries"`
	Active  string           `json:"active"`
}

type ProxyImportError struct {
	Line  int    `json:"line"`
	Input string `json:"input"`
	Error string `json:"error"`
	Kind  string `json:"kind,omitempty"`
}

func proxyURLKey(u *url.URL) string {
	if u == nil {
		return ""
	}
	v := *u
	v.Host = strings.ToLower(v.Host)
	v.Scheme = strings.ToLower(v.Scheme)
	return v.String()
}

func proxyURLKeyRaw(raw string) string {
	u, err := traffic.ValidateProxyURL(raw)
	if err != nil {
		return strings.ToLower(strings.TrimSpace(raw))
	}
	return proxyURLKey(u)
}

type proxyPoolState struct {
	mu         sync.Mutex
	entries    []proxyPoolEntry
	active     string
	logs       []ProxySwitchLog
	lastTimed  time.Time
	lastHealth time.Time
	// Configuration changes invalidate a whole in-flight probe round. A result
	// for an old endpoint must not bless a newly edited/re-enabled member.
	generation  uint64
	forceHealth bool
}

type proxyPoolTickOutcome struct {
	confirmed bool
	switched  bool
}

var errProxyPoolChanged = errors.New("代理配置已变更，请刷新后重试导入")

// decodeProxyPoolStartup accepts only a known persistence shape. A read or JSON
// failure is not evidence that the operator cleared the pool.
func decodeProxyPoolStartup(values map[string]string) (proxyPoolPersist, string, bool, error) {
	var pool proxyPoolPersist
	raw, present := values[settingProxyPool]
	global := strings.TrimSpace(values[settingGlobalProxy])
	if !present {
		if global == "" {
			return pool, "", false, nil
		}
		if _, err := traffic.ValidateProxyURL(global); err != nil {
			return pool, "", false, errors.New("saved global proxy is invalid; route was not changed")
		}
		pool.Entries = []proxyPoolEntry{{ProxyPoolEntry: ProxyPoolEntry{ID: "proxy-legacy", URL: maskSecrets(global), Enabled: true, Health: "healthy"}, RawURL: global}}
		pool.Active = "proxy-legacy"
		return pool, global, true, nil
	}
	var shape map[string]json.RawMessage
	if json.Unmarshal([]byte(raw), &shape) != nil || shape == nil || shape["entries"] == nil {
		return pool, "", false, errors.New("saved proxy pool is invalid; route was not changed")
	}
	if err := json.Unmarshal([]byte(raw), &pool); err != nil {
		return pool, "", false, errors.New("saved proxy pool has invalid fields; route was not changed")
	}
	seen := make(map[string]bool, len(pool.Entries))
	for i := range pool.Entries {
		e := &pool.Entries[i]
		if e.ID == "" || seen[e.ID] {
			return pool, "", false, errors.New("saved proxy pool has invalid or duplicate member IDs; route was not changed")
		}
		seen[e.ID] = true
		e.RawURL = strings.TrimSpace(e.RawURL)
		if e.RawURL != "" {
			if _, err := traffic.ValidateProxyURL(e.RawURL); err != nil {
				return pool, "", false, errors.New("saved proxy pool contains an invalid endpoint; route was not changed")
			}
			if e.Health == "" {
				e.Health = "unknown"
			}
		}
		e.URL = maskSecrets(e.RawURL)
	}
	// Prefer the previously active usable member, even while cooling. If it was
	// explicitly disabled/cleared, choose another enabled endpoint, not that URL.
	chosen := ""
	for _, e := range pool.Entries {
		if e.ID == pool.Active && e.Enabled && e.RawURL != "" {
			chosen = e.ID
			break
		}
	}
	if chosen == "" {
		for _, e := range pool.Entries {
			if e.Enabled && e.RawURL != "" && e.Health != "unhealthy-cooling" {
				chosen = e.ID
				break
			}
		}
	}
	if chosen == "" {
		for _, e := range pool.Entries {
			if e.Enabled && e.RawURL != "" {
				chosen = e.ID
				break
			}
		}
	}
	pool.Active, global = chosen, ""
	for _, e := range pool.Entries {
		if e.ID == chosen {
			global = e.RawURL
			break
		}
	}
	return pool, global, true, nil
}

func (m *Manager) initProxyPool() error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	// Read the coupled settings in one statement snapshot, not independent reads
	// that can straddle a committed route switch.
	rows, err := m.pg.QueryContext(ctx, `SELECT key,value FROM settings WHERE key IN ($1,$2,$3)`, settingProxyPool, settingProxyPoolActive, settingGlobalProxy)
	if err != nil {
		return fmt.Errorf("read saved proxy route: %w", err)
	}
	values := map[string]string{}
	for rows.Next() {
		var key, value string
		if err := rows.Scan(&key, &value); err != nil {
			rows.Close()
			return fmt.Errorf("read saved proxy route: %w", err)
		}
		values[key] = value
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return fmt.Errorf("read saved proxy route: %w", err)
	}
	pool, global, persist, err := decodeProxyPoolStartup(values)
	if err != nil {
		return err
	}
	if persist {
		if err := m.persistProxyRouteSnapshot(pool, global); err != nil {
			return fmt.Errorf("persist saved proxy route: %w", err)
		}
	}
	// Publish only a successfully loaded and committed snapshot.
	m.proxyPool = &proxyPoolState{entries: pool.Entries, active: pool.Active}
	m.mu.Lock()
	m.globalProxy = global
	m.mu.Unlock()
	m.proxyPoolConfigured.Store(len(pool.Entries) > 0)
	return nil
}

func (m *Manager) persistProxyRouteSnapshot(pool proxyPoolPersist, global string) error {
	if m.pg == nil {
		return nil
	}
	b, err := json.Marshal(pool)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return m.pg.SetSettingsAtomic(ctx, map[string]string{
		settingProxyPool: string(b), settingProxyPoolActive: pool.Active, settingGlobalProxy: global,
	})
}

func (m *Manager) persistProxyPoolLocked() error {
	return m.persistProxyRouteSnapshot(proxyPoolPersist{Entries: m.proxyPool.entries, Active: m.proxyPool.active}, m.GlobalProxy())
}

// A mutating API prepares its candidate under the pool lock, but must restore
// that candidate before unlocking if the durable commit was rejected.
func (m *Manager) proxyPoolRollbackLocked() func() {
	entries := append([]proxyPoolEntry(nil), m.proxyPool.entries...)
	active := m.proxyPool.active
	configured := m.proxyPoolConfigured.Load()
	return func() {
		m.proxyPool.entries, m.proxyPool.active = entries, active
		m.proxyPoolConfigured.Store(configured)
	}
}

func (m *Manager) ProxyPoolSnapshot() ([]ProxyPoolEntry, string, []ProxySwitchLog) {
	if m.proxyPool == nil {
		return []ProxyPoolEntry{}, "", []ProxySwitchLog{}
	}
	m.proxyPool.mu.Lock()
	snapshot := append([]proxyPoolEntry(nil), m.proxyPool.entries...)
	active := m.proxyPool.active
	logs := append([]ProxySwitchLog{}, m.proxyPool.logs...)
	m.proxyPool.mu.Unlock()
	// Copy under the lock, but do not hold it while regex-masking a large pool.
	entries := make([]ProxyPoolEntry, 0, len(snapshot))
	for _, e := range snapshot {
		p := e.ProxyPoolEntry
		p.URL = maskSecrets(e.RawURL)
		p.LastError = maskSecrets(p.LastError)
		if !p.Enabled {
			p.Health = "disabled"
		}
		entries = append(entries, p)
	}
	for i := range logs {
		logs[i].From, logs[i].To = maskSecrets(logs[i].From), maskSecrets(logs[i].To)
	}
	return entries, active, logs
}

func (m *Manager) ActiveProxy() string { return m.GlobalProxy() }

func (m *Manager) proxyPoolEntries() []proxyPoolEntry {
	if m.proxyPool == nil {
		return nil
	}
	m.proxyPool.mu.Lock()
	defer m.proxyPool.mu.Unlock()
	return append([]proxyPoolEntry(nil), m.proxyPool.entries...)
}

func (m *Manager) setProxyPoolEntries(entries []proxyPoolEntry, active string, expectedGeneration ...uint64) (err error) {
	if m.proxyPool == nil {
		m.proxyPool = &proxyPoolState{}
	}
	m.proxyPool.mu.Lock()
	defer m.proxyPool.mu.Unlock()
	if len(expectedGeneration) > 0 && m.proxyPool.generation != expectedGeneration[0] {
		return errProxyPoolChanged
	}
	rollback := m.proxyPoolRollbackLocked()
	defer func() {
		if err != nil {
			rollback()
		} else {
			m.proxyPool.generation++
		}
	}()
	m.proxyPool.entries = append([]proxyPoolEntry(nil), entries...)
	if active != "" {
		return m.applyActiveProxyLockedWithHealth(active, "manual", true)
	}
	for _, e := range entries {
		if e.Enabled && e.RawURL != "" && e.Health != "unhealthy-cooling" {
			return m.applyActiveProxyLocked(e.ID, "reconnect")
		}
	}
	for _, e := range entries {
		if e.Enabled && e.RawURL != "" {
			return m.applyActiveProxyLockedWithHealth(e.ID, "reconnect", true)
		}
	}
	return m.applyActiveProxyLocked("", "reconnect")
}

func (m *Manager) applyActiveProxyLocked(id, reason string) error {
	return m.applyActiveProxyLockedWithHealth(id, reason, false)
}

func (m *Manager) applyActiveProxyLockedWithHealth(id, reason string, allowUnhealthy bool) error {
	var next string
	for _, e := range m.proxyPool.entries {
		if e.ID == id && e.Enabled && e.RawURL != "" && (allowUnhealthy || e.Health != "unhealthy-cooling") {
			next = e.RawURL
			break
		}
	}
	if id != "" && next == "" {
		return errors.New("代理未启用或不健康")
	}
	// Validation is pure and identical to Traffic.SetUpstreamProxy's validation.
	// Finish it before committing; no runtime route changes on validation/DB error.
	if next != "" {
		if _, err := traffic.ValidateProxyURL(next); err != nil {
			return errors.New("代理地址无效，原路线保持不变")
		}
	}
	if err := m.persistProxyRouteSnapshot(proxyPoolPersist{Entries: m.proxyPool.entries, Active: id}, next); err != nil {
		return err
	}
	from := m.GlobalProxy()
	if m.traffic != nil {
		_ = m.traffic.SetUpstreamProxy(next) // the exact string was validated above
	}
	m.mu.Lock()
	m.globalProxy = next
	m.mu.Unlock()
	m.proxyPool.active = id
	m.proxyPoolConfigured.Store(len(m.proxyPool.entries) > 0)
	m.refreshTaskProxyRoutes()
	if from != next {
		m.proxyPool.logs = append(m.proxyPool.logs, ProxySwitchLog{At: time.Now(), From: maskSecrets(from), To: maskSecrets(next), Reason: reason})
		if len(m.proxyPool.logs) > 20 {
			m.proxyPool.logs = m.proxyPool.logs[len(m.proxyPool.logs)-20:]
		}
	}
	if m.pg != nil {
		m.syncBrowserMCPProxy()
	}
	return nil
}

func (m *Manager) selectHealthyLocked(exclude string) string {
	strategy := "sequential"
	if m.pg != nil {
		if v, ok, _ := m.pg.GetSetting(settingProxyRotateStrategy); ok && v == "random" {
			strategy = v
		}
	}
	pool := make([]string, 0)
	for _, e := range m.proxyPool.entries {
		if e.Enabled && e.RawURL != "" && e.Health != "unhealthy-cooling" && e.ID != exclude {
			pool = append(pool, e.ID)
		}
	}
	if len(pool) == 0 && exclude != "" {
		for _, e := range m.proxyPool.entries {
			if e.Enabled && e.RawURL != "" && e.Health != "unhealthy-cooling" {
				pool = append(pool, e.ID)
			}
		}
	}
	if len(pool) == 0 {
		return ""
	}
	if strategy == "random" {
		return pool[rand.Intn(len(pool))]
	}
	start := 0
	for i, e := range m.proxyPool.entries {
		if e.ID == exclude {
			start = i + 1
			break
		}
	}
	for n := 0; n < len(m.proxyPool.entries); n++ {
		e := m.proxyPool.entries[(start+n)%len(m.proxyPool.entries)]
		if e.Enabled && e.RawURL != "" && e.Health != "unhealthy-cooling" && e.ID != exclude {
			return e.ID
		}
	}
	return pool[0]
}

func (m *Manager) rotateProxy(reason string) bool {
	if m.proxyPool == nil {
		return false
	}
	m.proxyPool.mu.Lock()
	defer m.proxyPool.mu.Unlock()
	id := m.selectHealthyLocked(m.proxyPool.active)
	if id == "" || id == m.proxyPool.active {
		return false
	}
	return m.applyActiveProxyLocked(id, reason) == nil
}

func (m *Manager) markProxyProbe(id string, res egressProbeResult) {
	if m.proxyPool == nil {
		return
	}
	m.proxyPool.mu.Lock()
	defer m.proxyPool.mu.Unlock()
	m.markProxyProbeLocked(id, res)
}

func (m *Manager) markProxyProbeLocked(id string, res egressProbeResult) {
	for i := range m.proxyPool.entries {
		e := &m.proxyPool.entries[i]
		if e.ID != id {
			continue
		}
		e.LastCheckedAt, e.LatencyMS, e.EgressIP = time.Now(), res.LatencyMS, res.ExitIP
		if res.OK {
			e.ConsecutiveFailures, e.LastError, e.Health = 0, "", "healthy"
		} else {
			e.ConsecutiveFailures++
			e.LastError = maskSecrets(res.Error)
			threshold := defaultProxyHealthThreshold
			if m.pg != nil {
				threshold = proxyIntSetting(m.pg, settingProxyHealthThreshold, defaultProxyHealthThreshold)
			}
			if e.ConsecutiveFailures >= threshold {
				e.Health = "unhealthy-cooling"
			}
		}
		_ = m.persistProxyPoolLocked()
		return
	}
}

type proxyBatchProbe struct {
	entry    proxyPoolEntry
	result   egressProbeResult
	complete bool
}

// applyProxyProbeBatchLocked publishes one durable health snapshot per round,
// rather than serializing all N members for each of the N results. The caller
// has already joined every worker and checked the configuration generation.
func (m *Manager) applyProxyProbeBatchLocked(results []proxyBatchProbe, threshold int) (bool, error) {
	candidate := append([]proxyPoolEntry(nil), m.proxyPool.entries...)
	index := make(map[string]int, len(candidate))
	for i, e := range candidate {
		index[e.ID] = i
	}
	now := time.Now()
	changed, confirmed := false, false
	for _, probe := range results {
		if !probe.complete {
			return false, errors.New("proxy health round is incomplete")
		}
		i, found := index[probe.entry.ID]
		if !found {
			continue
		}
		e := &candidate[i]
		if !e.Enabled || e.RawURL == "" || e.RawURL != probe.entry.RawURL {
			continue
		}
		res := probe.result
		e.LastCheckedAt, e.LatencyMS, e.EgressIP = now, res.LatencyMS, res.ExitIP
		if res.OK {
			e.ConsecutiveFailures, e.LastError, e.Health = 0, "", "healthy"
			confirmed = confirmed || e.ID == m.proxyPool.active
		} else {
			e.ConsecutiveFailures++
			e.LastError = maskSecrets(res.Error)
			if e.ConsecutiveFailures >= threshold {
				e.Health = "unhealthy-cooling"
			}
		}
		changed = true
	}
	if changed {
		if err := m.persistProxyRouteSnapshot(proxyPoolPersist{Entries: candidate, Active: m.proxyPool.active}, m.GlobalProxy()); err != nil {
			return false, err
		}
		m.proxyPool.entries = candidate
	}
	return confirmed, nil
}

func (s *Server) queueProxyPoolHealth(force bool) bool {
	if s == nil || s.m == nil || s.m.proxyPool == nil || s.proxyPoolWake == nil || s.proxyShuttingDown() {
		return false
	}
	s.m.proxyPool.mu.Lock()
	if s.proxyShuttingDown() {
		s.m.proxyPool.mu.Unlock()
		return false
	}
	s.m.proxyPool.lastHealth = time.Time{}
	s.m.proxyPool.forceHealth = s.m.proxyPool.forceHealth || force
	s.m.proxyPool.mu.Unlock()
	select {
	case s.proxyPoolWake <- struct{}{}:
	default: // An existing wake already schedules the next round.
	}
	return true
}

func (s *Server) proxyPoolGenerationMatches(generation uint64) bool {
	if s.proxyShuttingDown() {
		return false
	}
	s.m.proxyPool.mu.Lock()
	defer s.m.proxyPool.mu.Unlock()
	return s.m.proxyPool.generation == generation
}

func proxyIntSetting(pg interface {
	GetSetting(string) (string, bool, error)
}, key string, def int) int {
	if pg == nil {
		return def
	}
	if v, ok, _ := pg.GetSetting(key); ok {
		var n int
		if _, err := fmt.Sscanf(strings.TrimSpace(v), "%d", &n); err == nil && n > 0 {
			return n
		}
	}
	return def
}

func (m *Manager) proxyHealthConfig() (threshold int, cooldown, interval time.Duration) {
	if m.pg == nil {
		return defaultProxyHealthThreshold, defaultProxyHealthCooldown * time.Second, defaultProxyHealthInterval * time.Second
	}
	return proxyIntSetting(m.pg, settingProxyHealthThreshold, defaultProxyHealthThreshold), time.Duration(proxyIntSetting(m.pg, settingProxyHealthCooldown, defaultProxyHealthCooldown)) * time.Second, time.Duration(proxyIntSetting(m.pg, settingProxyHealthInterval, defaultProxyHealthInterval)) * time.Second
}

func (m *Manager) SetProxyPoolSettings(timed bool, interval int, onFailure bool, strategy string, threshold, cooldown, healthInterval int) error {
	if interval < minProxyRotateInterval {
		return fmt.Errorf("定时轮换间隔不能小于 %d 秒", minProxyRotateInterval)
	}
	if strategy != "sequential" && strategy != "random" {
		return errors.New("proxy_rotate_strategy 必须是 sequential 或 random")
	}
	if threshold < 1 || cooldown < 1 || healthInterval < 1 {
		return errors.New("健康检查参数必须大于 0")
	}
	return m.pg.SetSettingsAtomic(context.Background(), map[string]string{settingProxyRotateTimed: fmt.Sprint(timed), settingProxyRotateInterval: fmt.Sprint(interval), settingProxyRotateFailure: fmt.Sprint(onFailure), settingProxyRotateStrategy: strategy, settingProxyHealthThreshold: fmt.Sprint(threshold), settingProxyHealthCooldown: fmt.Sprint(cooldown), settingProxyHealthInterval: fmt.Sprint(healthInterval)})
}

func (m *Manager) proxyRotateConfig() (bool, time.Duration, bool, string) {
	if m.pg == nil {
		return false, defaultProxyRotateInterval * time.Second, false, "sequential"
	}
	return m.pg.GetBool(settingProxyRotateTimed, false), time.Duration(proxyIntSetting(m.pg, settingProxyRotateInterval, defaultProxyRotateInterval)) * time.Second, m.pg.GetBool(settingProxyRotateFailure, false), func() string {
		v, ok, _ := m.pg.GetSetting(settingProxyRotateStrategy)
		if ok && v == "random" {
			return v
		}
		return "sequential"
	}()
}

func (s *Server) runProxyPoolSupervisor(ctx context.Context) {
	for {
		_, rotateInterval, _, _ := s.m.proxyRotateConfig()
		_, _, healthInterval := s.m.proxyHealthConfig()
		if rotateInterval <= 0 {
			rotateInterval = defaultProxyRotateInterval * time.Second
		}
		if healthInterval <= 0 {
			healthInterval = defaultProxyHealthInterval * time.Second
		}
		d := healthInterval
		if timed, _, _, _ := s.m.proxyRotateConfig(); timed && rotateInterval < d {
			d = rotateInterval
		}
		if d <= 0 {
			d = time.Second
		}
		t := time.NewTimer(d)
		select {
		case <-ctx.Done():
			if !t.Stop() {
				select {
				case <-t.C:
				default:
				}
			}
			return
		case <-t.C:
			s.proxyPoolTick()
		case <-s.proxyPoolWake:
			if !t.Stop() {
				select {
				case <-t.C:
				default:
				}
			}
			s.proxyPoolTick()
		}
	}
}

func (s *Server) proxyPoolTick() proxyPoolTickOutcome {
	if s == nil {
		return proxyPoolTickOutcome{}
	}
	s.proxyPoolTickMu.Lock()
	defer s.proxyPoolTickMu.Unlock()
	return s.proxyPoolTickLocked(false)
}

func (s *Server) proxyPoolTickLocked(forceHealth bool) proxyPoolTickOutcome {
	var outcome proxyPoolTickOutcome
	if s.m == nil || s.m.proxyPool == nil || s.proxyShuttingDown() {
		return outcome
	}
	now := time.Now()
	threshold, cooldown, healthInterval := s.m.proxyHealthConfig()
	s.m.proxyPool.mu.Lock()
	forceHealth = forceHealth || s.m.proxyPool.forceHealth
	s.m.proxyPool.forceHealth = false
	healthDue := s.m.proxyPool.lastHealth.IsZero() || now.Sub(s.m.proxyPool.lastHealth) >= healthInterval
	if !healthDue {
		// A cooling member becomes probe-eligible at its own cooldown deadline;
		// do not make it wait for the next full health-check interval.
		for _, e := range s.m.proxyPool.entries {
			if e.Enabled && strings.TrimSpace(e.RawURL) != "" && e.Health == "unhealthy-cooling" && (e.LastCheckedAt.IsZero() || now.Sub(e.LastCheckedAt) >= cooldown) {
				healthDue = true
				break
			}
		}
	}
	if healthDue {
		s.m.proxyPool.lastHealth = now
	}
	// Eligibility, force intent and configuration provenance share one snapshot.
	var entries []proxyPoolEntry
	generation, activeID := s.m.proxyPool.generation, s.m.proxyPool.active
	if healthDue || forceHealth {
		entries = append([]proxyPoolEntry(nil), s.m.proxyPool.entries...)
	}
	s.m.proxyPool.mu.Unlock()
	if healthDue || forceHealth {
		ctx := serverContext(s)
		eligible := make([]proxyPoolEntry, 0, len(entries))
		for _, e := range entries {
			if !e.Enabled || strings.TrimSpace(e.RawURL) == "" {
				continue
			}
			if !forceHealth && e.Health == "unhealthy-cooling" && e.ConsecutiveFailures >= threshold && !e.LastCheckedAt.IsZero() && now.Sub(e.LastCheckedAt) < cooldown {
				continue
			}
			eligible = append(eligible, e)
		}
		// Check the active route first, without claiming early recovery before
		// the complete batch is available for the existing reconciliation rules.
		for i, e := range eligible {
			if e.ID == activeID {
				copy(eligible[1:i+1], eligible[:i])
				eligible[0] = e
				break
			}
		}
		sem := s.proxyPoolProbeSem
		if sem == nil {
			sem = make(chan struct{}, proxyPoolMaxProbeConcurrency)
		}
		results := make([]proxyBatchProbe, len(eligible))
		jobs := make(chan int)
		var wg sync.WaitGroup
		// Fixed workers bound both network concurrency and waiting goroutines.
		for i := 0; i < min(proxyPoolMaxProbeConcurrency, len(eligible)); i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for n := range jobs {
					if ctx.Err() != nil || !s.proxyPoolGenerationMatches(generation) {
						continue
					}
					select {
					case sem <- struct{}{}:
					case <-ctx.Done():
						return
					}
					if ctx.Err() != nil || !s.proxyPoolGenerationMatches(generation) {
						<-sem
						continue
					}
					res := s.runProxyProbe(ctx, eligible[n].RawURL, "", defaultEgressProbes, proxyPoolProbeTimeout)
					<-sem
					results[n] = proxyBatchProbe{entry: eligible[n], result: res, complete: true}
				}
			}()
		}
	feed:
		for n := range eligible {
			if ctx.Err() != nil || !s.proxyPoolGenerationMatches(generation) {
				break
			}
			select {
			case jobs <- n:
			case <-ctx.Done():
				break feed
			}
		}
		close(jobs)
		wg.Wait()
		// A canceled or superseded round is not evidence of proxy failure.
		if ctx.Err() != nil || s.proxyShuttingDown() {
			return proxyPoolTickOutcome{}
		}
		s.m.proxyPool.mu.Lock()
		if ctx.Err() != nil || s.proxyShuttingDown() {
			s.m.proxyPool.mu.Unlock()
			return proxyPoolTickOutcome{}
		}
		if s.m.proxyPool.generation != generation {
			s.m.proxyPool.mu.Unlock()
			// Editing configuration supersedes evidence, not a pending manual
			// test-all. Requeue its force intent for the current configuration.
			if forceHealth {
				s.queueProxyPoolHealth(true)
			}
			return proxyPoolTickOutcome{}
		}
		confirmed, err := s.m.applyProxyProbeBatchLocked(results, threshold)
		s.m.proxyPool.mu.Unlock()
		if err != nil {
			// Persist failures do not authorize a route change or an all-pool pause.
			// Do not log DB errors here: they may contain the persisted proxy JSON.
			log.Printf("[proxy] health round not saved; route kept unchanged")
			return proxyPoolTickOutcome{}
		}
		outcome.confirmed = confirmed
		if s.reconcileProxyPoolAfterProbe(confirmed, generation) {
			outcome.switched = true
		}
	}

	// Health reconciliation runs before timed rotation. This matters when an
	// import starts a fresh probe round: tasks are not paused or rerouted until
	// the new members have been checked. A successful recovery switch already
	// selected the best available member for this round, so avoid immediately
	// rotating away from it.
	timed, rotateInterval, _, _ := s.m.proxyRotateConfig()
	if s.proxyShuttingDown() {
		return proxyPoolTickOutcome{}
	}
	if timed && !outcome.switched {
		s.m.proxyPool.mu.Lock()
		if s.proxyShuttingDown() {
			s.m.proxyPool.mu.Unlock()
			return proxyPoolTickOutcome{}
		}
		due := s.m.proxyPool.lastTimed.IsZero() || now.Sub(s.m.proxyPool.lastTimed) >= rotateInterval
		if due {
			s.m.proxyPool.lastTimed = now
		}
		s.m.proxyPool.mu.Unlock()
		if due && !s.proxyShuttingDown() {
			s.m.proxyPool.mu.Lock()
			if !s.proxyShuttingDown() {
				id := s.m.selectHealthyLocked(s.m.proxyPool.active)
				if id != "" && id != s.m.proxyPool.active {
					outcome.switched = s.m.applyActiveProxyLocked(id, "timed") == nil
				}
			}
			s.m.proxyPool.mu.Unlock()
			if outcome.switched {
				s.proxyReconnectSuccess()
			}
		}
	}
	return outcome
}

func serverContext(s *Server) context.Context {
	if s != nil && s.proxyCtx != nil {
		return s.proxyCtx
	}
	if s != nil && s.ctx != nil {
		return s.ctx
	}
	return context.Background()
}

// reconcileProxyPoolAfterProbe never holds the pool mutex while mutating task
// lifecycle state. A probe caller supplies its configuration generation so a
// newer import/edit/manual activation invalidates its downstream decision.
func (s *Server) reconcileProxyPoolAfterProbe(activeConfirmed bool, expectedGeneration ...uint64) bool {
	if s.proxyShuttingDown() {
		return false
	}
	s.m.proxyPool.mu.Lock()
	generation := s.m.proxyPool.generation
	if len(expectedGeneration) > 0 && generation != expectedGeneration[0] {
		s.m.proxyPool.mu.Unlock()
		return false
	}
	activeID := s.m.proxyPool.active
	activeAvailable := false
	for _, e := range s.m.proxyPool.entries {
		if e.ID == activeID && e.Enabled && e.RawURL != "" && e.Health != "unhealthy-cooling" {
			activeAvailable = true
			break
		}
	}
	if activeAvailable {
		s.m.proxyPool.mu.Unlock()
		if activeConfirmed && s.proxyPoolGenerationMatches(generation) {
			s.proxyReconnectSuccess()
		}
		return false
	}
	id := s.m.selectHealthyLocked(activeID)
	var err error
	if id != "" && !s.proxyShuttingDown() {
		// Validation, durable route commit and publication share the pool lock.
		err = s.m.applyActiveProxyLocked(id, "reconnect")
	} else if s.proxyShuttingDown() {
		s.m.proxyPool.mu.Unlock()
		return false
	}
	allCooling, enabled := false, 0
	for _, e := range s.m.proxyPool.entries {
		if e.Enabled && e.RawURL != "" {
			enabled++
			if e.Health == "unhealthy-cooling" {
				allCooling = true
			} else {
				allCooling = false
				break
			}
		}
	}
	configured := activeID != "" || strings.TrimSpace(s.m.GlobalProxy()) != ""
	s.m.proxyPool.mu.Unlock()
	if !s.proxyPoolGenerationMatches(generation) {
		return false
	}
	if err == nil && id != "" {
		s.proxyReconnectSuccess()
		return true
	}
	if id == "" && enabled > 0 && allCooling && configured {
		s.pauseAllProxyTasks(errors.New("代理池暂无健康成员"))
	}
	return false
}

func (m *Manager) activateProxyID(id, reason string) error {
	if m.proxyPool == nil {
		return errors.New("代理池未初始化")
	}
	m.proxyPool.mu.Lock()
	defer m.proxyPool.mu.Unlock()
	before := m.proxyPool.active
	if err := m.applyActiveProxyLocked(id, reason); err != nil {
		return err
	}
	if before != m.proxyPool.active {
		m.proxyPool.generation++
	}
	return nil
}

func (s *Server) handleProxyPoolFailure(cause error) bool {
	if s == nil || s.m == nil || s.m.proxyPool == nil || !isGlobalProxyDialFailure(cause) {
		return false
	}
	cur := strings.TrimSpace(s.m.GlobalProxy())
	if cur == "" {
		return false
	}
	res := s.runProxyProbe(serverContext(s), cur, "", defaultEgressProbes, proxyPoolProbeTimeout)
	if s.proxyShuttingDown() {
		return true
	}
	if res.OK {
		return false
	}
	_, _, onFailure, _ := s.m.proxyRotateConfig()
	s.proxyPoolTickMu.Lock()
	if s.proxyShuttingDown() {
		s.proxyPoolTickMu.Unlock()
		return true
	}
	s.m.proxyPool.mu.Lock()
	if s.proxyShuttingDown() {
		s.m.proxyPool.mu.Unlock()
		s.proxyPoolTickMu.Unlock()
		return true
	}
	active := s.m.proxyPool.active
	currentChanged := s.m.GlobalProxy() != cur
	for _, e := range s.m.proxyPool.entries {
		if e.RawURL == cur {
			s.m.markProxyProbeLocked(e.ID, res)
			break
		}
	}
	if currentChanged {
		s.m.proxyPool.mu.Unlock()
		s.proxyPoolTickMu.Unlock()
		return true
	}
	if onFailure {
		if id := s.m.selectHealthyLocked(active); id != "" && id != active {
			if err := s.m.applyActiveProxyLocked(id, "failure"); err == nil {
				s.m.proxyPool.mu.Unlock()
				s.proxyPoolTickMu.Unlock()
				s.proxyReconnectSuccess()
				return true
			}
		}
	}
	select {
	case s.proxyPoolWake <- struct{}{}:
	default:
	}
	s.m.proxyPool.mu.Unlock()
	s.proxyPoolTickMu.Unlock()
	return false
}

func (s *Server) pauseAllProxyTasks(cause error) {
	for _, t := range s.m.List() {
		st := t.lifecycleSnapshot()
		if isTerminalStatus(st.Status) || st.Paused || (s.engine != nil && s.engine.IsDeleting(t.ID)) {
			continue
		}
		s.pauseProxyTask(t.ID, cause)
	}
}

func (s *Server) getProxyPool(w http.ResponseWriter, r *http.Request) {
	entries, active, logs := s.m.ProxyPoolSnapshot()
	timed, interval, onFailure, strategy := s.m.proxyRotateConfig()
	threshold, cooldown, healthInterval := s.m.proxyHealthConfig()
	writeJSON(w, 200, map[string]any{"entries": entries, "active_id": active, "active_proxy": maskSecrets(s.m.GlobalProxy()), "switch_log": logs, "timed_enabled": timed, "interval_seconds": int(interval / time.Second), "on_failure": onFailure, "strategy": strategy, "health_fail_threshold": threshold, "health_cooldown_seconds": int(cooldown / time.Second), "health_check_interval_seconds": int(healthInterval / time.Second)})
}

func (s *Server) importProxyPool(w http.ResponseWriter, r *http.Request) {
	if s.proxyShuttingDown() {
		writeErr(w, http.StatusServiceUnavailable, "服务正在停止，代理未导入")
		return
	}
	var req struct {
		Text           string  `json:"text"`
		TimedEnabled   *bool   `json:"timed_enabled"`
		Interval       *int    `json:"interval_seconds"`
		OnFailure      *bool   `json:"on_failure"`
		Strategy       *string `json:"strategy"`
		Threshold      *int    `json:"health_fail_threshold"`
		Cooldown       *int    `json:"health_cooldown_seconds"`
		HealthInterval *int    `json:"health_check_interval_seconds"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	parsed, errs := ParseProxyBatch(req.Text)
	if errs == nil {
		errs = []ProxyImportError{}
	}
	var entries []proxyPoolEntry
	var generation uint64
	active := ""
	if s.m.proxyPool != nil {
		s.m.proxyPool.mu.Lock()
		entries = append([]proxyPoolEntry(nil), s.m.proxyPool.entries...)
		generation, active = s.m.proxyPool.generation, s.m.proxyPool.active
		s.m.proxyPool.mu.Unlock()
	}
	seen := map[string]bool{}
	for _, e := range entries {
		seen[proxyURLKeyRaw(e.RawURL)] = true
	}
	imported := 0
	for _, p := range parsed {
		u, _ := traffic.ValidateProxyURL(p.URL)
		key := proxyURLKey(u)
		if seen[key] {
			line := 0
			if i := strings.LastIndex(p.ID, "-"); i >= 0 {
				_, _ = fmt.Sscanf(p.ID[i+1:], "%d", &line)
			}
			errs = append(errs, ProxyImportError{Line: line, Input: proxyImportDisplayURL(u), Error: "重复代理，已跳过", Kind: "duplicate"})
			continue
		}
		entries = append(entries, proxyPoolEntry{ProxyPoolEntry: p, RawURL: u.String()})
		imported++
		seen[proxyURLKey(u)] = true
	}
	duplicates, invalid := 0, 0
	for _, item := range errs {
		if item.Kind == "duplicate" {
			duplicates++
		} else {
			invalid++
		}
	}
	respond := func(probeQueued bool) {
		result := map[string]any{"ok": true, "imported": imported, "skipped_duplicates": duplicates, "invalid": invalid, "errors": errs, "probe_queued": probeQueued}
		if r.URL.Query().Get("summary") == "1" {
			activeOut, count := "", 0
			if s.m.proxyPool != nil {
				s.m.proxyPool.mu.Lock()
				activeOut, count = s.m.proxyPool.active, len(s.m.proxyPool.entries)
				s.m.proxyPool.mu.Unlock()
			}
			result["active_id"], result["count"] = activeOut, count
		} else {
			entriesOut, activeOut, logs := s.m.ProxyPoolSnapshot()
			result["entries"], result["active_id"], result["switch_log"] = entriesOut, activeOut, logs
		}
		writeJSON(w, http.StatusOK, result)
	}
	policyUpdate := req.Interval != nil || req.TimedEnabled != nil || req.OnFailure != nil || req.Strategy != nil || req.Threshold != nil || req.Cooldown != nil || req.HealthInterval != nil
	if imported == 0 && !policyUpdate {
		if s.m.proxyPool != nil {
			s.m.proxyPool.mu.Lock()
			changed := s.m.proxyPool.generation != generation
			s.m.proxyPool.mu.Unlock()
			if changed {
				writeErr(w, http.StatusConflict, errProxyPoolChanged.Error())
				return
			}
		}
		// Invalid-only and duplicate-only pastes do not rewrite the route,
		// invalidate health rounds, or probe thousands of existing members.
		respond(false)
		return
	}
	if active != "" {
		foundActive := false
		for _, e := range entries {
			if e.ID == active {
				foundActive = true
				break
			}
		}
		if !foundActive {
			active = ""
		}
	}
	if err := s.m.setProxyPoolEntries(entries, active, generation); err != nil {
		if errors.Is(err, errProxyPoolChanged) {
			writeErr(w, http.StatusConflict, err.Error())
			return
		}
		writeErr(w, 500, err.Error())
		return
	}
	if policyUpdate {
		timed, interval, onFailure, strategy := s.m.proxyRotateConfig()
		threshold, cooldown, healthInterval := s.m.proxyHealthConfig()
		if req.TimedEnabled != nil {
			timed = *req.TimedEnabled
		}
		if req.Interval != nil {
			interval = time.Duration(*req.Interval) * time.Second
		}
		if req.OnFailure != nil {
			onFailure = *req.OnFailure
		}
		if req.Strategy != nil {
			strategy = *req.Strategy
		}
		if req.Threshold != nil {
			threshold = *req.Threshold
		}
		if req.Cooldown != nil {
			cooldown = time.Duration(*req.Cooldown) * time.Second
		}
		if req.HealthInterval != nil {
			healthInterval = time.Duration(*req.HealthInterval) * time.Second
		}
		if err := s.m.SetProxyPoolSettings(timed, int(interval/time.Second), onFailure, strategy, threshold, int(cooldown/time.Second), int(healthInterval/time.Second)); err != nil {
			writeErr(w, 400, err.Error())
			return
		}
	}
	// Import commits configuration only; the existing supervisor probes it in
	// the background. It still reconciles/pause-checks only after a whole round.
	probeQueued := s.queueProxyPoolHealth(false)
	if !probeQueued {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "配置已保存但后台探测未排队", "saved": true, "probe_queued": false})
		return
	}
	respond(probeQueued)
}

func (m *Manager) proxyPoolActiveID() string {
	if m.proxyPool == nil {
		return ""
	}
	m.proxyPool.mu.Lock()
	defer m.proxyPool.mu.Unlock()
	return m.proxyPool.active
}

func mergeMaskedProxyURL(raw, old string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", nil
	}
	nu, err := traffic.ValidateProxyURL(raw)
	if err != nil {
		return "", fmt.Errorf("%s", maskSecrets(err.Error()))
	}
	if ou, _ := traffic.ValidateProxyURL(old); ou != nil && nu.User != nil && ou.User != nil {
		newUser := nu.User.Username()
		newPW, hasNewPW := nu.User.Password()
		maskedUser := newUser == "***" || newUser == "******"
		maskedPW := hasNewPW && (strings.Contains(newPW, "***") || strings.Contains(newPW, "******"))
		if maskedUser {
			oldPW, hasOldPW := ou.User.Password()
			if hasOldPW {
				nu.User = url.UserPassword(ou.User.Username(), oldPW)
			} else {
				nu.User = url.User(ou.User.Username())
			}
		} else if maskedPW {
			if oldPW, hasOldPW := ou.User.Password(); hasOldPW {
				nu.User = url.UserPassword(newUser, oldPW)
			} else {
				nu.User = url.User(newUser)
			}
		}
	}
	return nu.String(), nil
}

func (s *Server) updateProxyPool(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var req struct {
		URL     *string `json:"url"`
		Name    *string `json:"name"`
		Remark  *string `json:"remark"`
		Enabled *bool   `json:"enabled"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&req); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	if s.m.proxyPool == nil {
		writeErr(w, 404, "代理池为空")
		return
	}
	needPause := false
	switched := false
	routed := false
	recoverPaused := false
	proxyGuards := s.snapshotProxyReconnectGuards(false)
	s.m.proxyPool.mu.Lock()
	rollback := s.m.proxyPoolRollbackLocked()
	committed := false
	unlock := func() {
		if !committed {
			rollback()
		}
		s.m.proxyPool.mu.Unlock()
	}
	for i := range s.m.proxyPool.entries {
		e := &s.m.proxyPool.entries[i]
		if e.ID != id {
			continue
		}
		oldEntry := *e
		oldRaw := e.RawURL
		if req.URL != nil {
			raw, err := mergeMaskedProxyURL(*req.URL, oldRaw)
			if err != nil {
				unlock()
				writeErr(w, 400, err.Error())
				return
			}
			e.RawURL, e.URL = raw, maskSecrets(raw)
			if e.ID == s.m.proxyPool.active && e.Enabled && raw != "" {
				// A validated edit replaces the endpoint and starts a fresh health round,
				// including when the old endpoint was cooling down.
				e.Health, e.ConsecutiveFailures, e.LastError = "healthy", 0, ""
			}
		}
		if req.Name != nil {
			e.Name = strings.TrimSpace(*req.Name)
		}
		if req.Remark != nil {
			e.Remark = strings.TrimSpace(*req.Remark)
		}
		if req.Enabled != nil {
			e.Enabled = *req.Enabled
			if !e.Enabled {
				e.Health = "disabled"
			} else if e.Health == "disabled" {
				e.Health = "healthy"
			}
		}
		if s.m.proxyPool.active == id {
			if req.URL != nil || req.Enabled != nil {
				if e.Enabled && e.RawURL != "" {
					if err := s.m.applyActiveProxyLockedWithHealth(id, "manual", req.URL != nil); err != nil {
						*e = oldEntry
						unlock()
						writeErr(w, 400, maskSecrets(err.Error()))
						return
					}
					routed = true
				} else if next := s.m.selectHealthyLocked(id); next != "" {
					for _, candidate := range s.m.proxyPool.entries {
						if candidate.ID == next && candidate.RawURL == "" {
							next = ""
							break
						}
					}
					if next == "" {
						// Fall through to the enabled-member fallback below.
					} else {
						if err := s.m.applyActiveProxyLocked(next, "manual"); err != nil {
							unlock()
							writeErr(w, 400, maskSecrets(err.Error()))
							return
						}
						switched = true
						routed = true
					}
				}
				if !routed {
					// A disabled or cleared active member must never remain the
					// runtime route. Keep traffic on any surviving enabled member,
					// including a cooling one, until the normal reconnect loop can
					// establish a healthy route.
					next := ""
					for _, candidate := range s.m.proxyPool.entries {
						if candidate.ID != id && candidate.Enabled && candidate.RawURL != "" {
							next = candidate.ID
							break
						}
					}
					if next != "" {
						if err := s.m.applyActiveProxyLockedWithHealth(next, "manual", true); err != nil {
							unlock()
							writeErr(w, 400, maskSecrets(err.Error()))
							return
						}
						needPause = true
					} else {
						if err := s.m.applyActiveProxyLocked("", "manual"); err != nil {
							unlock()
							writeErr(w, 400, maskSecrets(err.Error()))
							return
						}
						needPause = false
						recoverPaused = true
					}
					routed = true
				}
			}
		}
		if !routed {
			if err := s.m.persistProxyPoolLocked(); err != nil {
				unlock()
				writeErr(w, 500, maskSecrets(err.Error()))
				return
			}
		}
		committed = true
		s.m.proxyPool.generation++
		out := e.ProxyPoolEntry
		out.URL = maskSecrets(e.RawURL)
		if !out.Enabled {
			out.Health = "disabled"
		}
		unlock()
		if needPause {
			s.pauseAllProxyTasks(errors.New("代理池暂无健康成员"))
		}
		if switched {
			s.proxyReconnectSuccess()
		}
		if recoverPaused {
			s.proxyReconnectSuccessWithGuards(proxyGuards, nil)
		}
		writeJSON(w, 200, map[string]any{"entry": out})
		return
	}
	unlock()
	writeErr(w, 404, "代理不存在")
}

func (s *Server) deleteProxyPool(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if s.m.proxyPool == nil {
		writeErr(w, 404, "代理不存在")
		return
	}
	needPause := false
	switched := false
	recoverPaused := false
	proxyGuards := s.snapshotProxyReconnectGuards(false)
	s.m.proxyPool.mu.Lock()
	rollback := s.m.proxyPoolRollbackLocked()
	committed := false
	routed := false
	unlock := func() {
		if !committed {
			rollback()
		}
		s.m.proxyPool.mu.Unlock()
	}
	found := false
	out := make([]proxyPoolEntry, 0, len(s.m.proxyPool.entries))
	for _, e := range s.m.proxyPool.entries {
		if e.ID == id {
			found = true
			continue
		}
		out = append(out, e)
	}
	if !found {
		unlock()
		writeErr(w, 404, "代理不存在")
		return
	}
	s.m.proxyPool.entries = out
	if s.m.proxyPool.active == id {
		if next := s.m.selectHealthyLocked(id); next != "" {
			if err := s.m.applyActiveProxyLocked(next, "manual"); err != nil {
				unlock()
				writeErr(w, 500, maskSecrets(err.Error()))
				return
			}
			switched = true
		} else {
			// Keep runtime traffic attached to a surviving pool member even when
			// every remaining member is cooling down; never retain the deleted URL.
			next := ""
			for _, e := range s.m.proxyPool.entries {
				if e.Enabled && e.RawURL != "" {
					next = e.ID
					break
				}
			}
			if next != "" {
				if err := s.m.applyActiveProxyLockedWithHealth(next, "manual", true); err != nil {
					unlock()
					writeErr(w, 500, maskSecrets(err.Error()))
					return
				}
				needPause = true
			} else {
				if err := s.m.applyActiveProxyLocked("", "manual"); err != nil {
					unlock()
					writeErr(w, 500, maskSecrets(err.Error()))
					return
				}
				// Clearing the last usable route leaves no proxy to recover.
				// Only an enabled member that is actually cooling warrants the
				// pool-wide proxy pause barrier.
				for _, candidate := range s.m.proxyPool.entries {
					if candidate.Enabled && candidate.RawURL != "" && candidate.Health == "unhealthy-cooling" {
						needPause = true
						break
					}
				}
				if !needPause {
					recoverPaused = true
				}
			}
		}
		routed = true
	}
	if !routed {
		if err := s.m.persistProxyPoolLocked(); err != nil {
			unlock()
			writeErr(w, 500, maskSecrets(err.Error()))
			return
		}
		s.m.proxyPoolConfigured.Store(len(out) > 0)
	}
	committed = true
	s.m.proxyPool.generation++
	unlock()
	if needPause {
		s.pauseAllProxyTasks(errors.New("代理池暂无健康成员"))
	}
	if switched {
		s.proxyReconnectSuccess()
	}
	if recoverPaused {
		s.proxyReconnectSuccessWithGuards(proxyGuards, nil)
	}
	writeJSON(w, 200, map[string]any{"ok": true})
}

func (s *Server) activateProxyPool(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	before := s.m.proxyPoolActiveID()
	if err := s.m.activateProxyID(id, "manual"); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	if before != id {
		s.proxyReconnectSuccess()
	}
	writeJSON(w, 200, map[string]any{"ok": true, "active_id": id, "active_proxy": maskSecrets(s.m.GlobalProxy())})
}

func (s *Server) testProxyPoolEntry(w http.ResponseWriter, r *http.Request) {
	if !s.proxyPoolTickMu.TryLock() {
		writeErr(w, http.StatusConflict, "批量探测中，请稍后测试单条代理")
		return
	}
	defer s.proxyPoolTickMu.Unlock()
	if r.Context().Err() != nil || s.proxyShuttingDown() {
		writeErr(w, http.StatusServiceUnavailable, "请求已取消或服务正在停止，探测未执行")
		return
	}
	id := r.PathValue("id")
	if s.m == nil || s.m.proxyPool == nil {
		writeErr(w, http.StatusNotFound, "代理不存在")
		return
	}
	s.m.proxyPool.mu.Lock()
	entries := append([]proxyPoolEntry(nil), s.m.proxyPool.entries...)
	generation := s.m.proxyPool.generation
	s.m.proxyPool.mu.Unlock()
	for _, e := range entries {
		if e.ID == id && e.Enabled && strings.TrimSpace(e.RawURL) != "" {
			res := s.runProxyProbe(r.Context(), e.RawURL, "", defaultEgressProbes, proxyPoolProbeTimeout)
			if r.Context().Err() != nil || s.proxyShuttingDown() {
				writeErr(w, http.StatusServiceUnavailable, "请求已取消或服务正在停止，结果未保存")
				return
			}
			s.m.proxyPool.mu.Lock()
			if r.Context().Err() != nil || s.proxyShuttingDown() || s.m.proxyPool.generation != generation {
				s.m.proxyPool.mu.Unlock()
				writeErr(w, http.StatusConflict, "代理配置已变更，旧探测结果未保存")
				return
			}
			threshold := defaultProxyHealthThreshold
			if s.m.pg != nil {
				threshold = proxyIntSetting(s.m.pg, settingProxyHealthThreshold, threshold)
			}
			confirmed, err := s.m.applyProxyProbeBatchLocked([]proxyBatchProbe{{entry: e, result: res, complete: true}}, threshold)
			s.m.proxyPool.mu.Unlock()
			if err != nil {
				writeErr(w, http.StatusInternalServerError, "探测结果保存失败，路线保持不变")
				return
			}
			if res.OK {
				s.reconcileProxyPoolAfterProbe(confirmed, generation)
			}
			writeJSON(w, 200, map[string]any{"ok": res.OK, "result": res})
			return
		}
	}
	writeErr(w, 404, "代理不存在")
}

func (s *Server) testAllProxyPool(w http.ResponseWriter, r *http.Request) {
	if !s.queueProxyPoolHealth(true) {
		writeErr(w, http.StatusServiceUnavailable, "服务正在停止，探测未排队")
		return
	}
	if r.URL.Query().Get("summary") == "1" {
		writeJSON(w, 200, map[string]any{"ok": true, "probe_queued": true})
		return
	}
	entries, active, logs := s.m.ProxyPoolSnapshot()
	writeJSON(w, 200, map[string]any{"ok": true, "entries": entries, "active_id": active, "switch_log": logs, "probe_queued": true})
}
