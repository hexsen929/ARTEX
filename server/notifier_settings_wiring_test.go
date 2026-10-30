package server

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Autumn-27/artex/db"
	"github.com/Autumn-27/artex/notify"
	"github.com/jackc/pgx/v5"
)

// Global Notifier.step must never be exercised against a developer's existing
// notification channels. These cloud-only fixtures use a newly created local
// database, verify its identity before schema/data writes, and only fake hooks.
func notifierWiringDB(t *testing.T) *db.DB {
	t.Helper()
	if os.Getenv("CI") == "" {
		t.Skip("global notifier wiring regressions require an isolated CI database")
	}
	dsn := os.Getenv("ARTEX_PG_DSN")
	u, err := url.Parse(dsn)
	if err != nil || u.Scheme != "postgres" && u.Scheme != "postgresql" {
		t.Fatal("notifier wiring fixture requires a PostgreSQL URL supplied by CI")
	}
	host := u.Hostname()
	ip := net.ParseIP(host)
	if host != "localhost" && (ip == nil || !ip.IsLoopback()) {
		t.Fatal("notifier wiring fixture only accepts the local CI PostgreSQL service")
	}
	admin, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	name := fmt.Sprintf("artex_notify_wiring_%d", time.Now().UnixNano())
	identifier := pgx.Identifier{name}.Sanitize()
	if _, err := admin.ExecContext(t.Context(), `CREATE DATABASE `+identifier); err != nil {
		admin.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if _, err := admin.ExecContext(context.Background(), `DROP DATABASE `+identifier+` WITH (FORCE)`); err != nil {
			t.Errorf("drop isolated notifier fixture: %v", err)
		}
		admin.Close()
	})
	u.Path, u.RawPath = "/"+name, ""
	query := u.Query()
	query.Del("dbname")
	u.RawQuery = query.Encode()
	fixtureDSN := u.String()
	probe, err := sql.Open("pgx", fixtureDSN)
	if err != nil {
		t.Fatal(err)
	}
	var connected string
	err = probe.QueryRowContext(t.Context(), `SELECT current_database()`).Scan(&connected)
	probe.Close()
	if err != nil || connected != name {
		t.Fatalf("notifier fixture did not select its isolated database: %v", err)
	}
	pg, err := db.Open(fixtureDSN)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { pg.Close() })
	return pg
}

type notifierWiringFixture struct {
	pg             *db.DB
	notifier       *Notifier
	realtime       *fakeWebhook
	digest         *fakeWebhook
	realtimeID     int64
	digestID       int64
	pendingEventID int64
}

func newNotifierWiringFixture(t *testing.T) *notifierWiringFixture {
	t.Helper()
	t.Setenv(notify.AllowLocalTargetsEnv, "1")
	pg := notifierWiringDB(t)
	pg.SetMaxOpenConns(1)
	pg.SetMaxIdleConns(1)
	fixture := &notifierWiringFixture{
		pg: pg, notifier: &Notifier{pg: pg, buckets: map[int64]*notifyBucket{}},
		realtime: newFakeWebhook(t), digest: newFakeWebhook(t),
	}
	for _, entry := range []struct {
		mode, address string
		id            *int64
	}{
		{db.NotifyModeRealtime, fixture.realtime.URL, &fixture.realtimeID},
		{db.NotifyModeDigest, fixture.digest.URL, &fixture.digestID},
	} {
		config, _ := json.Marshal(map[string]string{"webhook": entry.address})
		id, err := pg.SaveNotificationChannel(t.Context(), &db.NotificationChannel{
			Name: "isolated wiring " + entry.mode, Kind: notify.KindDingTalk,
			Mode: entry.mode, Config: config, RatePerMin: 0,
		})
		if err != nil {
			t.Fatal(err)
		}
		*entry.id = id
	}
	fixture.addEvent(t, 101)
	if events, deliveries, err := pg.FanOutPendingEvents(t.Context(), 10); err != nil || events != 1 || deliveries != 2 {
		t.Fatalf("fixture fanout=%d/%d err=%v", events, deliveries, err)
	}
	if _, err := pg.Exec(`UPDATE notification_deliveries SET created_at=now()-interval '2 hours'`); err != nil {
		t.Fatal(err)
	}
	fixture.pendingEventID = fixture.addEvent(t, 102)
	return fixture
}

func (f *notifierWiringFixture) addEvent(t *testing.T, id int64) int64 {
	t.Helper()
	event, err := f.pg.AddNotificationEvent(t.Context(), notify.EventFindingCreated, id, notify.Snapshot{
		Kind: notify.EventFindingCreated, FindingID: id, Name: "SYNTHETIC_WIRING_ONLY",
		VulnClass: "Fixture", Severity: "high", Summary: "Synthetic notification; no real target or credential",
	})
	if err != nil {
		t.Fatal(err)
	}
	return event
}

func (f *notifierWiringFixture) settings(t *testing.T, values map[string]string) {
	t.Helper()
	if _, err := f.pg.Exec(`DELETE FROM settings WHERE key IN($1,$2,$3)`, settingNotifyEnabled, settingNotifyPublicBaseURL, settingNotifyDigestMinutes); err != nil {
		t.Fatal(err)
	}
	if err := f.pg.SetSettingsAtomic(t.Context(), values); err != nil {
		t.Fatal(err)
	}
}

func (f *notifierWiringFixture) fingerprint(t *testing.T) string {
	t.Helper()
	var result strings.Builder
	for _, table := range []string{"notification_events", "notification_deliveries", "notification_channels"} {
		var count int64
		var hash string
		if err := f.pg.QueryRow(`SELECT count(*),md5(COALESCE(string_agg(md5(to_jsonb(t)::text),'|' ORDER BY md5(to_jsonb(t)::text)),'')) FROM `+table+` t`).Scan(&count, &hash); err != nil {
			t.Fatal(err)
		}
		fmt.Fprintf(&result, "%s:%d:%s;", table, count, hash)
	}
	return result.String()
}

func (f *notifierWiringFixture) assertNoDispatch(t *testing.T, before string) {
	t.Helper()
	if after := f.fingerprint(t); after != before {
		t.Fatal("unknown/disabled settings mutated fan-out, delivery leases or channel state")
	}
	if f.realtime.count() != 0 || f.digest.count() != 0 || len(f.notifier.buckets) != 0 {
		t.Fatal("unknown/disabled settings reached send or consumed dispatch quota")
	}
}

func TestNotifierSettingsWiringUnknownAndOffAdmitNoWork(t *testing.T) {
	f := newNotifierWiringFixture(t)
	for _, tc := range []struct{ key, value string }{
		{settingNotifyEnabled, "false"},
		{settingNotifyEnabled, "unknown"},
		{settingNotifyPublicBaseURL, "not-an-http-url"},
		{settingNotifyDigestMinutes, "0"},
		{settingNotifyDigestMinutes, "1441"},
		{settingNotifyDigestMinutes, "not-a-number"},
	} {
		t.Run(tc.key+"/"+tc.value, func(t *testing.T) {
			f.settings(t, map[string]string{tc.key: tc.value})
			before := f.fingerprint(t)
			f.notifier.step(t.Context())
			f.assertNoDispatch(t, before)
			if raw, _, err := f.pg.GetSetting(tc.key); err != nil || raw != tc.value {
				t.Fatal("notifier silently repaired the setting instead of refusing admission")
			}
		})
	}
}

func TestNotifierSettingsWiringReadFailureCannotBecomeDefaultOn(t *testing.T) {
	f := newNotifierWiringFixture(t)
	f.settings(t, nil)
	before := f.fingerprint(t)
	// Only settings reads fail. The same connection still has working event,
	// channel and delivery tables, so the old GetBool(default=true) path would
	// fan out, claim leases and send to the fake hooks despite the read failure.
	if _, err := f.pg.Exec(`CREATE TEMP TABLE settings(key TEXT PRIMARY KEY)`); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = f.pg.Exec(`DROP TABLE IF EXISTS pg_temp.settings`) })
	f.notifier.step(t.Context())
	f.assertNoDispatch(t, before)
	if _, err := f.pg.Exec(`DROP TABLE pg_temp.settings`); err != nil {
		t.Fatal(err)
	}
	f.notifier.step(t.Context())
	if f.realtime.count() != 2 || f.digest.count() != 1 {
		t.Fatalf("ready queued work did not recover after settings became known: %d/%d", f.realtime.count(), f.digest.count())
	}
}

func TestNotifierSettingsWiringAbsentDefaultsRemainReadOnlyAndDeliver(t *testing.T) {
	f := newNotifierWiringFixture(t)
	f.settings(t, nil)
	f.notifier.step(t.Context())
	if f.realtime.count() != 2 || f.digest.count() != 1 {
		t.Fatalf("absent settings changed existing enabled/30m defaults: %d/%d", f.realtime.count(), f.digest.count())
	}
	if body := markdownText(t, f.digest.last(t)); !strings.Contains(body, "30") {
		t.Fatalf("default digest interval was not used: %s", body)
	}
	var pending, sent, attempts int
	if err := f.pg.QueryRow(`SELECT count(*) FILTER(WHERE state='pending'),count(*) FILTER(WHERE state='sent'),sum(attempts) FROM notification_deliveries`).Scan(&pending, &sent, &attempts); err != nil || pending != 0 || sent != 4 || attempts != 4 {
		t.Fatalf("delivery transitions changed: pending=%d sent=%d attempts=%d err=%v", pending, sent, attempts, err)
	}
	var settings int
	if err := f.pg.QueryRow(`SELECT count(*) FROM settings WHERE key IN($1,$2,$3)`, settingNotifyEnabled, settingNotifyPublicBaseURL, settingNotifyDigestMinutes).Scan(&settings); err != nil || settings != 0 {
		t.Fatal("notifier persisted absent defaults")
	}
}

func TestNotifierSettingsWiringUsesOneAdmittedWindowAndRoute(t *testing.T) {
	f := newNotifierWiringFixture(t)
	f.settings(t, map[string]string{
		settingNotifyEnabled: "true", settingNotifyPublicBaseURL: "https://panel.fixture.test/artex/", settingNotifyDigestMinutes: "15",
	})
	var realtimeSends atomic.Int64
	updates := make(chan error, 1)
	hook := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		if realtimeSends.Add(1) == 1 {
			updates <- f.pg.SetSettingsAtomic(r.Context(), map[string]string{
				settingNotifyPublicBaseURL: "not-a-url", settingNotifyDigestMinutes: "1440",
			})
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"errcode":0,"errmsg":"ok"}`)
	}))
	t.Cleanup(hook.Close)
	channel, err := f.pg.NotificationChannelByID(t.Context(), f.realtimeID)
	if err != nil {
		t.Fatal(err)
	}
	channel.Config, _ = json.Marshal(map[string]string{"webhook": hook.URL})
	if _, err := f.pg.SaveNotificationChannel(t.Context(), channel); err != nil {
		t.Fatal(err)
	}
	f.notifier.step(t.Context())
	select {
	case err := <-updates:
		if err != nil {
			t.Fatal(err)
		}
	default:
		t.Fatal("checked tick did not reach its synthetic realtime recipient")
	}
	if realtimeSends.Load() != 2 || f.digest.count() != 1 {
		t.Fatal("an admitted tick reread the changed window or cancelled its existing batch")
	}
	text := markdownText(t, f.digest.last(t))
	if !strings.Contains(text, "15") || !strings.Contains(text, "https://panel.fixture.test/artex") || strings.Contains(text, "not-a-url") {
		t.Fatalf("digest did not retain the checked tick snapshot: %s", text)
	}
	// The next tick must read again, not retain the earlier valid admission.
	f.addEvent(t, 103)
	before := f.fingerprint(t)
	f.notifier.step(t.Context())
	if f.fingerprint(t) != before || realtimeSends.Load() != 2 || f.digest.count() != 1 {
		t.Fatal("next tick reused stale admission after settings became invalid")
	}
}

func TestNotifierSettingsWiringUnavailableStoreAdmitsNoWork(t *testing.T) {
	closed, err := sql.Open("pgx", "postgres://notifier-fixture.invalid/unused")
	if err != nil {
		t.Fatal(err)
	}
	if err := closed.Close(); err != nil {
		t.Fatal(err)
	}
	for _, pg := range []*db.DB{nil, {DB: closed}} {
		n := &Notifier{pg: pg, buckets: map[int64]*notifyBucket{}}
		n.step(t.Context())
		if len(n.buckets) != 0 {
			t.Fatal("unavailable storage admitted notification work")
		}
	}
}
