package traffic

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"sync"
	"time"

	"golang.org/x/net/proxy"
)

// go-mitmproxy always offers SOCKS5 password authentication, including when no
// credentials are configured. This private SOCKS5 bridge authenticates the
// library locally, then uses the same nil-auth upstream semantics as net/http.
// SOCKS framing reads exact lengths so even server-first tunnel data is kept.
type socksBridge struct {
	url         *url.URL
	password    string
	listener    net.Listener
	route       func() *url.URL
	failure     func(error)
	ctx         context.Context
	cancel      context.CancelFunc
	mu          sync.Mutex
	closed      bool
	connections map[net.Conn]bool
	workers     sync.WaitGroup
}

func newSOCKSBridge(route func() *url.URL, failure func(error)) (*socksBridge, error) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	password := rand.Text()
	ctx, cancel := context.WithCancel(context.Background())
	b := &socksBridge{url: &url.URL{Scheme: "socks5", Host: listener.Addr().String(), User: url.UserPassword("artex", password)}, password: password, listener: listener, route: route, failure: failure, ctx: ctx, cancel: cancel, connections: map[net.Conn]bool{}}
	go b.accept()
	return b, nil
}

func (b *socksBridge) accept() {
	for {
		client, err := b.listener.Accept()
		if err != nil {
			return
		}
		b.mu.Lock()
		if b.closed {
			b.mu.Unlock()
			_ = client.Close()
			return
		}
		b.connections[client] = true
		b.workers.Add(1)
		b.mu.Unlock()
		go func() { defer b.workers.Done(); defer b.release(client); b.serve(client) }()
	}
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
	err := b.listener.Close()
	b.workers.Wait()
	return err
}

func socksReadField(conn net.Conn) ([]byte, error) {
	length := []byte{0}
	if _, err := io.ReadFull(conn, length); err != nil {
		return nil, err
	}
	value := make([]byte, int(length[0]))
	_, err := io.ReadFull(conn, value)
	return value, err
}

func (b *socksBridge) authenticate(client net.Conn) bool {
	header := make([]byte, 2)
	if _, err := io.ReadFull(client, header); err != nil || header[0] != 5 {
		return false
	}
	methods := make([]byte, int(header[1]))
	if _, err := io.ReadFull(client, methods); err != nil {
		return false
	}
	passwordMethod := false
	for _, method := range methods {
		passwordMethod = passwordMethod || method == 2
	}
	if !passwordMethod {
		_, _ = client.Write([]byte{5, 255})
		return false
	}
	if _, err := client.Write([]byte{5, 2}); err != nil {
		return false
	}
	if _, err := io.ReadFull(client, header[:1]); err != nil || header[0] != 1 {
		return false
	}
	username, err := socksReadField(client)
	if err != nil {
		return false
	}
	password, err := socksReadField(client)
	if err != nil {
		return false
	}
	valid := subtle.ConstantTimeCompare(username, []byte("artex")) & subtle.ConstantTimeCompare(password, []byte(b.password))
	if valid != 1 {
		_, _ = client.Write([]byte{1, 1})
		return false
	}
	_, err = client.Write([]byte{1, 0})
	return err == nil
}

func socksConnectTarget(client net.Conn) (string, error) {
	header := make([]byte, 4)
	if _, err := io.ReadFull(client, header); err != nil {
		return "", err
	}
	if header[0] != 5 || header[1] != 1 || header[2] != 0 {
		return "", errors.New("unsupported SOCKS request")
	}
	var host string
	switch header[3] {
	case 1, 4:
		length := 4
		if header[3] == 4 {
			length = 16
		}
		address := make([]byte, length)
		if _, err := io.ReadFull(client, address); err != nil {
			return "", err
		}
		host = net.IP(address).String()
	case 3:
		name, err := socksReadField(client)
		if err != nil {
			return "", err
		}
		host = string(name)
	default:
		return "", errors.New("unsupported SOCKS address")
	}
	port := make([]byte, 2)
	if _, err := io.ReadFull(client, port); err != nil {
		return "", err
	}
	if host == "" || binary.BigEndian.Uint16(port) == 0 {
		return "", errors.New("empty SOCKS destination")
	}
	return net.JoinHostPort(host, strconv.Itoa(int(binary.BigEndian.Uint16(port)))), nil
}

func (b *socksBridge) serve(client net.Conn) {
	_ = client.SetDeadline(time.Now().Add(10 * time.Second))
	if !b.authenticate(client) {
		return
	}
	target, err := socksConnectTarget(client)
	if err != nil {
		_, _ = client.Write([]byte{5, 7, 0, 1, 0, 0, 0, 0, 0, 0})
		return
	}
	upstream := b.route()
	if upstream == nil || upstream.Scheme != "socks5" {
		_, _ = client.Write([]byte{5, 1, 0, 1, 0, 0, 0, 0, 0, 0})
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
		b.fail(client, err)
		return
	}
	ctx, cancel := context.WithTimeout(b.ctx, 10*time.Second)
	remote, err := dialer.(proxy.ContextDialer).DialContext(ctx, "tcp", target)
	cancel()
	if err != nil {
		b.fail(client, err)
		return
	}
	if !b.track(remote) {
		return
	}
	defer b.release(remote)
	if _, err := client.Write([]byte{5, 0, 0, 1, 127, 0, 0, 1, 0, 0}); err != nil {
		return
	}
	_ = client.SetDeadline(time.Time{})
	done := make(chan struct{})
	go func() { defer close(done); _, _ = io.Copy(remote, client); _ = remote.Close() }()
	_, _ = io.Copy(client, remote)
	_ = client.Close()
	_ = remote.Close()
	<-done
}

func (b *socksBridge) fail(client net.Conn, err error) {
	if b.failure != nil {
		b.failure(err)
	}
	_, _ = client.Write([]byte{5, 1, 0, 1, 0, 0, 0, 0, 0, 0})
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
