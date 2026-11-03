package server

import (
	"database/sql"
	"net/http"
	"testing"

	"github.com/Autumn-27/artex/db"
	"github.com/Autumn-27/artex/guard"
)

func TestDeepSeekSettingsRefreshEgressFingerprintsWithoutActiveModel(t *testing.T) {
	s, request, _ := callbackSettingsFixture(t)
	prior, existed, err := s.m.pg.GetSetting(settingDeepSeekSearchKey)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if existed {
			_ = s.m.pg.SetSetting(settingDeepSeekSearchKey, prior)
		} else {
			_ = s.m.pg.DeleteSetting(settingDeepSeekSearchKey)
		}
	})
	const oldKey = "fixture-deepseek-old-key-123456"
	const newKey = "fixture-deepseek-new-key-654321"
	for _, key := range []string{oldKey, newKey, ""} {
		body := `{"deepseek_search_api_key":"` + key + `"}`
		if response := request(http.MethodPut, body, true); response.Code != http.StatusOK {
			t.Fatalf("search key update HTTP %d", response.Code)
		}
		if key != "" {
			if kind, hit := s.m.egress.Check("query contains " + key); !hit || kind != "search_api_key" {
				t.Fatalf("new search key not protected: kind=%q hit=%v", kind, hit)
			}
		}
		if key != oldKey {
			if _, hit := s.m.egress.Check("query contains " + oldKey); hit {
				t.Fatal("obsolete search key fingerprint was not cleared")
			}
		}
		if key == "" {
			if _, hit := s.m.egress.Check("query contains " + newKey); hit {
				t.Fatal("explicit search key clear kept old fingerprint")
			}
		}
	}
}

func TestDeepSeekEgressReadFailureRetainsLastKnownProtection(t *testing.T) {
	closed, err := sql.Open("pgx", "postgres://fixture.invalid/unused")
	if err != nil {
		t.Fatal(err)
	}
	if err := closed.Close(); err != nil {
		t.Fatal(err)
	}
	eg := guard.NewEgressGuard()
	const secret = "fixture-deepseek-protected-key-123456"
	eg.AddFingerprint("search_api_key", secret)
	s := &Server{m: &Manager{pg: &db.DB{DB: closed}, egress: eg}}
	s.registerEgressFingerprints()
	if kind, hit := eg.Check(secret); !hit || kind != "search_api_key" {
		t.Fatal("failed search settings read discarded known secret protection")
	}
}
