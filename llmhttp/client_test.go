package llmhttp

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestNewClientUsesNativeSOCKS5Proxy(t *testing.T) {
	client, err := NewClient("socks5://user:pass@127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	transport, ok := client.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("transport type=%T", client.Transport)
	}
	proxyURL, err := transport.Proxy(httptest.NewRequest(http.MethodGet, "https://target.invalid", nil))
	if err != nil {
		t.Fatal(err)
	}
	if proxyURL == nil || proxyURL.Scheme != "socks5" || proxyURL.Host != "127.0.0.1" || proxyURL.User.String() != "user:pass" {
		t.Fatalf("proxy URL=%v", proxyURL)
	}
}

func TestNewClientEmptyProxyIgnoresEnvironment(t *testing.T) {
	t.Setenv("HTTPS_PROXY", "http://environment-proxy.invalid:8181")
	client, err := NewClient("")
	if err != nil {
		t.Fatal(err)
	}
	if transport, ok := client.Transport.(*http.Transport); !ok || transport.Proxy != nil {
		t.Fatal("empty explicit proxy inherited process environment")
	}
}

func TestNewClientRejectsMalformedProxiesWithoutCredentials(t *testing.T) {
	for _, proxy := range []string{"http://", "ftp://user:fixture-secret@host.invalid", "http://user:fixture-secret@%zz"} {
		_, err := NewClient(proxy)
		if err == nil || strings.Contains(err.Error(), "fixture-secret") {
			t.Fatalf("invalid proxy accepted or secret exposed: %v", err)
		}
	}
}
