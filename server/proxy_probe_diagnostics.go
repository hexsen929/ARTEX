package server

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"syscall"
	"time"
)

type proxyConnectStatusError struct{ Status int }

func (e proxyConnectStatusError) Error() string {
	return fmt.Sprintf("proxy CONNECT status %d", e.Status)
}

// Public errors are closed host-selected categories, not raw proxy/server
// messages. Keep certificate verification enabled and do not trust peer roots.
func classifyEgressProbeError(err error) (string, string) {
	var invalid x509.CertificateInvalidError
	var unknown x509.UnknownAuthorityError
	var hostname x509.HostnameError
	var certificate *tls.CertificateVerificationError
	var network net.Error
	var connectStatus proxyConnectStatusError
	switch {
	case errors.As(err, &connectStatus):
		if connectStatus.Status == http.StatusProxyAuthRequired {
			return "proxy_authentication", "代理认证失败或需要账号密码"
		}
		return "proxy_connect_rejected", fmt.Sprintf("代理拒绝 HTTPS CONNECT（HTTP %d）", connectStatus.Status)
	case errors.Is(err, context.Canceled):
		return "cancelled", "探测已取消"
	case errors.As(err, &invalid):
		if invalid.Reason == x509.Expired {
			return "certificate_time_invalid", "TLS 证书已过期或尚未生效"
		}
		return "certificate_invalid", "TLS 证书验证失败"
	case errors.As(err, &unknown):
		return "certificate_untrusted", "TLS 证书链不受信任"
	case errors.As(err, &hostname):
		return "certificate_hostname", "TLS 证书域名不匹配"
	case errors.As(err, &certificate):
		return "certificate_invalid", "TLS 证书验证失败"
	case errors.Is(err, context.DeadlineExceeded):
		return "timeout", "代理连接或出口响应超时"
	case errors.As(err, &network) && network.Timeout():
		return "timeout", "代理连接或出口响应超时"
	case errors.Is(err, syscall.ECONNREFUSED):
		return "connection_refused", "代理连接被拒绝"
	}
	var dns *net.DNSError
	if errors.As(err, &dns) {
		return "dns_failed", "DNS 解析失败"
	}
	// net/http's CONNECT authentication response and Go's SOCKS5 dialer use
	// text errors; recognize them, but never echo the returned text.
	var requestError *url.Error
	if errors.As(err, &requestError) {
		err = requestError.Err
	}
	lower := strings.ToLower(err.Error())
	switch {
	case lower == "proxy authentication required" || strings.Contains(lower, "proxyconnect tcp: proxy authentication required") || strings.Contains(lower, "username/password authentication failed"):
		return "proxy_authentication", "代理认证失败或需要账号密码"
	case strings.Contains(lower, "拒绝访问本机") || strings.Contains(lower, "拒绝跨主机重定向"):
		return "probe_boundary", "探测边界拒绝访问本机或跨主机重定向"
	case strings.Contains(lower, "无法从响应中解析出 ip") || strings.Contains(lower, "ip 字段不是合法 ip"):
		return "invalid_ip_response", "出口回显不是有效 IP，可能是错误页或反爬页"
	case strings.HasPrefix(lower, "http "):
		return "echo_http_error", "出口回显服务返回非成功状态"
	case strings.Contains(lower, "tls"):
		return "tls_handshake", "TLS 握手失败"
	default:
		return "proxy_transport", "代理隧道或出口请求失败"
	}
}

func summarizeEgressProbeFailures(attempts []egressAttempt) (string, string) {
	if len(attempts) == 0 {
		return "no_probe", "没有可用的探测地址"
	}
	seen := map[string]bool{}
	labels := []string{}
	first, common := attempts[0].ErrorCode, true
	for _, attempt := range attempts {
		if attempt.ErrorCode != first {
			common = false
		}
		if !seen[attempt.Error] && len(labels) < 2 {
			seen[attempt.Error] = true
			labels = append(labels, attempt.Error)
		}
	}
	code := "multiple_probe_failures"
	if common && first != "" {
		code = first
	}
	return code, "全部探测地址均失败：" + strings.Join(labels, "；")
}

// Diagnostics never publish health, persist settings, switch routes, or release
// paused tasks. Plain HTTP is a separate explicit check, not a TLS fallback.
func (s *Server) diagnoseProxyPoolEntry(w http.ResponseWriter, r *http.Request) {
	release, ok := s.beginInteractiveProxyProbe(w)
	if !ok {
		return
	}
	defer release()
	if r.Context().Err() != nil || s.proxyShuttingDown() {
		writeErr(w, http.StatusServiceUnavailable, "请求已取消或服务正在停止，诊断未执行")
		return
	}
	if s.m == nil || s.m.proxyPool == nil {
		writeErr(w, http.StatusNotFound, "代理不存在")
		return
	}
	s.m.proxyPool.mu.Lock()
	generation := s.m.proxyPool.generation
	var selected *proxyPoolEntry
	for _, entry := range s.m.proxyPool.entries {
		if entry.ID == r.PathValue("id") && entry.Enabled && strings.TrimSpace(entry.RawURL) != "" {
			copy := entry
			selected = &copy
			break
		}
	}
	s.m.proxyPool.mu.Unlock()
	if selected == nil {
		writeErr(w, http.StatusNotFound, "代理不存在或已禁用")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	stopCancellation := context.AfterFunc(serverContext(s), cancel)
	defer stopCancellation()
	var https, httpResult egressProbeResult
	if selected.ProtocolAuto && selected.DetectedProtocol == "" {
		// RawURL is only a storage placeholder until an HTTPS-verified candidate
		// is selected. Diagnose all candidates, not that placeholder's scheme.
		https, httpResult, _ = s.inspectProxyProtocols(ctx, *selected, false)
	} else {
		https = s.runProxyProbe(ctx, selected.RawURL, "", defaultEgressProbes, proxyPoolProbeTimeout)
	}
	if ctx.Err() != nil || s.proxyShuttingDown() || !s.proxyPoolGenerationMatches(generation) {
		writeErr(w, http.StatusConflict, "请求已取消或代理配置已变更，诊断结果已作废")
		return
	}
	if !selected.ProtocolAuto || selected.DetectedProtocol != "" {
		httpResult = s.runProxyProbe(ctx, selected.RawURL, "", []string{"http://api.ipify.org?format=json"}, proxyPoolProbeTimeout)
	}
	if ctx.Err() != nil || s.proxyShuttingDown() || !s.proxyPoolGenerationMatches(generation) {
		writeErr(w, http.StatusConflict, "请求已取消或代理配置已变更，诊断结果已作废")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"id": selected.ID, "https": https, "http": httpResult,
		"checked_at": time.Now(), "health_updated": false,
		"scope": "仅诊断这条代理；HTTP 回显可能受网络转发影响，不证明代理身份或 HTTPS 可信，不改健康状态、出口或任务暂停状态",
	})
}
