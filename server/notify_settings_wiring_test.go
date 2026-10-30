package server

import (
	"database/sql"
	"testing"

	"github.com/Autumn-27/artex/db"
)

func TestNotifyGlobalSettingsWiringKeepsUnknownDistinctFromDefaults(t *testing.T) {
	pg := openIntranetTestDB(t)
	pg.SetMaxOpenConns(1)
	pg.SetMaxIdleConns(1)
	if _, err := pg.Exec(`CREATE TEMP TABLE settings (LIKE public.settings INCLUDING ALL)`); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = pg.Exec(`DROP TABLE IF EXISTS pg_temp.settings`) })
	value, err := readNotifyGlobalSettings(pg)
	if err != nil || !value.Enabled || value.PublicBaseURL != "" || value.DigestMinutes != notifyDefaultDigestMinutes {
		t.Fatalf("absent setting defaults = %+v err=%v", value, err)
	}
	var rows int
	if err := pg.QueryRow(`SELECT count(*) FROM settings`).Scan(&rows); err != nil || rows != 0 {
		t.Fatalf("read initialized defaults by writing: rows=%d err=%v", rows, err)
	}
	for key, raw := range map[string]string{
		settingNotifyEnabled: "false", settingNotifyPublicBaseURL: "https://notify.fixture.test/artex", settingNotifyDigestMinutes: "30",
	} {
		if err := pg.SetSetting(key, raw); err != nil {
			t.Fatal(err)
		}
	}
	value, err = readNotifyGlobalSettings(pg)
	if err != nil || value.Enabled || value.PublicBaseURL != "https://notify.fixture.test/artex" || value.DigestMinutes != 30 {
		t.Fatalf("stored known values = %+v err=%v", value, err)
	}
	for _, tc := range []struct{ key, raw string }{
		{settingNotifyEnabled, "unknown"},
		{settingNotifyPublicBaseURL, "not-an-absolute-url"},
		{settingNotifyDigestMinutes, "0"},
		{settingNotifyDigestMinutes, "1441"},
		{settingNotifyDigestMinutes, "invalid"},
	} {
		before, _, _ := pg.GetSetting(tc.key)
		if err := pg.SetSetting(tc.key, tc.raw); err != nil {
			t.Fatal(err)
		}
		if _, err := readNotifyGlobalSettings(pg); err == nil {
			t.Errorf("unknown %s became a successful snapshot", tc.key)
		}
		if raw, _, err := pg.GetSetting(tc.key); err != nil || raw != tc.raw {
			t.Errorf("invalid setting was silently replaced: %s err=%v", tc.key, err)
		}
		if err := pg.SetSetting(tc.key, before); err != nil {
			t.Fatal(err)
		}
	}
}

func TestNotifyGlobalSettingsWiringFailsOnUnavailableStore(t *testing.T) {
	if _, err := readNotifyGlobalSettings(nil); err == nil {
		t.Fatal("missing database became successful defaults")
	}
	closed, err := sql.Open("pgx", "postgres://notify-fixture.invalid/unused")
	if err != nil {
		t.Fatal(err)
	}
	if err := closed.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := readNotifyGlobalSettings(&db.DB{DB: closed}); err == nil {
		t.Fatal("database failure became successful default-on settings")
	}
}
