package db

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/url"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

func TestUpstreamMergeFindingDateAndVerificationFiltersCompose(t *testing.T) {
	d, task, ids := caseTestDB(t)
	for i, verification := range []string{"confirmed", "unverified", "confirmed", "blocked"} {
		day := "2026-10-28T12:00:00Z"
		if i == 2 {
			day = "2026-10-29T12:00:00Z"
		}
		if _, err := d.Exec(`UPDATE findings SET verification_status=$2,created_at=$3::timestamptz WHERE id=$1`, ids[i], verification, day); err != nil {
			t.Fatal(err)
		}
	}
	for _, tc := range []struct {
		verification, from, to string
		want                   int64
	}{
		{"unverified", "2026-10-28T00:00:00Z", "2026-10-28T23:59:59Z", ids[1]},
		{"confirmed", "2026-10-29T00:00:00Z", "2026-10-29T23:59:59Z", ids[2]},
		{"blocked", "2026-10-28T00:00:00Z", "2026-10-28T23:59:59Z", ids[3]},
	} {
		filter := FindingFilter{TaskID: fmt.Sprint(task.ID), Verification: tc.verification, From: tc.from, To: tc.to, Sort: "time", TimeDirection: "asc"}
		rows, total, err := d.ListFindingsPage(filter, 1, 10)
		if err != nil || total != 1 || len(rows) != 1 || rows[0].ID != tc.want {
			t.Fatalf("%s raw page: rows=%+v total=%d err=%v", tc.verification, rows, total, err)
		}
		groups, groupTotal, findingTotal, err := d.ListFindingGroups(filter, 1, 10)
		if err != nil || groupTotal != 1 || findingTotal != 1 || len(groups) != 1 || groups[0].TaskID == nil || *groups[0].TaskID != task.ID {
			t.Fatalf("%s groups: rows=%+v groups=%d findings=%d err=%v", tc.verification, groups, groupTotal, findingTotal, err)
		}
		cases, total, err := d.ListFindingCases(filter, 1, 10)
		if err != nil || total != 1 || len(cases) != 1 || cases[0].FindingID != tc.want {
			t.Fatalf("%s unified page: rows=%+v total=%d err=%v", tc.verification, cases, total, err)
		}
	}
}

func TestUpstreamMergeFindingSuggestionsConcurrentExactGroup(t *testing.T) {
	d, task, ids := caseTestDB(t)
	const writers = 8
	results := make(chan int64, writers)
	errors := make(chan error, writers)
	var wg sync.WaitGroup
	for i := 0; i < writers; i++ {
		wg.Add(1)
		go func(reverse bool) {
			defer wg.Done()
			members := append([]int64(nil), ids[:3]...)
			if reverse {
				members[0], members[2] = members[2], members[0]
			}
			id, err := d.SuggestFindingCase(context.Background(), task.ID, members, "same group", "same fixture evidence")
			results <- id
			errors <- err
		}(i%2 == 0)
	}
	wg.Wait()
	close(results)
	close(errors)
	for err := range errors {
		if err != nil {
			t.Fatal(err)
		}
	}
	var want int64
	for id := range results {
		if want == 0 {
			want = id
		}
		if id == 0 || id != want {
			t.Fatalf("same-member concurrent suggestion IDs differ: %d vs %d", id, want)
		}
	}
	groups, err := d.FindingCaseSuggestions(task.ID)
	if err != nil || len(groups) != 1 || groups[0].ID != want || len(groups[0].FindingIDs) != 3 {
		t.Fatalf("exact-group concurrency left duplicates: %+v %v", groups, err)
	}
}

func TestUpstreamMergeFindingSuggestionsSameAnchorsDifferentSets(t *testing.T) {
	d, task, ids := caseTestDB(t)
	left, err := d.SuggestFindingCase(t.Context(), task.ID, ids[:3], "left group", "first member set")
	if err != nil {
		t.Fatal(err)
	}
	right, err := d.SuggestFindingCase(t.Context(), task.ID, []int64{ids[0], ids[1], ids[3]}, "right group", "different complete member set")
	if err != nil || left == right {
		t.Fatalf("different sets sharing compatibility anchors were conflated: %d/%d %v", left, right, err)
	}
	groups, err := d.FindingCaseSuggestions(task.ID)
	if err != nil || len(groups) != 2 {
		t.Fatalf("different sets missing: %+v %v", groups, err)
	}
	for _, group := range groups {
		if group.LeftID != ids[0] || group.RightID != ids[1] || len(group.FindingIDs) != 3 {
			t.Fatalf("group lost its exact members: %+v", group)
		}
	}
}

// Use a separate CI database rather than temporarily dropping tables from the
// package's shared fixture database. No runtime/deployment database is involved.
func upstreamMergeFixtureDSN(t *testing.T) string {
	t.Helper()
	dsn := testDSN(t)
	admin, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	name := fmt.Sprintf("artex_merge_compat_%d", time.Now().UnixNano())
	identifier := pgx.Identifier{name}.Sanitize()
	if _, err := admin.ExecContext(t.Context(), `CREATE DATABASE `+identifier); err != nil {
		admin.Close()
		t.Fatalf("create isolated migration fixture database: %v", err)
	}
	t.Cleanup(func() {
		if _, err := admin.ExecContext(context.Background(), `DROP DATABASE `+identifier+` WITH (FORCE)`); err != nil {
			t.Errorf("drop isolated migration fixture database: %v", err)
		}
		admin.Close()
	})
	fixtureDSN, err := upstreamMergeDatabaseDSN(dsn, name)
	if err != nil {
		t.Fatal(err)
	}
	// Config.ConnString() returns pgx's ORIGINAL connection string; assigning
	// Config.Database then calling ConnString silently reuses the package DB.
	// Verify identity before any Open/schema/fixture writes can touch a database.
	probe, err := sql.Open("pgx", fixtureDSN)
	if err != nil {
		t.Fatal(err)
	}
	defer probe.Close()
	var connected string
	if err := probe.QueryRowContext(t.Context(), `SELECT current_database()`).Scan(&connected); err != nil {
		t.Fatal(err)
	}
	if connected != name {
		t.Fatalf("migration fixture connected to %q instead of its isolated database %q", connected, name)
	}
	return fixtureDSN
}

func upstreamMergeDatabaseDSN(dsn, database string) (string, error) {
	if _, err := pgx.ParseConfig(dsn); err != nil {
		return "", err
	}
	u, err := url.Parse(dsn)
	if err == nil && (u.Scheme == "postgres" || u.Scheme == "postgresql") {
		u.Path, u.RawPath = "/"+database, ""
		query := u.Query()
		query.Del("dbname") // an explicit query override must not defeat the new path
		u.RawQuery = query.Encode()
		return u.String(), nil
	}
	// libpq keyword DSNs accept a final dbname override. Escape the value rather
	// than reconstructing credentials/TLS/runtime options from parsed fields.
	value := strings.ReplaceAll(strings.ReplaceAll(database, `\`, `\\`), `'`, `\'`)
	return dsn + " dbname='" + value + "'", nil
}

func TestUpstreamMergeFixtureDSNTargetsNewDatabase(t *testing.T) {
	for _, original := range []string{
		"postgres://fixture:fixture-pass@127.0.0.1:5432/package_db?sslmode=disable&application_name=merge-fixture",
		"postgresql://fixture@127.0.0.1:5432/package_db?sslmode=disable",
		"postgres://fixture@127.0.0.1:5432/package_db?sslmode=disable&dbname=another_package_db",
		"host=127.0.0.1 port=5432 user=fixture password=fixture-pass dbname=package_db sslmode=disable application_name=merge-fixture",
	} {
		before, err := pgx.ParseConfig(original)
		if err != nil {
			t.Fatal(err)
		}
		dsn, err := upstreamMergeDatabaseDSN(original, "isolated_merge_fixture")
		if err != nil {
			t.Fatal(err)
		}
		after, err := pgx.ParseConfig(dsn)
		if err != nil || after.Database != "isolated_merge_fixture" {
			t.Fatalf("fixture DSN did not select its own database: %v", err)
		}
		if after.Host != before.Host || after.Port != before.Port || after.User != before.User || after.Password != before.Password || !reflect.DeepEqual(after.RuntimeParams, before.RuntimeParams) || (after.TLSConfig == nil) != (before.TLSConfig == nil) {
			t.Fatal("fixture database override changed connection credentials or options")
		}
	}
}

func TestUpstreamMergeLegacyPairMigrationReopenIsIdempotent(t *testing.T) {
	dsn := upstreamMergeFixtureDSN(t)
	d, err := Open(dsn)
	if err != nil {
		t.Fatal(err)
	}
	initial := d
	t.Cleanup(func() { initial.Close() })
	task, err := d.CreateTask("legacy migration fixture", "preserve records", nil, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	ids := make([]int64, 4)
	for i := range ids {
		ids[i], err = d.AddFinding(task.ID, 0, "fixture", "finding", "low", "summary", "evidence", "worker", nil)
		if err != nil {
			t.Fatal(err)
		}
	}
	if err := d.SetSetting("workers", "4"); err != nil {
		t.Fatal(err)
	}
	var protectedCheck string
	if err := d.QueryRow(`SELECT pg_get_constraintdef(oid) FROM pg_constraint WHERE conrelid='side_question_sessions'::regclass AND conname='side_question_sessions_check'`).Scan(&protectedCheck); err != nil {
		t.Fatal(err)
	}
	// Recreate only the previous suggestion representation and the old task
	// columns. The next Open must migrate them without deleting history.
	if _, err := d.Exec(`DROP TRIGGER finding_case_suggestion_reanchor ON findings;
DROP FUNCTION finding_case_suggestion_reanchor();
DROP TABLE finding_case_suggestion_members;
ALTER TABLE finding_case_suggestions ADD CONSTRAINT finding_case_suggestions_left_id_right_id_key UNIQUE(left_id,right_id);
ALTER TABLE tasks DROP COLUMN worker_count, DROP COLUMN active_run_seconds, DROP COLUMN run_segment_started_at;
ALTER TABLE task_templates DROP COLUMN remark, DROP COLUMN worker_count;`); err != nil {
		t.Fatal(err)
	}
	var pending, rejected int64
	if err := d.QueryRow(`INSERT INTO finding_case_suggestions(task_id,left_id,right_id,title,reason,state) VALUES($1,$2,$3,'pending history','old evidence','pending') RETURNING id`, task.ID, ids[0], ids[1]).Scan(&pending); err != nil {
		t.Fatal(err)
	}
	if err := d.QueryRow(`INSERT INTO finding_case_suggestions(task_id,left_id,right_id,title,reason,state) VALUES($1,$2,$3,'rejected history','human decision','rejected') RETURNING id`, task.ID, ids[2], ids[3]).Scan(&rejected); err != nil {
		t.Fatal(err)
	}
	d.Close()
	for pass := 0; pass < 2; pass++ {
		d, err = Open(dsn)
		if err != nil {
			t.Fatal(err)
		}
		opened := d
		t.Cleanup(func() { opened.Close() })
		var rows, members, oldUnique int
		if err := d.QueryRow(`SELECT count(*) FROM finding_case_suggestions WHERE id=ANY($1::bigint[])`, []int64{pending, rejected}).Scan(&rows); err != nil {
			t.Fatal(err)
		}
		if err := d.QueryRow(`SELECT count(*) FROM finding_case_suggestion_members WHERE suggestion_id=ANY($1::bigint[])`, []int64{pending, rejected}).Scan(&members); err != nil {
			t.Fatal(err)
		}
		if err := d.QueryRow(`SELECT count(*) FROM pg_constraint WHERE conrelid='finding_case_suggestions'::regclass AND conname='finding_case_suggestions_left_id_right_id_key'`).Scan(&oldUnique); err != nil {
			t.Fatal(err)
		}
		if rows != 2 || members != 4 || oldUnique != 0 {
			t.Fatalf("migration pass %d changed history or duplicated members: rows=%d members=%d oldUnique=%d", pass, rows, members, oldUnique)
		}
		var state, check, workers string
		if err := d.QueryRow(`SELECT state FROM finding_case_suggestions WHERE id=$1`, rejected).Scan(&state); err != nil {
			t.Fatal(err)
		}
		if err := d.QueryRow(`SELECT pg_get_constraintdef(oid) FROM pg_constraint WHERE conrelid='side_question_sessions'::regclass AND conname='side_question_sessions_check'`).Scan(&check); err != nil {
			t.Fatal(err)
		}
		if err := d.QueryRow(`SELECT value FROM settings WHERE key='workers'`).Scan(&workers); err != nil {
			t.Fatal(err)
		}
		if state != "rejected" || check != protectedCheck || workers != "4" {
			t.Fatalf("migration pass %d overwrote human state, protection CHECK, or settings", pass)
		}
		got, err := d.GetTask(task.ID)
		if err != nil || got == nil || got.WorkerCount != pass*7 || got.ActiveRunSeconds != int64(pass*23) {
			t.Fatalf("migration pass %d task defaults/custom values: %+v %v", pass, got, err)
		}
		if pass == 0 {
			if _, err := d.Exec(`UPDATE tasks SET worker_count=7,active_run_seconds=23 WHERE id=$1`, task.ID); err != nil {
				t.Fatal(err)
			}
		}
		d.Close()
	}
}

func TestUpstreamMergeLegacyArchiveRuntimeDefaults(t *testing.T) {
	d, task, _ := caseTestDB(t)
	var raw []byte
	if err := d.QueryRow(`SELECT to_jsonb(t) FROM tasks t WHERE id=$1`, task.ID).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	var row map[string]any
	if err := json.Unmarshal(raw, &row); err != nil {
		t.Fatal(err)
	}
	delete(row, "worker_count")
	delete(row, "active_run_seconds")
	delete(row, "run_segment_started_at")
	if _, err := d.Exec(`UPDATE tasks SET worker_count=9,active_run_seconds=57,run_segment_started_at=now() WHERE id=$1`, task.ID); err != nil {
		t.Fatal(err)
	}
	tx, err := d.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if err := restoreTaskStub(tx, row, task.ID, 0); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	got, err := d.GetTask(task.ID)
	if err != nil || got == nil || got.WorkerCount != 0 || got.ActiveRunSeconds != 0 || got.RunSegmentStartedAt != nil {
		t.Fatalf("legacy archive inherited stale runtime fields: %+v %v", got, err)
	}
}

func TestUpstreamMergeSuggestionIdentityKeepsMemberOrderCanonical(t *testing.T) {
	got, err := sortedFindingIDs([]int64{7, 2, 5})
	if err != nil || !reflect.DeepEqual(got, []int64{2, 5, 7}) {
		t.Fatalf("canonical identity=%v %v", got, err)
	}
}
