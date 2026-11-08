package traffic

import (
	"bufio"
	"encoding/base64"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	mproxy "github.com/lqqyt2423/go-mitmproxy/proxy"
)

// The real capture listener must turn SOCKS failures into HTTP 502 and notify
// its normal error hook. An EOF alone is insufficient: net/http recovers a
// handler panic and closes that connection, which is the reported regression.
type socksFailureObserver struct {
	mproxy.BaseAddon
	errors chan error
}

func (o *socksFailureObserver) HTTPConnectError(_ *mproxy.Flow, err error) {
	select {
	case o.errors <- err:
	default:
	}
}

func socksFixture(t *testing.T, mode string) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	var workers sync.WaitGroup
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			workers.Add(1)
			go func() {
				defer workers.Done()
				defer conn.Close()
				_ = conn.SetDeadline(time.Now().Add(3 * time.Second))
				header := make([]byte, 2)
				if _, err := io.ReadFull(conn, header); err != nil || header[0] != 5 {
					return
				}
				methods := make([]byte, int(header[1]))
				if _, err := io.ReadFull(conn, methods); err != nil {
					return
				}
				if mode == "prefer-auth" {
					for _, method := range methods {
						if method == 2 {
							_, _ = conn.Write([]byte{5, 2})
							return
						}
					}
				}
				switch mode {
				case "truncated-greeting":
					_, _ = conn.Write([]byte{5})
					return
				case "methods-rejected":
					_, _ = conn.Write([]byte{5, 255})
					return
				case "auth-rejected":
					_, _ = conn.Write([]byte{5, 2})
					if _, err := io.ReadFull(conn, header); err != nil || header[0] != 1 {
						return
					}
					if _, err := io.CopyN(io.Discard, conn, int64(header[1])); err != nil {
						return
					}
					if _, err := io.ReadFull(conn, header[:1]); err != nil {
						return
					}
					if _, err := io.CopyN(io.Discard, conn, int64(header[0])); err != nil {
						return
					}
					_, _ = conn.Write([]byte{1, 1})
					return
				}
				_, _ = conn.Write([]byte{5, 0})
				request := make([]byte, 4)
				if _, err := io.ReadFull(conn, request); err != nil || request[0] != 5 || request[1] != 1 {
					return
				}
				length := 0
				switch request[3] {
				case 1:
					length = 4
				case 4:
					length = 16
				case 3:
					if _, err := io.ReadFull(conn, header[:1]); err != nil {
						return
					}
					length = int(header[0])
				default:
					return
				}
				if _, err := io.CopyN(io.Discard, conn, int64(length+2)); err != nil {
					return
				}
				switch mode {
				case "connect-rejected":
					_, _ = conn.Write([]byte{5, 5, 0, 1, 127, 0, 0, 1, 0, 0})
				case "truncated-connect":
					_, _ = conn.Write([]byte{5, 0})
				case "success", "prefer-auth":
					_, _ = conn.Write([]byte{5, 0, 0, 1, 127, 0, 0, 1, 0, 0})
					_, _ = conn.Write([]byte("fixture-tunnel-ready\n"))
					_, _ = io.Copy(conn, conn)
				}
			}()
		}
	}()
	t.Cleanup(func() { _ = listener.Close(); <-done; workers.Wait() })
	u := &url.URL{Scheme: "socks5", Host: listener.Addr().String()}
	if mode == "auth-rejected" {
		u.User = url.UserPassword("fixture-user", "fixture-secret")
	}
	return u.String()
}

func TestSOCKS5CaptureDoesNotOfferAbsentCredentials(t *testing.T) {
	t.Setenv("ARTEX_PROXY_AUTH", "capture-fixture:local-secret")
	for _, shared := range []bool{false, true} {
		for _, intercepted := range []bool{false, true} {
			t.Run(fmt.Sprintf("shared=%t/intercept=%t", shared, intercepted), func(t *testing.T) {
				parent, err := Open(t.TempDir(), sharedProxyAddress(t))
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = parent.Close() })
				capture := parent
				if shared {
					capture, err = parent.OpenProxy(t.TempDir(), sharedProxyAddress(t))
					if err != nil {
						t.Fatal(err)
					}
				}
				capture.proxy.SetShouldInterceptRule(func(*http.Request) bool { return intercepted })
				upstream := socksFixture(t, "prefer-auth")
				if err := capture.SetUpstreamProxy(upstream); err != nil {
					t.Fatal(err)
				}
				startSharedProxy(t, capture)
				conn, _, status := captureConnect(t, capture, "echo.fixture.invalid:443")
				_ = conn.Close()
				if status != http.StatusOK {
					t.Fatalf("credential-free SOCKS5 capture status=%d; must not offer password authentication", status)
				}
			})
		}
	}
}

func captureConnect(t *testing.T, capture *Traffic, target string) (net.Conn, *bufio.Reader, int) {
	t.Helper()
	u, err := url.Parse(capture.ProxyAddr())
	if err != nil {
		t.Fatal(err)
	}
	conn, err := (&net.Dialer{Timeout: time.Second}).DialContext(t.Context(), "tcp", u.Host)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	_ = conn.SetDeadline(time.Now().Add(3 * time.Second))
	req := &http.Request{Method: http.MethodConnect, URL: &url.URL{Opaque: target}, Host: target, Header: http.Header{}}
	user := u.User.Username()
	password, _ := u.User.Password()
	req.Header.Set("Proxy-Authorization", "Basic "+base64.StdEncoding.EncodeToString([]byte(user+":"+password)))
	if err := req.Write(conn); err != nil {
		t.Fatal(err)
	}
	reader := bufio.NewReader(conn)
	response, err := http.ReadResponse(reader, req)
	if err != nil {
		t.Fatalf("capture returned no HTTP response (possible handler panic): %v", err)
	}
	if response.StatusCode != http.StatusOK {
		_ = response.Body.Close()
	}
	return conn, reader, response.StatusCode
}

func TestSOCKS5UpstreamFailuresReturn502(t *testing.T) {
	t.Setenv("ARTEX_PROXY_AUTH", "capture-fixture:local-secret")
	t.Setenv("ARTEX_PROXY_SSL_INSECURE", "false")
	for _, shared := range []bool{false, true} {
		for _, intercepted := range []bool{false, true} {
			t.Run(fmt.Sprintf("shared=%t/intercept=%t", shared, intercepted), func(t *testing.T) {
				parent, err := Open(t.TempDir(), sharedProxyAddress(t))
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = parent.Close() })
				capture := parent
				if shared {
					var err error
					capture, err = parent.OpenProxy(t.TempDir(), sharedProxyAddress(t))
					if err != nil {
						t.Fatal(err)
					}
				}
				capture.proxy.SetShouldInterceptRule(func(*http.Request) bool { return intercepted })
				observer := &socksFailureObserver{errors: make(chan error, 16)}
				capture.proxy.AddAddon(observer)
				startSharedProxy(t, capture)
				// A listening target would observe any accidental direct fallback.
				target, err := net.Listen("tcp", "127.0.0.1:0")
				if err != nil {
					t.Fatal(err)
				}
				defer target.Close()
				for _, mode := range []string{"connection-refused", "methods-rejected", "auth-rejected", "connect-rejected", "truncated-greeting", "truncated-connect"} {
					t.Run(mode, func(t *testing.T) {
						upstream := "socks5://" + sharedProxyAddress(t)
						if mode != "connection-refused" {
							upstream = socksFixture(t, mode)
						}
						if err := capture.SetUpstreamProxy(upstream); err != nil {
							t.Fatal(err)
						}
						for range 2 {
							conn, _, status := captureConnect(t, capture, target.Addr().String())
							_ = conn.Close()
							if status != http.StatusBadGateway {
								t.Fatalf("failure status=%d, want 502", status)
							}
							select {
							case err := <-observer.errors:
								if err == nil {
									t.Fatal("missing SOCKS error")
								}
							case <-time.After(time.Second):
								t.Fatal("normal CONNECT failure hook was bypassed")
							}
						}
						if capture.UpstreamProxy() != upstream {
							t.Fatal("SOCKS failure changed the configured route")
						}
					})
				}
				_ = target.(*net.TCPListener).SetDeadline(time.Now().Add(50 * time.Millisecond))
				conn, err := target.Accept()
				if err == nil {
					_ = conn.Close()
					t.Fatal("failed SOCKS connection silently fell back to direct")
				}
				if network, ok := err.(net.Error); !ok || !network.Timeout() {
					t.Fatal(err)
				}
				// After failures the same listener must still establish a tunnel
				// when its upstream becomes usable, without rebuilding the proxy.
				capture.proxy.SetShouldInterceptRule(func(*http.Request) bool { return false })
				working := socksFixture(t, "success")
				if err := capture.SetUpstreamProxy(working); err != nil {
					t.Fatal(err)
				}
				conn, reader, status := captureConnect(t, capture, target.Addr().String())
				if status != http.StatusOK {
					t.Fatalf("recovered tunnel status=%d", status)
				}
				line, err := reader.ReadString('\n')
				if err != nil || strings.TrimSpace(line) != "fixture-tunnel-ready" {
					t.Fatal("SOCKS tunnel not established", err, line)
				}
				_, _ = io.WriteString(conn, "after-failure\n")
				line, err = reader.ReadString('\n')
				_ = conn.Close()
				if err != nil || line != "after-failure\n" {
					t.Fatal("recovered tunnel did not forward", err, line)
				}
			})
		}
	}
}
