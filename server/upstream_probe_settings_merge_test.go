package server

import (
	"net/http"
	"strings"
	"testing"

	"github.com/Autumn-27/artex/config"
)

func TestEgressProbeMergeInvalidUpdatePrecedesUnrelatedWrites(t *testing.T) {
	s, request, _ := callbackSettingsFixture(t)
	preserveEgressProbeSetting(t, s.m)
	workers := s.m.Workers()
	for _, address := range []string{"file:///fixture", "http://127.0.0.1/ip", "http://169.254.169.254/ip"} {
		body := `{"egress_probe_url":"` + address + `","callback_addr":"not-saved.fixture:443","workers":9}`
		if response := request(http.MethodPut, body, true); response.Code != http.StatusBadRequest {
			t.Fatalf("invalid probe accepted: HTTP %d", response.Code)
		}
		if s.m.Workers() != workers {
			t.Fatal("invalid probe changed workers")
		}
		if _, found, err := s.m.pg.GetSetting(config.CallbackAddrSetting); err != nil || found {
			t.Fatal("invalid probe changed callback settings")
		}
	}
}

func TestEgressProbeMergeSameAddressKeepsPoolGeneration(t *testing.T) {
	s, request, _ := callbackSettingsFixture(t)
	preserveEgressProbeSetting(t, s.m)
	const custom = "http://10.0.0.5/ip"
	if err := s.m.pg.SetSetting(settingEgressProbeURL, custom); err != nil {
		t.Fatal(err)
	}
	if s.m.proxyPool == nil {
		t.Fatal("proxy pool was not initialized")
	}
	s.m.proxyPool.mu.Lock()
	s.m.proxyPool.generation = 7
	s.m.proxyPool.mu.Unlock()
	response := request(http.MethodPut, `{"egress_probe_url":"`+custom+`"}`, true)
	if response.Code != http.StatusOK {
		t.Fatalf("unchanged probe save HTTP %d", response.Code)
	}
	s.m.proxyPool.mu.Lock()
	generation := s.m.proxyPool.generation
	s.m.proxyPool.mu.Unlock()
	if generation != 7 {
		t.Fatal("unchanged probe address invalidated the pool")
	}
	if !strings.Contains(response.Body.String(), custom) {
		t.Fatal("settings did not return the saved custom probe")
	}
}
