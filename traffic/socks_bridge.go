package traffic

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"sync"
	"time"

	"golang.org/x/net/proxy"
)

// go-mitmproxy's CONNECT helper always offers password authentication for
// SOCKS5, even without URL credentials. Use the same nil-auth semantics as
// net/http probes through a private HTTP bridge. It never falls back to direct.
type socksBridge struct {
	url           *url.URL
	authorization string
	server        *http.Server
	route         func() *url.URL
	failure       func(error)
	cancel        context.CancelFunc
	mu            sync.Mutex
	closed        bool
	connections   map[net.Conn]bool
	workers       sync.WaitGroup
}

func newSOCKSBridge(route func() *url.URL, failure func(error)) (*socksBridge, error) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	password := rand.Text()
	ctx, cancel := context.WithCancel(context.Background())
	b := &socksBridge{url: &url.URL{Scheme: "http", Host: listener.Addr().String(), User: url.UserPassword("artex", password)},
		authorization: "Basic " + base64.StdEncoding.EncodeToString([]byte("artex:"+password)), route: route, failure: failure, cancel: cancel, connections: map[net.Conn]bool{}}
	b.server = &http.Server{Handler: b, ReadHeaderTimeout: 5 * time.Second, IdleTimeout: 30 * time.Second, MaxHeaderBytes: 64 << 10,
		BaseContext: func(net.Listener) context.Context { return ctx }}
	go func() { _ = b.server.Serve(listener) }()
	return b, nil
}

func (b *socksBridge) track(conn net.Conn) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		_ = conn.Close()
		return false
	}
	b.connections[conn] = true
	return true
}

func (b *socksBridge) release(conn net.Conn) {
	_ = conn.Close()
	b.mu.Lock()
	delete(b.connections, conn)
	b.mu.Unlock()
}

func (b *socksBridge) Close() error {
	b.mu.Lock()
	b.closed = true
	for conn := range b.connections {
		_ = conn.Close()
	}
	b.mu.Unlock()
	b.cancel()
	err := b.server.Close()
	b.workers.Wait()
	return err
}

func (b *socksBridge) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if subtle.ConstantTimeCompare([]byte(r.Header.Get("Proxy-Authorization")), []byte(b.authorization)) != 1 {
		w.WriteHeader(http.StatusProxyAuthRequired)
		return
	}
	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		w.WriteHeader(http.StatusServiceUnavailable)
		return
	}
	b.workers.Add(1)
	b.mu.Unlock()
	defer b.workers.Done()
	upstream := b.route()
	if upstream == nil || upstream.Scheme != "socks5" {
		w.WriteHeader(http.StatusBadGateway)
		return
	}
	if r.Method != http.MethodConnect {
		b.forwardHTTP(w, r, upstream)
		return
	}
	var auth *proxy.Auth
	if upstream.User != nil {
		password, _ := upstream.User.Password()
		auth = &proxy.Auth{User: upstream.User.Username(), Password: password}
	}
	address := upstream.Host
	if upstream.Port() == "" {
		address = net.JoinHostPort(upstream.Hostname(), "1080")
	}
	dialer, err := proxy.SOCKS5("tcp", address, auth, &net.Dialer{Timeout: 10 * time.Second})
	if err != nil {
		b.fail(w, err)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	remote, err := dialer.(proxy.ContextDialer).DialContext(ctx, "tcp", r.Host)
	cancel()
	if err != nil {
		b.fail(w, err)
		return
	}
	if !b.track(remote) {
		return
	}
	defer b.release(remote)
	client, buffered, err := http.NewResponseController(w).Hijack()
	if err != nil {
		return
	}
	if !b.track(client) {
		return
	}
	defer b.release(client)
	if _, err := buffered.WriteString("HTTP/1.1 200 Connection Established\r\n\r\n"); err != nil {
		return
	}
	if err := buffered.Flush(); err != nil {
		return
	}
	done := make(chan struct{})
	go func() { defer close(done); _, _ = io.Copy(remote, buffered.Reader); _ = remote.Close() }()
	_, _ = io.Copy(client, remote)
	_ = client.Close()
	_ = remote.Close()
	<-done
}

func (b *socksBridge) fail(w http.ResponseWriter, err error) {
	if b.failure != nil {
		b.failure(err)
	}
	http.Error(w, "SOCKS5 upstream unavailable", http.StatusBadGateway)
}

func (b *socksBridge) forwardHTTP(w http.ResponseWriter, r *http.Request, upstream *url.URL) {
	if r.URL.Scheme != "http" || r.URL.Host == "" {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	request := r.Clone(r.Context())
	request.RequestURI = ""
	request.Header.Del("Proxy-Authorization")
	request.Header.Del("Proxy-Connection")
	transport := &http.Transport{Proxy: http.ProxyURL(upstream), DisableKeepAlives: true, ResponseHeaderTimeout: 10 * time.Second}
	defer transport.CloseIdleConnections()
	response, err := transport.RoundTrip(request)
	if err != nil {
		b.fail(w, err)
		return
	}
	defer response.Body.Close()
	for name, values := range response.Header {
		for _, value := range values {
			w.Header().Add(name, value)
		}
	}
	w.WriteHeader(response.StatusCode)
	_, _ = io.Copy(w, response.Body)
}

func (t *Traffic) captureUpstream(_ *http.Request) (*url.URL, error) {
	upstream := t.upstream.Load()
	if upstream == nil || upstream.Scheme != "socks5" {
		return upstream, nil
	}
	t.bridgeMu.Lock()
	defer t.bridgeMu.Unlock()
	if t.stopping() {
		return nil, errors.New("capture proxy stopped")
	}
	if t.socksBridge == nil {
		bridge, err := newSOCKSBridge(t.upstream.Load, t.reportUpstreamFailure)
		if err != nil {
			return nil, err
		}
		t.socksBridge = bridge
	}
	return t.socksBridge.url, nil
}
