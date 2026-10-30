package server

import (
	"database/sql"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Autumn-27/artex/config"
	"github.com/Autumn-27/artex/db"
)

func controlsWiringFixture(t *testing.T) (*Server, func(string, string, bool) *httptest.ResponseRecorder) {
	t.Helper()
	s, request := roeSettingsFixture(t)
	for _, key := range []string{settingFindingVerifyMode, settingLLMRecordsRetentionDays} {
		before, found, err := s.m.pg.GetSetting(key)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			if found {
				_ = s.m.pg.SetSetting(key, before)
			} else {
				_ = s.m.pg.DeleteSetting(key)
			}
		})
		if err := s.m.pg.DeleteSetting(key); err != nil {
			t.Fatal(err)
		}
	}
	return s, request
}

func TestSettingsControlsDefaultsAreReadOnlyAndOmittedUpdatesPreserve(t *testing.T) {
	s, request := controlsWiringFixture(t)
	payload := callbackPayload(t, request(http.MethodGet, "", true))
	if payload["roe_enforcement"] != "warn" || payload["finding_verify_mode"] != "auto" || payload["llm_records_retention_days"] != float64(30) {
		t.Fatalf("default controls changed: %#v", payload)
	}
	for _, key := range []string{settingRoEEnforcement, settingFindingVerifyMode, settingLLMRecordsRetentionDays} {
		if _, found, err := s.m.pg.GetSetting(key); err != nil || found {
			t.Fatalf("GET persisted default %s: %v %v", key, found, err)
		}
	}
	for _, mode := range []string{"off", "auto"} {
		payload = callbackPayload(t, request(http.MethodPut, `{"finding_verify_mode":"`+mode+`"}`, true))
		if payload["finding_verify_mode"] != mode {
			t.Fatalf("verification mode did not round-trip: %#v", payload["finding_verify_mode"])
		}
	}
	payload = callbackPayload(t, request(http.MethodPut, `{"finding_verify_mode":"off","llm_records_retention_days":0,"roe_enforcement":"strict"}`, true))
	if payload["finding_verify_mode"] != "off" || payload["llm_records_retention_days"] != float64(0) || payload["roe_enforcement"] != "strict" {
		t.Fatal("explicit batch controls were not preserved")
	}
	payload = callbackPayload(t, request(http.MethodPut, `{}`, true))
	if payload["finding_verify_mode"] != "off" || payload["llm_records_retention_days"] != float64(0) || payload["roe_enforcement"] != "strict" {
		t.Fatal("omitted controls reset saved values")
	}
}

func TestSettingsControlsInvalidValuesPrecedeUnrelatedWrites(t *testing.T) {
	s, request := controlsWiringFixture(t)
	workers := s.m.Workers()
	for _, body := range []string{
		`{"finding_verify_mode":null}`, `{"finding_verify_mode":""}`, `{"finding_verify_mode":"AUTO"}`, `{"finding_verify_mode":true}`, `{"finding_verify_mode":"invalid"}`,
		`{"llm_records_retention_days":null}`, `{"llm_records_retention_days":"1"}`, `{"llm_records_retention_days":true}`, `{"llm_records_retention_days":1.5}`,
		`{"llm_records_retention_days":-1}`, `{"llm_records_retention_days":106752}`, `{"llm_records_retention_days":9223372036854775807}`,
	} {
		mixed := strings.TrimSuffix(body, "}") + `,"callback_addr":"not-saved.fixture:443","workers":9}`
		if response := request(http.MethodPut, mixed, true); response.Code != http.StatusBadRequest {
			t.Fatalf("invalid control accepted: %d %s", response.Code, response.Body)
		}
		if s.m.Workers() != workers {
			t.Fatal("invalid control changed workers")
		}
		if _, found, err := s.m.pg.GetSetting(config.CallbackAddrSetting); err != nil || found {
			t.Fatal("invalid control changed callback settings")
		}
		for _, key := range []string{settingFindingVerifyMode, settingLLMRecordsRetentionDays} {
			if _, found, err := s.m.pg.GetSetting(key); err != nil || found {
				t.Fatalf("invalid update persisted %s", key)
			}
		}
	}
}

func TestSettingsControlsUnknownValuesRequireExplicitRepair(t *testing.T) {
	s, request := controlsWiringFixture(t)
	for key, value := range map[string]string{settingRoEEnforcement: "bad-mode", settingFindingVerifyMode: "bad-mode", settingLLMRecordsRetentionDays: "bad-days"} {
		if err := s.m.pg.SetSetting(key, value); err != nil {
			t.Fatal(err)
		}
	}
	if response := request(http.MethodGet, "", true); response.Code != http.StatusInternalServerError {
		t.Fatal("invalid saved controls were rendered as defaults")
	}
	if response := request(http.MethodPut, `{"roe_enforcement":"warn"}`, true); response.Code != http.StatusInternalServerError {
		t.Fatal("unknown peer values were silently guessed during a partial repair")
	}
	if value, _, _ := s.m.pg.GetSetting(settingRoEEnforcement); value != "bad-mode" {
		t.Fatal("failed candidate settings partially changed a mode")
	}
	payload := callbackPayload(t, request(http.MethodPut, `{"roe_enforcement":"strict","finding_verify_mode":"off","llm_records_retention_days":0}`, true))
	if payload["roe_enforcement"] != "strict" || payload["finding_verify_mode"] != "off" || payload["llm_records_retention_days"] != float64(0) {
		t.Fatal("explicit valid repair did not replace damaged controls")
	}
}

func TestSettingsControlsBatchFailureRollsBackAllThreePolicies(t *testing.T) {
	s, request := controlsWiringFixture(t)
	before := map[string]string{settingRoEEnforcement: "strict", settingFindingVerifyMode: "auto", settingLLMRecordsRetentionDays: "0"}
	for key, value := range before {
		if err := s.m.pg.SetSetting(key, value); err != nil {
			t.Fatal(err)
		}
	}
	constraint := fmt.Sprintf("settings_control_fixture_%d", time.Now().UnixNano())
	if _, err := s.m.pg.Exec(`ALTER TABLE settings ADD CONSTRAINT ` + constraint + ` CHECK(key <> 'llm_records_retention_days' OR value <> '1')`); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = s.m.pg.Exec(`ALTER TABLE settings DROP CONSTRAINT IF EXISTS ` + constraint) })
	response := request(http.MethodPut, `{"roe_enforcement":"off","finding_verify_mode":"off","llm_records_retention_days":1}`, true)
	if response.Code != http.StatusInternalServerError {
		t.Fatalf("injected policy save failure accepted: %d", response.Code)
	}
	for key, want := range before {
		if value, _, err := s.m.pg.GetSetting(key); err != nil || value != want {
			t.Fatalf("failed batch partially changed %s=%q, want=%q err=%v", key, value, want, err)
		}
	}
}

func TestSettingsControlsRetentionSaveAndUnknownStateNeverDeleteImmediately(t *testing.T) {
	s, request := controlsWiringFixture(t)
	marker := fmt.Sprintf("retention_wiring_%d", time.Now().UnixNano())
	var recordID, usageID int64
	if err := s.m.pg.QueryRow(`INSERT INTO llm_records(ts,session_id,request_body,status) VALUES(now()-interval '365 days',$1,'synthetic retention record','ok') RETURNING id`, marker).Scan(&recordID); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = s.m.pg.Exec(`DELETE FROM llm_records WHERE id=$1`, recordID) })
	if err := s.m.pg.QueryRow(`INSERT INTO llm_usage(ts,worker,status) VALUES(now()-interval '365 days',$1,'ok') RETURNING id`, marker).Scan(&usageID); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = s.m.pg.Exec(`DELETE FROM llm_usage WHERE id=$1`, usageID) })
	assertRetained := func() {
		t.Helper()
		var records, usages int
		if err := s.m.pg.QueryRow(`SELECT count(*) FROM llm_records WHERE id=$1`, recordID).Scan(&records); err != nil {
			t.Fatal(err)
		}
		if err := s.m.pg.QueryRow(`SELECT count(*) FROM llm_usage WHERE id=$1`, usageID).Scan(&usages); err != nil {
			t.Fatal(err)
		}
		if records != 1 || usages != 1 {
			t.Fatalf("record/usage removed outside a valid scheduled cleanup: %d/%d", records, usages)
		}
	}
	callbackPayload(t, request(http.MethodPut, `{"llm_records_retention_days":1}`, true))
	assertRetained() // saving does not run cleanup
	for _, invalid := range []string{"-1", "not-days", "106752", "9223372036854775807"} {
		if err := s.m.pg.SetSetting(settingLLMRecordsRetentionDays, invalid); err != nil {
			t.Fatal(err)
		}
		if _, err := s.m.llmRecordsRetentionDaysChecked(); err == nil || s.m.llmRecordsRetentionDays() != 0 {
			t.Fatal("invalid retention did not preserve records")
		}
		s.m.cleanupLLMRecordsRetention()
		assertRetained()
	}
	if err := s.m.pg.SetSetting(settingLLMRecordsRetentionDays, "0"); err != nil {
		t.Fatal(err)
	}
	s.m.cleanupLLMRecordsRetention()
	assertRetained()
	if err := s.m.pg.SetSetting(settingLLMRecordsRetentionDays, "106751"); err != nil {
		t.Fatal(err)
	}
	if days, err := s.m.llmRecordsRetentionDaysChecked(); err != nil || days != maxLLMRecordsRetentionDays {
		t.Fatalf("safe duration boundary rejected: %d %v", days, err)
	}
}

func TestSettingsControlsReadFailuresDoNotInventDefaults(t *testing.T) {
	closed, err := sql.Open("pgx", "postgres://control-wiring-fixture.invalid/unused")
	if err != nil {
		t.Fatal(err)
	}
	_ = closed.Close()
	pg := &db.DB{DB: closed}
	if mode, err := readFindingVerifyModeSetting(pg); err == nil || mode != "" {
		t.Fatal("closed store invented auto verification")
	}
	if days, err := readLLMRecordsRetentionSetting(pg); err == nil || days != 0 {
		t.Fatal("closed store invented destructive retention")
	}
	m := &Manager{pg: pg}
	m.cleanupLLMRecordsRetention() // checked failure exits before DELETE
}
