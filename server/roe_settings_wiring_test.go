package server

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Autumn-27/artex/config"
	"github.com/Autumn-27/artex/db"
	"github.com/Autumn-27/artex/guard"
)

func roeSettingsFixture(t *testing.T) (*Server, func(string, string, bool) *httptest.ResponseRecorder) {
	t.Helper()
	s, request, _ := callbackSettingsFixture(t)
	before, found, err := s.m.pg.GetSetting(settingRoEEnforcement)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if found {
			_ = s.m.pg.SetSetting(settingRoEEnforcement, before)
		} else {
			_ = s.m.pg.DeleteSetting(settingRoEEnforcement)
		}
	})
	if err := s.m.pg.DeleteSetting(settingRoEEnforcement); err != nil {
		t.Fatal(err)
	}
	return s, request
}

func TestRoESettingsDefaultReadDoesNotPersistOrDisableScope(t *testing.T) {
	s, request := roeSettingsFixture(t)
	if response := request(http.MethodPut, `{"roe_enforcement":"off"}`, false); response.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated policy update: %d", response.Code)
	}
	payload := callbackPayload(t, request(http.MethodGet, "", true))
	if payload["roe_enforcement"] != "warn" {
		t.Fatalf("unsaved mode=%#v, want original warn default", payload["roe_enforcement"])
	}
	if _, saved, err := s.m.pg.GetSetting(settingRoEEnforcement); err != nil || saved {
		t.Fatalf("GET or rejected write persisted a policy value: saved=%v err=%v", saved, err)
	}
}

func TestRoESettingsRoundTripAffectsNextGuardReadWithoutRebuild(t *testing.T) {
	s, request := roeSettingsFixture(t)
	cfg := newRoEConfig(s.m.pg)
	cfg.TaskIDOf = func(context.Context) int64 { return 1 }
	cfg.ScopeWithError = func(int64) ([]guard.ScopeRule, error) {
		return []guard.ScopeRule{{Kind: "domain", Value: "allowed.fixture"}}, nil
	}
	g := guard.New()
	g.SetRoE(cfg)
	input := json.RawMessage(`{"command":"curl https://outside.fixture/"}`)
	for _, mode := range []string{"strict", "warn", "off", "strict"} {
		body, _ := json.Marshal(map[string]string{"roe_enforcement": mode})
		payload := callbackPayload(t, request(http.MethodPut, string(body), true))
		if payload["roe_enforcement"] != mode {
			t.Fatalf("saved mode=%v, want %s", payload["roe_enforcement"], mode)
		}
		// Only the pre-tool hook is evaluated: no command, model or HTTP target
		// is executed by this synthetic fixture.
		blocked, _, _ := g.Hooks().PreToolUse(t.Context(), "Bash", input)
		if blocked != (mode == "strict") {
			t.Fatalf("existing guard did not read updated mode=%s: blocked=%v", mode, blocked)
		}
	}
	payload := callbackPayload(t, request(http.MethodPut, `{}`, true))
	if payload["roe_enforcement"] != "strict" {
		t.Fatal("omitted policy field reset a saved mode")
	}
}

func TestRoESettingsInvalidUpdatePrecedesAllOtherMutation(t *testing.T) {
	s, request := roeSettingsFixture(t)
	if err := s.m.pg.SetSetting(settingRoEEnforcement, "strict"); err != nil {
		t.Fatal(err)
	}
	workers := s.m.Workers()
	for _, raw := range []string{`null`, `""`, `"WARN"`, `"unknown"`, `" strict "`, `true`, `9`, `[]`, `{}`} {
		t.Run(raw, func(t *testing.T) {
			body := `{"roe_enforcement":` + raw + `,"callback_addr":"must-not-save.fixture:443","workers":9}`
			response := request(http.MethodPut, body, true)
			if response.Code != http.StatusBadRequest {
				t.Fatalf("invalid policy accepted: %d %s", response.Code, response.Body)
			}
			if value, _, err := s.m.pg.GetSetting(settingRoEEnforcement); err != nil || value != "strict" {
				t.Fatalf("invalid policy changed mode=%q err=%v", value, err)
			}
			if s.m.Workers() != workers {
				t.Fatal("invalid policy changed unrelated workers")
			}
			if _, found, err := s.m.pg.GetSetting(config.CallbackAddrSetting); err != nil || found {
				t.Fatalf("invalid policy changed unrelated callback: %v %v", found, err)
			}
		})
	}
	for _, body := range []string{
		`{"roe_enforcement":"off","workers":0}`,
		`{"roe_enforcement":"warn","callback_addr":"https://invalid.fixture"}`,
	} {
		if response := request(http.MethodPut, body, true); response.Code != http.StatusBadRequest {
			t.Fatalf("invalid mixed settings request accepted: %d %s", response.Code, response.Body)
		}
		if value, _, _ := s.m.pg.GetSetting(settingRoEEnforcement); value != "strict" {
			t.Fatal("failed mixed settings request partially changed policy")
		}
	}
}

func TestRoESettingsUnknownSavedValueIsVisibleAndExplicitlyRepairable(t *testing.T) {
	s, request := roeSettingsFixture(t)
	if err := s.m.pg.SetSetting(settingRoEEnforcement, "broken-fixture-value"); err != nil {
		t.Fatal(err)
	}
	response := request(http.MethodGet, "", true)
	if response.Code != http.StatusInternalServerError || strings.Contains(response.Body.String(), `"roe_enforcement":"warn"`) {
		t.Fatalf("unknown saved mode became a valid warn snapshot: %d %s", response.Code, response.Body)
	}
	if value, _, _ := s.m.pg.GetSetting(settingRoEEnforcement); value != "broken-fixture-value" {
		t.Fatal("read error changed saved policy")
	}
	payload := callbackPayload(t, request(http.MethodPut, `{"roe_enforcement":"strict"}`, true))
	if payload["roe_enforcement"] != "strict" {
		t.Fatal("explicit valid policy repair failed")
	}
}

func TestRoESettingsDependencyFailureDoesNotPartiallyChangePolicy(t *testing.T) {
	s, request := roeSettingsFixture(t)
	beforePools, found, err := s.m.pg.GetSetting(settingWorkerPools)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if found {
			_ = s.m.pg.SetSetting(settingWorkerPools, beforePools)
		} else {
			_ = s.m.pg.DeleteSetting(settingWorkerPools)
		}
	})
	if err := s.m.pg.SetSetting(settingRoEEnforcement, "strict"); err != nil {
		t.Fatal(err)
	}
	if err := s.m.pg.SetSetting(settingWorkerPools, `{"invalid":"fixture"}`); err != nil {
		t.Fatal(err)
	}
	response := request(http.MethodPut, `{"roe_enforcement":"off"}`, true)
	if response.Code != http.StatusInternalServerError {
		t.Fatalf("unreadable full settings snapshot accepted: %d", response.Code)
	}
	if value, _, _ := s.m.pg.GetSetting(settingRoEEnforcement); value != "strict" {
		t.Fatal("full response failure applied an unconfirmed mode")
	}
}

func TestRoESettingsClosedStoreDoesNotInventDefault(t *testing.T) {
	closed, err := sql.Open("pgx", "postgres://roe-fixture.invalid/unused")
	if err != nil {
		t.Fatal(err)
	}
	_ = closed.Close()
	pg := &db.DB{DB: closed}
	if value, err := readRoEEnforcementSetting(pg); err == nil || value != "" {
		t.Fatalf("closed store returned policy=%q err=%v", value, err)
	}
	if got := newRoEConfig(pg).Mode(); got != "strict" {
		t.Fatalf("existing runtime storage-failure behavior changed: %s", got)
	}
	if value, err := readRoEEnforcementSetting(nil); err == nil || value != "" {
		t.Fatal("unavailable settings invented a mode")
	}
}

func TestRoESettingsUnknownRuntimeValueDoesNotDowngradeToWarn(t *testing.T) {
	s, _ := roeSettingsFixture(t)
	for _, invalid := range []string{"", "invalid", "WARN", " strict "} {
		if err := s.m.pg.SetSetting(settingRoEEnforcement, invalid); err != nil {
			t.Fatal(err)
		}
		cfg := newRoEConfig(s.m.pg)
		if mode := cfg.Mode(); mode != "strict" {
			t.Fatalf("unknown saved policy downgraded to %s", mode)
		}
		cfg.TaskIDOf = func(context.Context) int64 { return 1 }
		cfg.ScopeWithError = func(int64) ([]guard.ScopeRule, error) {
			return []guard.ScopeRule{{Kind: "domain", Value: "allowed.fixture"}}, nil
		}
		g := guard.New()
		g.SetRoE(cfg)
		blocked, _, _ := g.Hooks().PreToolUse(t.Context(), "Bash", json.RawMessage(`{"command":"curl https://outside.fixture/"}`))
		if !blocked {
			t.Fatal("unknown policy was not enforced as strict before tool execution")
		}
		if saved, _, _ := s.m.pg.GetSetting(settingRoEEnforcement); saved != invalid {
			t.Fatal("runtime error handling rewrote the administrator's saved value")
		}
	}
}
