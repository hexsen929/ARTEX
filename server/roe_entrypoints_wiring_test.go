package server

import (
	"strings"
	"testing"

	"github.com/Autumn-27/artex/agent"
)

func TestRoEWiringUnknownModeDoesNotAllowSessionOrTunnelRegistration(t *testing.T) {
	pg := openIntranetTestDB(t)
	pg.SetMaxOpenConns(1)
	pg.SetMaxIdleConns(1)
	if _, err := pg.Exec(`CREATE TEMP TABLE settings (LIKE public.settings INCLUDING ALL)`); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = pg.Exec(`DROP TABLE IF EXISTS pg_temp.settings`) })
	if err := pg.SetSetting(settingRoEEnforcement, "unknown-policy"); err != nil {
		t.Fatal(err)
	}
	s := &Server{m: &Manager{pg: pg}}
	ctx := agent.WithRunInfo(t.Context(), agent.RunInfo{TaskID: 42})
	// These are parser/scope checks only: no session, tunnel, listener, process
	// or network request is created for the synthetic host.
	if warning, err := s.sessionRoECheck(ctx, "https://scope-fixture.invalid"); err == nil || warning != "" || !strings.Contains(err.Error(), "范围策略") {
		t.Fatalf("unknown session policy became warn/allow: warning=%q err=%v", warning, err)
	}
	if warning, err := s.roeCheckHost(ctx, "scope-fixture.invalid"); err == nil || warning != "" || !strings.Contains(err.Error(), "范围策略") {
		t.Fatalf("unknown tunnel policy became warn/allow: warning=%q err=%v", warning, err)
	}
	if raw, _, err := pg.GetSetting(settingRoEEnforcement); err != nil || raw != "unknown-policy" {
		t.Fatalf("scope read rewrote stored configuration: %q err=%v", raw, err)
	}
}
