package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func TestSkillLibraryPresenceRejectsEscapesAndRequiresRegularFile(t *testing.T) {
	root := t.TempDir()
	file := filepath.Join(root, "present", "SKILL.md")
	if err := os.MkdirAll(filepath.Dir(file), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(file, []byte("fixture"), 0o644); err != nil {
		t.Fatal(err)
	}
	if !skillLibraryPresent(root, "present") || skillLibraryPresent(root, "absent") || skillLibraryPresent(root, "../present") || skillLibraryPresent(root, "/present") {
		t.Fatal("invalid library presence result")
	}
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "SKILL.md"), []byte("outside fixture"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "escape")); err != nil {
		t.Fatal(err)
	}
	if skillLibraryPresent(root, "escape") {
		t.Fatal("followed a skill symlink outside the library")
	}
	if err := os.MkdirAll(filepath.Join(root, "directory", "SKILL.md"), 0o755); err != nil {
		t.Fatal(err)
	}
	if skillLibraryPresent(root, "directory") {
		t.Fatal("directory named SKILL.md counted as an installed skill")
	}
}

func TestMissingSkillHTTPAcknowledgesWithoutDeletingLedger(t *testing.T) {
	s, _, _ := callbackSettingsFixture(t)
	name := fmt.Sprintf("missing-http-%d", s.m.pg.Stats().OpenConnections)
	const marker = "skill_missing_notices_cleared_through_v1"
	previous, existed, err := s.m.pg.GetSetting(marker)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.m.pg.DeleteSetting(marker); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = s.m.pg.Exec(`DELETE FROM skill_usage WHERE skill=$1`, name)
		if existed {
			_ = s.m.pg.SetSetting(marker, previous)
		} else {
			_ = s.m.pg.DeleteSetting(marker)
		}
	})
	if _, err := s.m.pg.Exec(`INSERT INTO skill_usage(skill,agent_key,found) VALUES($1,'auto',false)`, name); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(s.skillDir, name, "SKILL.md")
	if err := os.MkdirAll(filepath.Dir(file), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(file, []byte("---\nname: "+name+"\ndescription: fixture\n---\nfixture"), 0o644); err != nil {
		t.Fatal(err)
	}
	token, err := signJWT(s.jwtKey)
	if err != nil {
		t.Fatal(err)
	}
	request := func(method string, authorized bool) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, "/api/skills/missing?limit=200", nil)
		if authorized {
			r.Header.Set("Authorization", "Bearer "+token)
		}
		response := httptest.NewRecorder()
		s.Handler().ServeHTTP(response, r)
		return response
	}
	for _, method := range []string{http.MethodGet, http.MethodDelete} {
		if response := request(method, false); response.Code != http.StatusUnauthorized {
			t.Fatalf("unauthenticated %s status=%d", method, response.Code)
		}
	}
	readCalls := func() int {
		t.Helper()
		response := request(http.MethodGet, true)
		if response.Code != http.StatusOK {
			t.Fatalf("missing skills HTTP %d: %s", response.Code, response.Body)
		}
		var payload struct {
			Missing []struct {
				Skill          string `json:"skill"`
				Calls          int    `json:"calls"`
				LibraryPresent bool   `json:"library_present"`
			} `json:"missing"`
		}
		if err := json.Unmarshal(response.Body.Bytes(), &payload); err != nil {
			t.Fatal(err)
		}
		for _, item := range payload.Missing {
			if item.Skill == name {
				if !item.LibraryPresent {
					t.Fatal("existing file was described as missing")
				}
				return item.Calls
			}
		}
		return 0
	}
	if readCalls() != 1 {
		t.Fatal("initial unresolved event was absent")
	}
	if response := request(http.MethodDelete, true); response.Code != http.StatusOK {
		t.Fatalf("acknowledge HTTP %d: %s", response.Code, response.Body)
	}
	if readCalls() != 0 {
		t.Fatal("acknowledged event remained visible")
	}
	var count int
	if err := s.m.pg.QueryRow(`SELECT count(*) FROM skill_usage WHERE skill=$1`, name).Scan(&count); err != nil || count != 1 {
		t.Fatalf("acknowledgement deleted the audit row: %d %v", count, err)
	}
	if _, err := s.m.pg.Exec(`INSERT INTO skill_usage(skill,agent_key,found,ts) VALUES($1,'auto',false,clock_timestamp())`, name); err != nil {
		t.Fatal(err)
	}
	if readCalls() != 1 {
		t.Fatal("new unresolved event was hidden by old acknowledgement")
	}
}
