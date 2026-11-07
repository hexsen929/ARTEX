package server

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"time"

	"github.com/Autumn-27/artex/traffic"
)

// A user can explicitly select an observed HTTP-capable protocol. This mode
// checks HTTP health and stays out of automatic HTTPS route selection. It does
// not downgrade HTTPS requests, trust peer certificates, or fall back to direct.
type proxyProtocolSelection struct {
	Protocol string `json:"protocol"`
	HTTPOnly bool   `json:"http_only"`
}

func (m *Manager) activateProxyProtocol(id string, selection proxyProtocolSelection) (err error) {
	if m.proxyPool == nil {
		return errors.New("代理池未初始化")
	}
	if selection.Protocol != "http" && selection.Protocol != "https" && selection.Protocol != "socks5" {
		return errors.New("请选择支持转发的 HTTP、HTTPS 或 SOCKS5 协议")
	}
	m.proxyPool.mu.Lock()
	defer m.proxyPool.mu.Unlock()
	rollback := m.proxyPoolRollbackLocked()
	defer func() {
		if err != nil {
			rollback()
		}
	}()
	for i := range m.proxyPool.entries {
		e := &m.proxyPool.entries[i]
		if e.ID != id || !e.Enabled {
			continue
		}
		var check *proxyProtocolCheck
		for j := range e.ProtocolChecks {
			if e.ProtocolChecks[j].Protocol == selection.Protocol && e.ProtocolChecks[j].Supported {
				check = &e.ProtocolChecks[j]
				break
			}
		}
		if check == nil || (!selection.HTTPOnly && !check.HTTPS.OK) || (selection.HTTPOnly && !check.HTTP.OK) {
			return errors.New("所选协议没有对应的成功回显，请重新测试后选择")
		}
		if e.LastCheckedAt.IsZero() || time.Since(e.LastCheckedAt) > 5*time.Minute {
			return errors.New("协议测试结果已过期，请重新测试后选择")
		}
		u, parseErr := url.Parse(e.RawURL)
		if parseErr != nil {
			return errors.New("代理地址无效")
		}
		u.Scheme = selection.Protocol
		if _, validateErr := traffic.ValidateProxyURL(u.String()); validateErr != nil {
			return errors.New("代理地址无效")
		}
		e.RawURL, e.URL, e.ProtocolAuto, e.DetectedProtocol = u.String(), maskSecrets(u.String()), false, ""
		e.HTTPOnly = selection.HTTPOnly
		e.ConsecutiveFailures, e.LastError, e.LastErrorCode = 0, "", ""
		e.Health, e.EgressIP, e.LatencyMS = "healthy", check.HTTPS.IP, check.HTTPS.LatencyMS
		if e.HTTPOnly {
			e.Health, e.EgressIP, e.LatencyMS = "http-only", check.HTTP.IP, check.HTTP.LatencyMS
		}
		m.proxyPool.disabled = false
		if err = m.applyActiveProxyLocked(id, "manual-protocol"); err != nil {
			return err
		}
		m.proxyPool.generation++
		return nil
	}
	return errors.New("代理不存在或已禁用")
}

func decodeProxyProtocolSelection(w http.ResponseWriter, r *http.Request) (proxyProtocolSelection, error) {
	var selection proxyProtocolSelection
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 2048))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&selection); err != nil && err != io.EOF {
		return selection, errors.New("协议选择请求无效")
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return selection, errors.New("协议选择请求无效")
	}
	if selection.Protocol == "" && selection.HTTPOnly {
		return selection, errors.New("HTTP 受限使用必须指定代理协议")
	}
	return selection, nil
}

func (s *Server) probeHTTPOnlyEntry(ctx context.Context, entry proxyPoolEntry) egressProbeResult {
	secure := make(chan egressProbeResult, 1)
	go func() { secure <- s.runProxyProbe(ctx, entry.RawURL, "", defaultEgressProbes, 5*time.Second) }()
	controlResult := make(chan egressProbeResult, 1)
	if entry.ProtocolAuto {
		go func() {
			controlResult <- s.runProxyProbe(ctx, "http://203.0.113.1:80", "", []string{"http://api.ipify.org?format=json"}, 2*time.Second)
		}()
	} else {
		controlResult <- egressProbeResult{}
	}
	plain := s.runProxyProbe(ctx, entry.RawURL, "", []string{"http://api.ipify.org?format=json"}, 5*time.Second)
	https := <-secure
	control := <-controlResult
	u, _ := url.Parse(entry.RawURL)
	check := proxyProtocolCheck{Protocol: u.Scheme, Supported: true, Recognized: https.ProtocolRecognized || https.OK || plain.ProtocolRecognized,
		HTTP: capabilityCheck(plain), HTTPS: capabilityCheck(https), Evidence: "按 HTTP 受限能力检测；HTTPS 仍执行证书校验"}
	plain.HTTPOnly = true
	plain.ProtocolChecks = []proxyProtocolCheck{check}
	plain.Via, plain.ViaLabel = u.Scheme, schemeLabel(u.Scheme)
	if entry.ProtocolAuto && https.OK {
		https.ProtocolChecks = []proxyProtocolCheck{check}
		return https
	}
	if entry.ProtocolAuto && plain.OK && !check.Recognized {
		if control.OK && control.ExitIP == plain.ExitIP {
			plain.ProtocolChecks[0].AutoSelectionBlocked = true
			plain.OK, plain.ErrorCode, plain.Error = false, "network_interception", "无效对照地址返回相同 HTTP 出口，无法确认代理仍可用"
		}
	}
	return plain
}

func (m *Manager) activeProxyHTTPOnly(raw string) bool {
	if m == nil || m.proxyPool == nil {
		return false
	}
	m.proxyPool.mu.Lock()
	defer m.proxyPool.mu.Unlock()
	for _, entry := range m.proxyPool.entries {
		if entry.ID == m.proxyPool.active && entry.RawURL == raw {
			return entry.HTTPOnly
		}
	}
	return false
}
