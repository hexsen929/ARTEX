package server

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"
)

const settingProxyHealthURL = "proxy_health_check_url"

// Static, body-free connectivity checks. IP lookup is an independent optional
// feature and is never a prerequisite for pool health or task recovery.
var defaultHealthProbes = []string{
	"https://www.gstatic.com/generate_204",
	"https://cp.cloudflare.com/generate_204",
}

func (m *Manager) ProxyHealthURLChecked() (string, error) {
	if m == nil || m.pg == nil {
		return "", nil
	}
	raw, _, err := m.pg.GetSetting(settingProxyHealthURL)
	if err != nil {
		return "", errors.New("读取代理连通性检测网址失败")
	}
	return strings.TrimSpace(raw), nil
}

func validateHealthURL(raw string) (string, error) {
	checked, err := validateProbeURL(raw)
	if err != nil {
		return "", err
	}
	u, err := url.Parse(checked)
	if err != nil || !strings.EqualFold(u.Scheme, "https") {
		return "", errors.New("连通性检测网址必须使用 HTTPS")
	}
	if u.User != nil || u.Fragment != "" {
		return "", errors.New("检测网址不能包含账号密码或片段")
	}
	return checked, nil
}

func (m *Manager) healthProbesChecked() ([]string, error) {
	raw, err := m.ProxyHealthURLChecked()
	if err != nil {
		return nil, err
	}
	if raw == "" {
		return append([]string(nil), defaultHealthProbes...), nil
	}
	checked, err := validateHealthURL(raw)
	if err != nil {
		return nil, errors.New("已保存的连通性检测网址无效，请在系统设置修正")
	}
	return []string{checked}, nil
}

func (m *Manager) healthProbesMatch(probes []string) bool {
	current, err := m.healthProbesChecked()
	return err == nil && slices.Equal(probes, current)
}

func (s *Server) runProxyHealthProbe(ctx context.Context, raw, ca string, probes []string, timeout time.Duration) egressProbeResult {
	if s != nil && s.proxyProbe != nil {
		return s.proxyProbe(ctx, raw, ca, probes, timeout)
	}
	return probeHTTP(ctx, raw, ca, probes, timeout, true)
}

type healthHTTPStatusError int

func (e healthHTTPStatusError) Error() string {
	return fmt.Sprintf("检测站点返回 HTTP %d", int(e))
}

func probeConnectivityOne(ctx context.Context, client *http.Client, target string) (int, error) {
	for _, method := range []string{http.MethodHead, http.MethodGet} {
		req, err := http.NewRequestWithContext(ctx, method, target, nil)
		if err != nil {
			return 0, err
		}
		req.Header.Set("User-Agent", "artex-connectivity-check/1.0")
		response, err := client.Do(req)
		if err != nil {
			return 0, err
		}
		status := response.StatusCode
		if method == http.MethodGet {
			_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 4096))
		}
		_ = response.Body.Close()
		if method == http.MethodHead && (status == http.StatusMethodNotAllowed || status == http.StatusNotImplemented) {
			continue
		}
		valid := status >= 200 && status < 300
		if slices.Contains(defaultHealthProbes, target) {
			valid = status == http.StatusNoContent
		}
		if !valid {
			return status, healthHTTPStatusError(status)
		}
		return status, nil
	}
	return 0, errors.New("连通性检查未完成")
}

func (s *Server) testProxyHealth(w http.ResponseWriter, r *http.Request) {
	s.testProxyPaths(w, r, true)
}
