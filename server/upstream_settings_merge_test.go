package server

import (
	"database/sql"
	"net/http"
	"reflect"
	"testing"

	"github.com/Autumn-27/artex/config"
	"github.com/Autumn-27/artex/db"
)

func mergeSettingsBrowserFixture(t *testing.T, s *Server) *db.MCPServer {
	t.Helper()
	servers, err := s.m.pg.ListMCP()
	if err != nil {
		t.Fatal(err)
	}
	for _, server := range servers {
		if server.Name == browserMCPName {
			original := *server
			t.Cleanup(func() { _, _ = s.m.pg.SaveMCP(&original) })
			return server
		}
	}
	t.Fatal("browser MCP fixture missing")
	return nil
}

func TestMergeScreenshotOptInDoesNotEnableDisabledMCPOrMutateOtherSettings(t *testing.T) {
	s, request, _ := callbackSettingsFixture(t)
	browser := mergeSettingsBrowserFixture(t, s)
	browser.Enabled = false
	browser.Args = encodeJSON([]string{"--user-data-dir", "/fixture/profile", "--custom-fixture"})
	if _, err := s.m.pg.SaveMCP(browser); err != nil {
		t.Fatal(err)
	}
	beforeWorkers := s.m.Workers()
	response := request(http.MethodPut, `{"browser_screenshot_enabled":true,"callback_addr":"not-saved.fixture:443","workers":9}`, true)
	if response.Code != http.StatusConflict {
		t.Fatalf("disabled MCP opt-in status=%d: %s", response.Code, response.Body)
	}
	if s.m.Workers() != beforeWorkers {
		t.Fatal("failed screenshot opt-in changed worker settings")
	}
	if _, found, err := s.m.pg.GetSetting(config.CallbackAddrSetting); err != nil || found {
		t.Fatalf("failed screenshot opt-in wrote callback setting: found=%v err=%v", found, err)
	}
	servers, _ := s.m.pg.ListMCP()
	for _, after := range servers {
		if after.ID == browser.ID && (after.Enabled || !reflect.DeepEqual(decodeStrSlice(after.Args), decodeStrSlice(browser.Args))) {
			t.Fatalf("screenshot opt-in rewrote administrator MCP settings: enabled=%v args=%s", after.Enabled, after.Args)
		}
	}
}

func TestMergeScreenshotOptInOnlyChangesReporterGrant(t *testing.T) {
	s, _, _ := callbackSettingsFixture(t)
	browser := mergeSettingsBrowserFixture(t, s)
	browser.Enabled = true
	browser.Args = encodeJSON([]string{"--user-data-dir=/fixture/profile", "--custom-fixture"})
	if _, err := s.m.pg.SaveMCP(browser); err != nil {
		t.Fatal(err)
	}
	reporter, err := s.m.pg.GetAgentByKey("reporter")
	if err != nil || reporter == nil {
		t.Fatalf("reporter fixture missing: %v", err)
	}
	var prior bool
	priorErr := s.m.pg.QueryRow(`SELECT enabled FROM agent_visibility WHERE agent_id=$1 AND resource_kind='mcp' AND resource_id=$2 AND mcp_tool_name=''`, reporter.ID, browser.ID).Scan(&prior)
	if priorErr != nil && priorErr != sql.ErrNoRows {
		t.Fatal(priorErr)
	}
	t.Cleanup(func() {
		if priorErr == sql.ErrNoRows {
			_, _ = s.m.pg.Exec(`DELETE FROM agent_visibility WHERE agent_id=$1 AND resource_kind='mcp' AND resource_id=$2 AND mcp_tool_name=''`, reporter.ID, browser.ID)
		} else {
			_, _ = s.m.pg.Exec(`INSERT INTO agent_visibility(agent_id,resource_kind,resource_id,mcp_tool_name,enabled) VALUES($1,'mcp',$2,'',$3) ON CONFLICT(agent_id,resource_kind,resource_id,mcp_tool_name) DO UPDATE SET enabled=EXCLUDED.enabled`, reporter.ID, browser.ID, prior)
		}
	})
	otherGrants := func() string {
		var fingerprint string
		if err := s.m.pg.QueryRow(`SELECT md5(COALESCE(string_agg(row_to_json(v)::text,'|' ORDER BY agent_id,mcp_tool_name),'')) FROM agent_visibility v WHERE resource_kind='mcp' AND resource_id=$1 AND agent_id<>$2`, browser.ID, reporter.ID).Scan(&fingerprint); err != nil {
			t.Fatal(err)
		}
		return fingerprint
	}
	beforeOther := otherGrants()
	for _, enabled := range []bool{true, false} {
		if err := s.setReporterBrowserScreenshotVisibility(enabled); err != nil {
			t.Fatal(err)
		}
		var granted bool
		if err := s.m.pg.QueryRow(`SELECT EXISTS(SELECT 1 FROM agent_visibility WHERE agent_id=$1 AND resource_kind='mcp' AND resource_id=$2 AND mcp_tool_name='' AND enabled)`, reporter.ID, browser.ID).Scan(&granted); err != nil || granted != enabled {
			t.Fatalf("reporter grant=%v err=%v want=%v", granted, err, enabled)
		}
		if otherGrants() != beforeOther {
			t.Fatal("screenshot setting changed another Agent's explicit permissions")
		}
	}
	servers, _ := s.m.pg.ListMCP()
	for _, after := range servers {
		if after.ID == browser.ID && (!after.Enabled || !reflect.DeepEqual(decodeStrSlice(after.Args), decodeStrSlice(browser.Args))) {
			t.Fatalf("reporter grant toggle changed MCP configuration: enabled=%v args=%s", after.Enabled, after.Args)
		}
	}
	for _, key := range []string{"worker_pools", "opsec_enabled", "show_egress_ip", db.SettingFindingInlineMerge, db.SettingCredentialAgent} {
		if _, exists := s.settingsPayload()[key]; !exists {
			t.Errorf("merged settings payload lost %s", key)
		}
	}
}

func TestMergeUnknownWorkerPoolsNeverBecomeEmptySettings(t *testing.T) {
	s, request, _ := callbackSettingsFixture(t)
	prior, existed, err := s.m.pg.GetSetting(settingWorkerPools)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if existed {
			_ = s.m.pg.SetSetting(settingWorkerPools, prior)
		} else {
			_ = s.m.pg.DeleteSetting(settingWorkerPools)
		}
	})
	beforeWorkers := s.m.Workers()
	const invalid = `{"unexpected":"not-a-worker-pool"}`
	if err := s.m.pg.SetSetting(settingWorkerPools, invalid); err != nil {
		t.Fatal(err)
	}
	if response := request(http.MethodGet, "", true); response.Code != http.StatusInternalServerError {
		t.Fatalf("unknown pools returned a successful empty snapshot: %d %s", response.Code, response.Body)
	}
	response := request(http.MethodPut, `{"workers":9,"callback_addr":"not-saved.fixture:443"}`, true)
	if response.Code != http.StatusInternalServerError || s.m.Workers() != beforeWorkers {
		t.Fatalf("unknown pools changed legacy concurrency: %d workers=%d", response.Code, s.m.Workers())
	}
	if _, found, err := s.m.pg.GetSetting(config.CallbackAddrSetting); err != nil || found {
		t.Fatalf("unknown pools allowed unrelated preflight mutation: found=%v err=%v", found, err)
	}
	if raw, _, err := s.m.pg.GetSetting(settingWorkerPools); err != nil || raw != invalid {
		t.Fatalf("unknown pool value was erased: %q err=%v", raw, err)
	}
	// A deliberate replacement is distinct from an omitted/unknown pool value.
	repaired := callbackPayload(t, request(http.MethodPut, `{"worker_pools":[]}`, true))
	if pools, ok := repaired["worker_pools"].([]any); !ok || len(pools) != 0 {
		t.Fatalf("explicit pool repair did not persist: %#v", repaired["worker_pools"])
	}
}
