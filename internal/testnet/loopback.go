// Package testnet is only imported by test entrypoints. It keeps regression
// fixtures from reaching a real provider when a saved dummy profile has no URL.
package testnet

import (
	"context"
	"fmt"
	"net"
	"net/http"
)

func LoopbackOnly() {
	tr := http.DefaultTransport.(*http.Transport).Clone()
	original := tr.DialContext
	if original == nil {
		original = (&net.Dialer{}).DialContext
	}
	tr.DialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
		host, _, err := net.SplitHostPort(addr)
		if err != nil {
			return nil, err
		}
		ip := net.ParseIP(host)
		if host != "localhost" && (ip == nil || !ip.IsLoopback()) {
			return nil, fmt.Errorf("fixture egress blocked: only loopback HTTP endpoints are allowed")
		}
		return original(ctx, network, addr)
	}
	http.DefaultTransport = tr
}
