package config

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

type callbackSettingFixture struct {
	value string
	found bool
	err   error
	reads int
}

func (f *callbackSettingFixture) GetSetting(key string) (string, bool, error) {
	f.reads++
	if key != CallbackAddrSetting {
		return "", false, errors.New("wrong setting key")
	}
	return f.value, f.found, f.err
}

func callbackFileFixture(t *testing.T, address string) {
	t.Helper()
	p := filepath.Join(t.TempDir(), "config.json")
	data, err := json.Marshal(Config{CallbackAddr: address})
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(p, data, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ARTEX_CONFIG", p)
}

func TestCallbackRuntimePrecedenceAndExplicitEmpty(t *testing.T) {
	callbackFileFixture(t, "file.example.test:443")
	t.Setenv("ARTEX_CALLBACK_ADDR", "")
	store := &callbackSettingFixture{}
	got, err := ResolveCallbackAddr(store)
	if err != nil || got != (CallbackAddress{Address: "file.example.test:443", Source: "config"}) {
		t.Fatalf("file fallback = %+v, %v", got, err)
	}
	store.value, store.found = " UI.Example.Test:08443 ", true
	got, err = ResolveCallbackAddr(store)
	if err != nil || got != (CallbackAddress{Address: "ui.example.test:8443", Source: "settings"}) {
		t.Fatalf("live DB value = %+v, %v", got, err)
	}
	store.value = ""
	got, err = ResolveCallbackAddr(store)
	if err != nil || got != (CallbackAddress{Source: "settings"}) {
		t.Fatalf("explicit clear fell back to file: %+v, %v", got, err)
	}
	t.Setenv("ARTEX_CALLBACK_ADDR", " ENV.Example.Test ")
	reads := store.reads
	got, err = ResolveCallbackAddr(store)
	if err != nil || got != (CallbackAddress{Address: "env.example.test", Source: "env", Locked: true}) || store.reads != reads {
		t.Fatalf("environment override = %+v, %v, reads=%d", got, err, store.reads)
	}
	// Removing the override reveals the persisted explicit empty value again.
	t.Setenv("ARTEX_CALLBACK_ADDR", " ")
	got, err = ResolveCallbackAddr(store)
	if err != nil || got.Address != "" || got.Source != "settings" || got.Locked {
		t.Fatalf("environment removed = %+v, %v", got, err)
	}
}

func TestCallbackReadAndValidationErrorsDoNotFallback(t *testing.T) {
	callbackFileFixture(t, "file.example.test")
	t.Setenv("ARTEX_CALLBACK_ADDR", "")
	want := errors.New("fixture read failure")
	store := &callbackSettingFixture{err: want}
	if got, err := ResolveCallbackAddr(store); !errors.Is(err, want) || got.Address != "" {
		t.Fatalf("read error fell back: %+v, %v", got, err)
	}
	store.err, store.found, store.value = nil, true, "http://invalid.example.test"
	if got, err := ResolveCallbackAddr(store); err == nil || got.Address != store.value || got.Source != "settings" {
		t.Fatalf("invalid DB address fell back: %+v, %v", got, err)
	}
	t.Setenv("ARTEX_CALLBACK_ADDR", "invalid;command")
	if got, err := ResolveCallbackAddr(store); err == nil || !got.Locked || got.Source != "env" {
		t.Fatalf("invalid env address fell back: %+v, %v", got, err)
	}
}

func TestCallbackUnsetAndNilStore(t *testing.T) {
	t.Setenv("ARTEX_CALLBACK_ADDR", "")
	callbackFileFixture(t, "")
	got, err := ResolveCallbackAddr(nil)
	if err != nil || got != (CallbackAddress{Source: "unset"}) {
		t.Fatalf("unset = %+v, %v", got, err)
	}
	callbackFileFixture(t, "0.0.0.0")
	if _, err := ResolveCallbackAddr(nil); err == nil {
		t.Fatal("invalid file address accepted")
	}
}

func TestValidateCallbackAddressForms(t *testing.T) {
	cases := []struct{ in, normalized, host string }{
		{"", "", ""},
		{"   ", "", ""},
		{"  CALLBACK.Example.Test  ", "callback.example.test", "callback.example.test"},
		{"HOST:1", "host:1", "host"},
		{"host:65535", "host:65535", "host"},
		{"host:00443", "host:443", "host"},
		{"host.example.test.", "host.example.test.", "host.example.test."},
		{"127.0.0.1", "127.0.0.1", "127.0.0.1"},
		{"localhost:4444", "localhost:4444", "localhost"},
		{"192.0.2.15:8787", "192.0.2.15:8787", "192.0.2.15"},
		{"::1", "::1", "::1"},
		{"[::1]", "::1", "::1"},
		{"2001:db8::15", "2001:db8::15", "2001:db8::15"},
		{"[2001:db8::15]:443", "[2001:db8::15]:443", "2001:db8::15"},
		{"fe80::15%en0", "fe80::15%en0", "fe80::15%en0"},
		{"[fe80::15%en0]:443", "[fe80::15%en0]:443", "fe80::15%en0"},
	}
	for _, tt := range cases {
		t.Run(tt.in, func(t *testing.T) {
			got, err := ValidateCallbackAddr(tt.in)
			if err != nil || got != tt.normalized {
				t.Fatalf("normalized = %q, %v, want %q", got, err, tt.normalized)
			}
			host, err := CallbackHostname(tt.in)
			if err != nil || host != tt.host {
				t.Fatalf("host = %q, %v, want %q", host, err, tt.host)
			}
		})
	}
}

func TestValidateCallbackRejectsInvalidAndCommandForms(t *testing.T) {
	for _, raw := range []string{
		"http://host", "https://host:443", "host/path", "user@host", "host?x", "host#x",
		"host;id", "host|id", "host&id", "$(id)", "`id`", "host'", `host"`, "host\\path",
		"host name", "host\n", "\thost", "host\x00", "host\u2003name", "host:0", "host:65536",
		"host:-1", "host:+1", "host:", ":443", "host:port", "[]", "[host]:443", "[192.0.2.15]:443", "[::1]:0",
		"0.0.0.0", "0.0.0.0:443", "::", "[::]:443", "224.0.0.1", "ff02::1",
		"255.255.255.255", "::ffff:0.0.0.0", "::ffff:255.255.255.255", "999.1.1.1",
		"-host", "host-", "host..test", ".host", "host_name", "[fe80::1%en0;id]:443",
	} {
		t.Run(raw, func(t *testing.T) {
			if got, err := ValidateCallbackAddr(raw); err == nil {
				t.Fatalf("accepted %q as %q", raw, got)
			}
		})
	}
}
