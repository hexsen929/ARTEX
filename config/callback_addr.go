package config

import (
	"fmt"
	"net"
	"net/netip"
	"os"
	"strconv"
	"strings"
	"unicode"
)

// CallbackAddrSetting is stored in the normal application settings table. An
// existing empty value deliberately disables the file fallback.
const CallbackAddrSetting = "callback_addr"

// CallbackSettingStore keeps the resolver usable by the server and read-only
// deployment diagnostics without making config depend on the database package.
type CallbackSettingStore interface {
	GetSetting(string) (value string, found bool, err error)
}

type CallbackAddress struct {
	Address string
	Source  string // env | settings | config | unset
	Locked  bool   // explicit environment configuration takes precedence over UI
}

// ResolveCallbackAddr reads the effective address on every call. Database read
// errors and invalid higher-priority values are returned, never hidden by a
// fallback to an older address. This resolver does not touch active resources.
func ResolveCallbackAddr(store CallbackSettingStore) (CallbackAddress, error) {
	if raw := os.Getenv("ARTEX_CALLBACK_ADDR"); strings.TrimSpace(raw) != "" {
		address, err := ValidateCallbackAddr(raw)
		result := CallbackAddress{Address: address, Source: "env", Locked: true}
		if err != nil {
			result.Address = strings.TrimSpace(raw)
			return result, fmt.Errorf("环境变量回连地址无效: %w", err)
		}
		return result, nil
	}
	if store != nil {
		raw, found, err := store.GetSetting(CallbackAddrSetting)
		if err != nil {
			return CallbackAddress{}, fmt.Errorf("读取回连地址设置失败: %w", err)
		}
		if found {
			address, err := ValidateCallbackAddr(raw)
			result := CallbackAddress{Address: address, Source: "settings"}
			if err != nil {
				result.Address = strings.TrimSpace(raw)
				return result, fmt.Errorf("后台回连地址无效: %w", err)
			}
			return result, nil
		}
	}
	if raw := Load().CallbackAddr; strings.TrimSpace(raw) != "" {
		address, err := ValidateCallbackAddr(raw)
		result := CallbackAddress{Address: address, Source: "config"}
		if err != nil {
			result.Address = strings.TrimSpace(raw)
			return result, fmt.Errorf("配置文件回连地址无效: %w", err)
		}
		return result, nil
	}
	return CallbackAddress{Source: "unset"}, nil
}

// ValidateCallbackAddr accepts a host/IP and an optional legacy port. It does
// not resolve DNS, probe the host, open a listener or alter network settings.
// Loopback is valid for a local fixture; wildcard and multicast destinations
// cannot identify a callback endpoint and are rejected.
func ValidateCallbackAddr(raw string) (string, error) {
	for _, r := range raw {
		if unicode.IsControl(r) {
			return "", fmt.Errorf("回连地址不允许控制字符")
		}
	}
	address := strings.TrimSpace(raw)
	if address == "" {
		return "", nil
	}
	if len(address) > 320 {
		return "", fmt.Errorf("回连地址过长")
	}
	for _, r := range address {
		if unicode.IsSpace(r) {
			return "", fmt.Errorf("回连地址不允许空格")
		}
	}
	if strings.ContainsAny(address, "/\\@?#'\"`$;&|<>(){}!*+=,") {
		return "", fmt.Errorf("请填写 IP 或域名，可附端口，不填写协议、路径或命令字符")
	}

	// A valid bare IPv6 address (including a zone) must be recognized before
	// interpreting colons as a host:port separator.
	if ip, err := netip.ParseAddr(address); err == nil {
		return callbackIP(ip)
	}
	if strings.HasPrefix(address, "[") && strings.HasSuffix(address, "]") {
		ip, err := netip.ParseAddr(address[1 : len(address)-1])
		if err != nil || !ip.Is6() {
			return "", fmt.Errorf("方括号仅用于 IPv6 地址")
		}
		return callbackIP(ip)
	}

	host, port := address, ""
	if strings.ContainsAny(address, ":[]") {
		var err error
		host, port, err = net.SplitHostPort(address)
		if err != nil || port == "" {
			return "", fmt.Errorf("回连地址格式应为 IP、域名或 host:port；IPv6 附端口时使用方括号")
		}
		for _, r := range port {
			if r < '0' || r > '9' {
				return "", fmt.Errorf("回连端口应为 1 到 65535 的整数")
			}
		}
		n, err := strconv.Atoi(port)
		if err != nil || n < 1 || n > 65535 {
			return "", fmt.Errorf("回连端口应为 1 到 65535 的整数")
		}
		port = strconv.Itoa(n)
	}

	var normalized string
	if ip, err := netip.ParseAddr(host); err == nil {
		var err error
		normalized, err = callbackIP(ip)
		if err != nil {
			return "", err
		}
	} else {
		var err error
		normalized, err = callbackDNSName(host)
		if err != nil {
			return "", err
		}
	}
	if port != "" {
		return net.JoinHostPort(normalized, port), nil
	}
	return normalized, nil
}

func callbackIP(ip netip.Addr) (string, error) {
	unmapped := ip.Unmap()
	if unmapped.IsUnspecified() || unmapped.IsMulticast() || unmapped == netip.AddrFrom4([4]byte{255, 255, 255, 255}) {
		return "", fmt.Errorf("请使用具体主机地址，不使用通配、广播或组播地址")
	}
	if zone := ip.Zone(); zone != "" {
		for _, r := range zone {
			if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '_' || r == '-' || r == '.') {
				return "", fmt.Errorf("IPv6 网卡标识格式无效")
			}
		}
	}
	return ip.String(), nil
}

func callbackDNSName(host string) (string, error) {
	base := strings.TrimSuffix(host, ".")
	if base == "" || len(base) > 253 {
		return "", fmt.Errorf("回连域名格式无效")
	}
	allNumeric := true
	for _, r := range base {
		if r != '.' && (r < '0' || r > '9') {
			allNumeric = false
			break
		}
	}
	if allNumeric && strings.Contains(base, ".") {
		return "", fmt.Errorf("回连 IP 地址格式无效")
	}
	for _, label := range strings.Split(base, ".") {
		if len(label) == 0 || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return "", fmt.Errorf("回连域名格式无效")
		}
		for _, r := range label {
			if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-') {
				return "", fmt.Errorf("回连域名应使用字母、数字和连字符")
			}
		}
	}
	return strings.ToLower(host), nil
}

// CallbackHostname returns the host portion for consumers that select their
// listener port separately. A legacy port is validated but never silently used
// to change an already configured listener or tunnel.
func CallbackHostname(address string) (string, error) {
	normalized, err := ValidateCallbackAddr(address)
	if err != nil {
		return "", err
	}
	if normalized == "" {
		return "", nil
	}
	if _, err := netip.ParseAddr(normalized); err == nil {
		return normalized, nil
	}
	if host, _, err := net.SplitHostPort(normalized); err == nil {
		return host, nil
	}
	return normalized, nil
}
