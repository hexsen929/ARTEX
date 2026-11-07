package server

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
)

// A batch names the snapshot's IDs, never a query that could include newly
// imported members. Cleanup additionally rechecks current durable health while
// holding the same lock as health publication and configuration mutations.
type proxyPoolBulkRequest struct {
	Action           string   `json:"action"`
	IDs              []string `json:"ids"`
	OnlyUnhealthy    bool     `json:"only_unhealthy,omitempty"`
	AllowActive      bool     `json:"allow_active,omitempty"`
	ExpectedActiveID string   `json:"expected_active_id,omitempty"`
}

func proxyCleanupEligible(entry ProxyPoolEntry, threshold int) bool {
	for _, check := range entry.ProtocolChecks {
		if check.HTTP.OK || check.HTTPS.OK {
			return false
		}
	}
	return (entry.Health == "unhealthy-cooling" || entry.Health == "disabled") && !entry.LastCheckedAt.IsZero() && entry.LastError != "" && entry.ConsecutiveFailures >= threshold
}

func (s *Server) bulkProxyPool(w http.ResponseWriter, r *http.Request) {
	var req proxyPoolBulkRequest
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<20))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "批量代理请求格式无效")
		return
	}
	if err := decoder.Decode(new(any)); err != io.EOF || (req.Action != "delete" && req.Action != "disable") || (req.OnlyUnhealthy && req.Action != "delete") || len(req.IDs) == 0 || len(req.IDs) > 100000 {
		writeErr(w, http.StatusBadRequest, "请选择 1 至 100000 条代理并指定删除或禁用操作")
		return
	}
	ids := make(map[string]bool, len(req.IDs))
	for _, id := range req.IDs {
		if id == "" || len(id) > 128 || strings.TrimSpace(id) != id {
			writeErr(w, http.StatusBadRequest, "代理 ID 无效")
			return
		}
		ids[id] = true
	}
	if s.proxyShuttingDown() {
		writeErr(w, http.StatusServiceUnavailable, "服务正在停止")
		return
	}
	threshold := defaultProxyHealthThreshold
	if req.OnlyUnhealthy && s.m.pg != nil {
		value, present, err := s.m.pg.GetSetting(settingProxyHealthThreshold)
		if err != nil {
			writeErr(w, http.StatusInternalServerError, "读取已保存的失效阈值失败，请刷新后重试")
			return
		}
		if present {
			threshold, err = strconv.Atoi(strings.TrimSpace(value))
			if err != nil || threshold < 1 {
				writeErr(w, http.StatusConflict, "已保存的失效阈值无效，请先修复健康检查策略")
				return
			}
		}
	}
	if s.m.proxyPool == nil {
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "changed": 0, "skipped": len(ids), "active_id": "", "active_changed": false})
		return
	}
	guards := s.snapshotProxyReconnectGuards(false)
	s.m.proxyPool.mu.Lock()
	pool := s.m.proxyPool
	previousActive := pool.active
	if !req.OnlyUnhealthy && ids[pool.active] && !(req.AllowActive && req.ExpectedActiveID == pool.active) {
		pool.mu.Unlock()
		writeErr(w, http.StatusConflict, "所选代理包含当前使用项，请刷新后确认出口切换")
		return
	}
	changed := 0
	entries := make([]proxyPoolEntry, 0, len(pool.entries))
	activeRemoved := false
	for _, entry := range pool.entries {
		selected := ids[entry.ID]
		if req.OnlyUnhealthy {
			selected = selected && entry.ID != pool.active && proxyCleanupEligible(entry.ProxyPoolEntry, threshold)
		}
		if selected && (req.Action == "delete" || entry.Enabled) {
			changed++
			activeRemoved = activeRemoved || entry.ID == pool.active
			if req.Action == "delete" {
				continue
			}
			entry.Enabled, entry.Health = false, "disabled"
		}
		entries = append(entries, entry)
	}
	if changed == 0 {
		pool.mu.Unlock()
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "changed": 0, "skipped": len(ids), "active_id": previousActive, "active_changed": false})
		return
	}
	rollback := s.m.proxyPoolRollbackLocked()
	pool.entries = entries
	needPause, switched, recoverPaused := false, false, false
	var err error
	if activeRemoved {
		if next := s.m.selectHealthyLocked(previousActive); next != "" {
			err = s.m.applyActiveProxyLocked(next, "bulk-"+req.Action)
			switched = err == nil
		} else {
			next := ""
			for _, entry := range entries {
				if proxyEntryUsable(entry) {
					next = entry.ID
					break
				}
			}
			err = s.m.applyActiveProxyLockedWithHealth(next, "bulk-"+req.Action, true)
			needPause, recoverPaused = next != "", next == ""
		}
	} else {
		err = s.m.persistProxyPoolLocked()
	}
	if err != nil {
		rollback()
		pool.mu.Unlock()
		writeErr(w, http.StatusInternalServerError, "批量操作保存失败，代理池和出口保持原状")
		return
	}
	s.m.proxyPoolConfigured.Store(len(entries) > 0)
	pool.generation++ // Discard every in-flight round for the previous membership.
	active := pool.active
	pool.mu.Unlock()
	// Keep the same task pause/recovery rules as an explicit single-member edit.
	if needPause {
		s.pauseAllProxyTasks(errors.New("代理池暂无健康成员"))
	}
	if switched {
		s.proxyReconnectSuccess()
	}
	if recoverPaused {
		s.proxyReconnectSuccessWithGuards(guards, nil)
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "changed": changed, "skipped": len(ids) - changed, "active_id": active, "active_changed": active != previousActive})
}
