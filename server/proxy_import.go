package server

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/Autumn-27/artex/traffic"
)

// Labels describe the input format; connectivity is established only by probes.
// shsocks5 is accepted as the website label supplied by the user, not as a new
// wire protocol. socks5h labels normalize to the supported SOCKS5 URL shape.
func proxyImportScheme(label string) string {
	label = strings.ToLower(strings.Trim(strings.TrimSpace(label), ":/："))
	label = strings.TrimSuffix(strings.TrimSuffix(label, "列表"), "代理")
	switch label {
	case "http", "https":
		return label
	case "socks", "socks5", "socks5h", "shsocks5":
		return "socks5"
	}
	return ""
}

func proxyImportComment(line string) (string, string) {
	var previous rune
	for offset, character := range line {
		if character == '#' && (offset == 0 || unicode.IsSpace(previous)) {
			return strings.TrimSpace(line[:offset]), strings.TrimSpace(line[offset+1:])
		}
		previous = character
	}
	return strings.TrimSpace(line), ""
}

func validProxyImportAuthority(authority string) bool {
	// Authenticated inputs must be complete URLs. A detached userinfo fragment
	// from malformed whitespace is not a new bare endpoint.
	if strings.Contains(authority, "@") {
		return false
	}
	host, port, err := net.SplitHostPort(authority)
	if err != nil || host == "" || strings.ContainsAny(host, "/?#") {
		return false
	}
	number, err := strconv.Atoi(port)
	return err == nil && number > 0 && number <= 65535
}

func normalizeProxyImportToken(token, inherited string) (*url.URL, bool, error) {
	for _, intro := range []string{"还有", "以及"} {
		if rest := strings.TrimPrefix(token, intro); rest != token {
			lower := strings.ToLower(rest)
			if strings.HasPrefix(lower, "socks5") || strings.HasPrefix(lower, "shsocks5") || strings.HasPrefix(lower, "http") {
				token = rest
			}
		}
	}
	explicit := false
	if separator := strings.Index(token, "://"); separator >= 0 {
		if scheme := proxyImportScheme(token[:separator]); scheme != "" {
			token, explicit = scheme+token[separator:], true
		}
	} else {
		// Common website copy: socks5199.116.114.11:4145. Restrict this
		// repair to a following numeric/bracketed endpoint so a hostname like
		// socks5.example:1080 keeps its original meaning.
		lower := strings.ToLower(token)
		for _, prefix := range []string{"shsocks5", "socks5h", "socks5", "https", "http"} {
			if !strings.HasPrefix(lower, prefix) {
				continue
			}
			authority := strings.TrimLeft(token[len(prefix):], ":/")
			if len(authority) > 0 && (authority[0] >= '0' && authority[0] <= '9' || authority[0] == '[') && validProxyImportAuthority(authority) {
				token, explicit = proxyImportScheme(prefix)+"://"+authority, true
				break
			}
		}
		if !explicit {
			if !validProxyImportAuthority(token) {
				return nil, false, errors.New("invalid proxy endpoint")
			}
			if inherited == "auto" {
				inherited = "http"
			}
			token = inherited + "://" + token
		}
	}
	u, err := traffic.ValidateProxyURL(token)
	return u, explicit, err
}

func proxyImportDisplayURL(u *url.URL) string {
	masked := url.URL{Scheme: u.Scheme, Host: u.Host}
	if u.User != nil {
		masked.User = url.User("***")
	}
	return masked.String()
}

// ParseProxyBatch supports URLs, bare endpoints, whitespace-separated URL
// blocks, protocol labels and whitespace-prefixed # remarks. Bare endpoints
// are marked for protocol detection, then inherit an explicit scheme/label.
// Existing URL|name|remark fields are retained. No network probing occurs here.
func ParseProxyBatch(raw string) ([]ProxyPoolEntry, []ProxyImportError) {
	return parseProxyBatchWithProtocol(raw, "auto")
}

func parseProxyBatchWithProtocol(raw, defaultProtocol string) ([]ProxyPoolEntry, []ProxyImportError) {
	var out []ProxyPoolEntry
	var errs []ProxyImportError
	seen := map[string]bool{}
	inherited := defaultProtocol
	batch := time.Now().UnixNano()
	for n, original := range strings.Split(raw, "\n") {
		line := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(original), "\ufeff"))
		if line == "" || strings.HasPrefix(line, "#") || line == "还有" || line == "以及" {
			continue
		}
		parts := strings.SplitN(line, "|", 3)
		addresses, comment := proxyImportComment(parts[0])
		name, remark := "", ""
		if len(parts) > 1 {
			name = strings.TrimSpace(parts[1])
		}
		if len(parts) > 2 {
			remark = strings.TrimSpace(parts[2])
		}
		if comment != "" {
			if remark != "" {
				remark += "；"
			}
			remark += comment
		}
		for _, token := range strings.Fields(addresses) {
			if label := proxyImportScheme(token); label != "" {
				inherited = label
				continue
			}
			u, explicit, err := normalizeProxyImportToken(token, inherited)
			if err != nil {
				// Do not echo malformed credential strings in input/error fields.
				errs = append(errs, ProxyImportError{Line: n + 1, Error: "地址格式无效，请使用代理 URL 或 IP:端口", Kind: "invalid"})
				continue
			}
			auto := !explicit && inherited == "auto"
			if explicit {
				inherited = u.Scheme
			}
			key := proxyURLKey(u)
			if auto {
				key = "auto:" + key
			}
			if seen[key] {
				errs = append(errs, ProxyImportError{Line: n + 1, Input: proxyImportDisplayURL(u), Error: "重复代理，已跳过", Kind: "duplicate"})
				continue
			}
			seen[key] = true
			// Keep source line as the last ID segment for existing duplicate
			// diagnostics; the ordinal makes same-line IDs unconditionally unique.
			out = append(out, ProxyPoolEntry{ID: fmt.Sprintf("proxy-%d-%d-%d", batch, len(out)+1, n+1), URL: u.String(), Name: name, Remark: remark, Enabled: true, Health: "unknown", ProtocolAuto: auto})
		}
	}
	return out, errs
}
