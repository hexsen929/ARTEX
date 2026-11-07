package server

import (
	"context"
	"io"
	"net"
	"net/url"
	"strings"
	"time"
)

// Bounded, credential-free summaries separate observations from route eligibility.
// An HTTP echo can be supplied by a transparent network intermediary; it alone
// neither identifies the endpoint's protocol nor verifies an HTTPS route.
type proxyCapabilityCheck struct {
	Checked   bool   `json:"checked"`
	OK        bool   `json:"ok"`
	IP        string `json:"ip,omitempty"`
	Error     string `json:"error,omitempty"`
	ErrorCode string `json:"error_code,omitempty"`
	LatencyMS int64  `json:"latency_ms"`
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
	return proxyCapabilityCheck{Checked: true, OK: r.OK, IP: r.ExitIP, Error: r.Error, ErrorCode: r.ErrorCode, LatencyMS: r.LatencyMS}
}

// SOCKS4 is detection-only. The capture and task forwarding transports do not
// implement it, so never put a socks4 URL into the routable pool. A SOCKS4a
// request also produces a valid rejection from SOCKS4-only servers; either
// response identifies the protocol but does not prove target connectivity.
func probeSOCKS4Response(parent context.Context, raw string) (bool, string) {
	ctx, cancel := context.WithTimeout(parent, 5*time.Second)
	defer cancel()
	u, err := url.Parse(raw)
	if err != nil {
		return false, "代理地址格式无效"
	}
	conn, err := (&net.Dialer{}).DialContext(ctx, "tcp", u.Host)
	if err != nil {
		_, label := classifyEgressProbeError(err)
		return false, label
	}
	defer conn.Close()
	stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stop()
	if deadline, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(deadline)
	}
	// No password or user ID is sent during protocol detection.
	request := append([]byte{4, 1, 1, 187, 0, 0, 0, 1, 0}, []byte("api.ipify.org\x00")...)
	if _, err = conn.Write(request); err != nil {
		_, label := classifyEgressProbeError(err)
		return false, label
	}
	reply := make([]byte, 8)
	if _, err = io.ReadFull(conn, reply); err != nil {
		_, label := classifyEgressProbeError(err)
		return false, label
	}
	if reply[0] != 0 || reply[1] < 90 || reply[1] > 93 {
		return false, "未收到有效 SOCKS4 协议响应"
	}
	if reply[1] == 90 {
		return true, "SOCKS4 请求通过；当前转发链不支持 SOCKS4，未验证 HTTP／HTTPS 回显"
	}
	return true, "收到 SOCKS4 拒绝响应；协议已识别，出口未验证，当前转发链不支持 SOCKS4"
}

// At most eight bounded checks per unknown entry: HTTPS + HTTP for each of
// three routable protocols, SOCKS4 recognition, and an HTTP negative control. All share the
// same ten-second budget. Join all workers before returning/cancelling the round.
func (s *Server) inspectProxyProtocols(parent context.Context, entry proxyPoolEntry, stopOnSuccess bool) (egressProbeResult, egressProbeResult, string) {
	ctx, cancel := context.WithTimeout(parent, proxyPoolProbeTimeout)
	defer cancel()
	started := time.Now()
	u, err := url.Parse(entry.RawURL)
	if err != nil || u.Host == "" {
		invalid := egressProbeResult{Error: "代理地址格式无效", ErrorCode: "invalid_proxy_url", Probes: []egressAttempt{}}
		return invalid, invalid, ""
	}
	type answer struct {
		index      int
		plain      bool
		result     egressProbeResult
		recognized bool
		evidence   string
	}
	answers := make(chan answer, 8)
	schemes := []string{"http", "https", "socks5", "socks4"}
	raws := make([]string, 3)
	for i, scheme := range schemes[:3] {
		candidate := *u
		candidate.Scheme = scheme
		raws[i] = candidate.String()
		for _, plain := range []bool{false, true} {
			go func(index int, raw string, plain bool) {
				probes := defaultEgressProbes
				if plain {
					probes = []string{"http://api.ipify.org?format=json"}
				}
				result := s.runProxyProbe(ctx, raw, "", probes, 5*time.Second)
				answers <- answer{index: index, plain: plain, result: result}
			}(i, raws[i], plain)
		}
	}
	go func() {
		probe := probeSOCKS4Response
		if s != nil && s.proxySOCKS4Probe != nil {
			probe = s.proxySOCKS4Probe
		}
		recognized, evidence := probe(ctx, entry.RawURL)
		answers <- answer{index: 3, recognized: recognized, evidence: evidence}
	}()
	// TEST-NET cannot be a public proxy. A valid IP echo through it exposes
	// transparent interception; equal HTTP echoes must not become auto routes.
	go func() {
		result := s.runProxyProbe(ctx, "http://203.0.113.1:80", "", []string{"http://api.ipify.org?format=json"}, 2*time.Second)
		answers <- answer{index: 4, result: result}
	}()
	checks := make([]proxyProtocolCheck, 4)
	secure, plain := make([]egressProbeResult, 3), make([]egressProbeResult, 3)
	for i, scheme := range schemes {
		checks[i] = proxyProtocolCheck{Protocol: scheme, Supported: i < 3}
	}
	winner := -1
	var control egressProbeResult
	for range 8 {
		a := <-answers
		if a.index == 4 {
			control = a.result
			continue
		}
		check := &checks[a.index]
		if a.index == 3 {
			check.Recognized, check.Evidence = a.recognized, a.evidence
			continue
		}
		check.Recognized = check.Recognized || a.result.ProtocolRecognized || (!a.plain && a.result.OK)
		if a.plain {
			plain[a.index] = a.result
			check.HTTP = capabilityCheck(a.result)
		} else {
			secure[a.index] = a.result
			check.HTTPS = capabilityCheck(a.result)
		}
		if check.HTTPS.OK && check.HTTP.OK && winner == -1 {
			winner = a.index
			if stopOnSuccess {
				cancel()
			}
		}
	}
	for i := range 3 {
		checks[i].AutoSelectionBlocked = !checks[i].HTTPS.OK && !checks[i].Recognized && checks[i].HTTP.OK && control.OK && checks[i].HTTP.IP == control.ExitIP
		switch {
		case checks[i].HTTPS.OK && checks[i].HTTP.OK:
			checks[i].Evidence = "HTTP／HTTPS 访问均通过，证书验证通过，可完整使用"
		case checks[i].HTTPS.OK:
			checks[i].Evidence = "HTTPS 访问通过，HTTP 访问未通过，未达到完整可用条件"
		case checks[i].Recognized:
			checks[i].Evidence = "已收到代理协议响应，HTTPS 验证未通过"
		case checks[i].AutoSelectionBlocked:
			checks[i].Evidence = "无效对照地址也返回相同 HTTP 出口，疑似网络转发干扰；不自动选用"
		case checks[i].HTTP.OK:
			checks[i].Evidence = "仅 HTTP 回显通过；可能受网络转发影响，未确认代理身份"
		default:
			checks[i].Evidence = "未确认代理协议，具体原因见 HTTP／HTTPS 结果"
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
			if checks[i].HTTP.OK {
				score += 10
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
		if result.OK {
			result = plain[selected]
			result.Error = "HTTPS 访问通过，但 HTTP 访问验证失败：" + result.Error
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
		case checks[3].Recognized:
			result.Via, result.ViaLabel = "socks4", "SOCKS4 代理"
			result.ProtocolRecognized = true
			result.ErrorCode, result.Error = "unsupported_proxy_protocol", checks[3].Evidence
		case checks[selected].AutoSelectionBlocked:
			result.ErrorCode, result.Error = "network_interception", checks[selected].Evidence
		case checks[selected].HTTP.OK:
			result.ErrorCode, result.Error = "http_echo_only", "仅 HTTP 回显通过；尚未确认代理身份，HTTPS 验证未通过"
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
