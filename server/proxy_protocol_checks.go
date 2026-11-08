package server

import (
	"context"
	"net/url"
	"strings"
	"time"
)

// Bounded, credential-free summaries separate observations from route eligibility.
// An HTTP echo can be supplied by a transparent network intermediary; it alone
// neither identifies the endpoint's protocol nor verifies an HTTPS route.
type proxyCapabilityCheck struct {
	StatusCode int    `json:"status_code,omitempty"`
	Checked    bool   `json:"checked"`
	OK         bool   `json:"ok"`
	IP         string `json:"ip,omitempty"`
	Error      string `json:"error,omitempty"`
	ErrorCode  string `json:"error_code,omitempty"`
	LatencyMS  int64  `json:"latency_ms"`
}

type proxyProtocolCheck struct {
	Protocol             string               `json:"protocol"`
	Recognized           bool                 `json:"recognized"`
	Supported            bool                 `json:"supported"`
	Evidence             string               `json:"evidence,omitempty"`
	AutoSelectionBlocked bool                 `json:"auto_selection_blocked,omitempty"`
	HTTPS                proxyCapabilityCheck `json:"https"`
	HTTP                 proxyCapabilityCheck `json:"http"`
}

func capabilityCheck(r egressProbeResult) proxyCapabilityCheck {
	return proxyCapabilityCheck{Checked: r.ErrorCode != "not_requested", OK: r.OK, IP: r.ExitIP, StatusCode: r.StatusCode, Error: r.Error, ErrorCode: r.ErrorCode, LatencyMS: r.LatencyMS}
}

func skippedProtocolCheck() egressProbeResult {
	return egressProbeResult{Kind: "connectivity", ErrorCode: "not_requested", Error: "未执行此类访问检查", Probes: []egressAttempt{}}
}

// Supported proxy protocols share one bounded HTTPS connectivity check.
// All candidates are joined; an IP echo is never required or requested.
func (s *Server) inspectProxyProtocols(parent context.Context, entry proxyPoolEntry, stopOnSuccess bool, selectedProbes ...[]string) (egressProbeResult, egressProbeResult, string) {
	ctx, cancel := context.WithTimeout(parent, proxyPoolProbeTimeout)
	defer cancel()
	started := time.Now()
	probes := defaultHealthProbes
	if len(selectedProbes) > 0 {
		probes = selectedProbes[0]
	}
	secureTargets := probes
	u, err := url.Parse(entry.RawURL)
	if err != nil || u.Host == "" {
		invalid := egressProbeResult{Error: "代理地址格式无效", ErrorCode: "invalid_proxy_url", Probes: []egressAttempt{}}
		return invalid, invalid, ""
	}
	type answer struct {
		index  int
		result egressProbeResult
	}
	answers := make(chan answer, 3)
	schemes := []string{"http", "https", "socks5", "socks4"}
	raws := make([]string, 3)
	for i, scheme := range schemes[:3] {
		candidate := *u
		candidate.Scheme = scheme
		raws[i] = candidate.String()
		go func(index int, raw string) {
			result := s.runProxyHealthProbe(ctx, raw, "", secureTargets, 5*time.Second)
			answers <- answer{index: index, result: result}
		}(i, raws[i])
	}
	checks := make([]proxyProtocolCheck, 4)
	secure, plain := make([]egressProbeResult, 3), make([]egressProbeResult, 3)
	for i, scheme := range schemes {
		checks[i] = proxyProtocolCheck{Protocol: scheme, Supported: i < 3}
		if i < 3 {
			plain[i] = skippedProtocolCheck()
			checks[i].HTTP = capabilityCheck(plain[i])
		}
	}
	checks[3].Evidence = "当前转发链不支持 SOCKS4，未执行外部探测"
	winner := -1
	for range 3 {
		a := <-answers
		check := &checks[a.index]
		check.Recognized = a.result.ProtocolRecognized || a.result.OK
		secure[a.index] = a.result
		check.HTTPS = capabilityCheck(a.result)
		if check.HTTPS.OK && winner == -1 {
			winner = a.index
			if stopOnSuccess {
				cancel()
			}
		}
	}
	for i := range 3 {
		switch {
		case checks[i].HTTPS.OK:
			checks[i].Evidence = "HTTPS 连通性通过，证书验证通过"
		case checks[i].Recognized:
			checks[i].Evidence = "代理协议有响应，HTTPS 连通性未通过"
		default:
			checks[i].Evidence = "尚未验证可用代理协议"
		}
	}
	selected := winner
	if selected == -1 {
		selected, best := 0, -1
		for i := range 3 {
			score := 0
			if checks[i].HTTPS.OK {
				score += 100
			}
			if checks[i].Recognized {
				score += 20
			}
			if checks[i].Recognized && strings.HasPrefix(secure[i].ErrorCode, "certificate_") {
				score += 10
			}
			if score > best {
				selected, best = i, score
			}
		}
		result := secure[selected]
		if !stopOnSuccess {
			result.ProtocolChecks = checks
			return result, plain[selected], ""
		}
		result.OK = false
		result.ProtocolRecognized = checks[selected].Recognized
		result.Via, result.ViaLabel = schemes[selected], schemeLabel(schemes[selected])
		result.ProtocolChecks, result.LatencyMS = checks, time.Since(started).Milliseconds()
		if result.Probes == nil {
			result.Probes = []egressAttempt{}
		}
		switch {
		case checks[selected].Recognized:
			result.Error = strings.ToUpper(schemes[selected]) + " 协议已识别；未完整可用：" + result.Error
		default:
			result.Error = "尚未识别代理协议；" + result.Error
		}
		return result, plain[selected], ""
	}
	result := secure[selected]
	result.Via, result.ViaLabel = schemes[selected], schemeLabel(schemes[selected])
	result.ProtocolRecognized = true
	result.ProtocolChecks, result.LatencyMS = checks, time.Since(started).Milliseconds()
	return result, plain[selected], raws[selected]
}
