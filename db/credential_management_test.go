package db

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
)

func TestNormalizeCredentialManagementMetadata(t *testing.T) {
	target, domain, source, err := NormalizeCredentialMetadata(" https://target.invalid/login?token=discard#fragment ", " CORP ", "https://source.invalid/evidence?key=discard#fragment")
	if err != nil || target != "https://target.invalid/login" || domain != "CORP" || source != "https://source.invalid/evidence" {
		t.Fatalf("normalization: %q %q %q %v", target, domain, source, err)
	}
	for _, raw := range []string{"javascript:alert(1)", "https://user:password@fixture.invalid/", "ftp://fixture.invalid/", "https://fixture.invalid/\nheader"} {
		if _, _, _, err := NormalizeCredentialMetadata("", "", raw); !errors.Is(err, ErrCredentialMetadataInvalid) {
			t.Errorf("unsafe source accepted: %q", raw)
		}
	}
	for _, raw := range []string{"https://user:pass@fixture.invalid", "fixture.invalid/path", "fixture.invalid?token=secret", "fixture.invalid\\path", "javascript:alert(1)"} {
		if _, _, _, err := NormalizeCredentialMetadata(raw, "", ""); !errors.Is(err, ErrCredentialMetadataInvalid) {
			t.Errorf("unsafe target accepted: %q", raw)
		}
	}
	for _, host := range []string{"target.invalid", "target.invalid:443", "192.0.2.1", "[::1]:8787"} {
		if _, _, _, err := NormalizeCredentialMetadata(host, "", ""); err != nil {
			t.Errorf("host rejected: %q %v", host, err)
		}
	}
}

func TestNormalizeCredentialManagementVerification(t *testing.T) {
	for _, status := range []string{"verified", "failed"} {
		if _, _, _, err := NormalizeCredentialVerification(status, "", ""); !errors.Is(err, ErrCredentialVerificationInvalid) {
			t.Fatal("verification without evidence accepted")
		}
		got, method, evidence, err := NormalizeCredentialVerification(status, " manual ", " fixture evidence ")
		if err != nil || got != status || method != "manual" || evidence != "fixture evidence" {
			t.Fatal("valid verification rejected")
		}
	}
	status, method, evidence, err := NormalizeCredentialVerification("unverified", "previous method", "previous evidence")
	if err != nil || status != "unverified" || method != "" || evidence != "" {
		t.Fatal("reset retained current verification claim")
	}
	if _, _, _, err := NormalizeCredentialVerification("unknown", "manual", "evidence"); err == nil {
		t.Fatal("unknown status accepted")
	}
}

func TestCredentialRecordNeverSerializesSecret(t *testing.T) {
	raw, err := json.Marshal(&CredentialRecord{Secret: "fixture-plaintext-secret"})
	if err != nil || strings.Contains(string(raw), "fixture-plaintext-secret") || strings.Contains(string(raw), `"secret":`) {
		t.Fatal("decrypted record serialized secret")
	}
}

func TestCredentialArchiveDefaultsPreserveLegacyMeaning(t *testing.T) {
	raw, err := credentialArchiveDefaults(json.RawMessage(`[{"id":1,"verified":false,"secret":"gcm1:opaque"},{"id":2,"verified":true,"verification_history":null},{"id":3,"verified":false,"verification_status":"failed","verification_method":"manual","verification_history":[{"status":"failed"}]}]`))
	if err != nil {
		t.Fatal(err)
	}
	var rows []map[string]any
	if err := json.Unmarshal(raw, &rows); err != nil {
		t.Fatal(err)
	}
	if rows[0]["verification_status"] != "unverified" || rows[1]["verification_status"] != "verified" || rows[2]["verification_status"] != "failed" {
		t.Fatal("legacy states changed")
	}
	if len(rows[0]["verification_history"].([]any)) != 0 || len(rows[1]["verification_history"].([]any)) != 0 || len(rows[2]["verification_history"].([]any)) != 1 {
		t.Fatal("history overwritten or invented")
	}
	if rows[0]["secret"] != "gcm1:opaque" || rows[0]["source_intent_id"] != float64(0) || rows[0]["target"] != "" || rows[0]["verification_at"] != nil {
		t.Fatal("legacy defaults changed source/ciphertext/time")
	}
}

func TestCredentialManagementMigrationIsIdempotent(t *testing.T) {
	d := portTestDB(t)
	tx, err := d.BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if _, err = tx.Exec(`CREATE TEMP TABLE credentials(id BIGINT PRIMARY KEY,verified BOOLEAN NOT NULL DEFAULT false) ON COMMIT DROP; INSERT INTO credentials(id,verified) VALUES(1,false),(2,true)`); err != nil {
		t.Fatal(err)
	}
	start := strings.Index(schemaSQL, "-- Additive, restart-safe upgrade of the credential management inventory.")
	if start < 0 {
		t.Fatal("credential migration block not found")
	}
	end := strings.Index(schemaSQL[start:], "CREATE INDEX IF NOT EXISTS idx_credentials_task")
	if end < 0 {
		t.Fatal("credential migration block not found")
	}
	migration := schemaSQL[start : start+end]
	if _, err = tx.Exec(migration); err != nil {
		t.Fatal(err)
	}
	var first, second string
	if err = tx.QueryRow(`SELECT verification_status FROM credentials WHERE id=1`).Scan(&first); err != nil {
		t.Fatal(err)
	}
	if err = tx.QueryRow(`SELECT verification_status FROM credentials WHERE id=2`).Scan(&second); err != nil {
		t.Fatal(err)
	}
	if first != "unverified" || second != "verified" {
		t.Fatal("legacy bool mapping changed")
	}
	if _, err = tx.Exec(`UPDATE credentials SET verification_status='failed' WHERE id=1`); err != nil {
		t.Fatal(err)
	}
	if _, err = tx.Exec(migration); err != nil {
		t.Fatal(err)
	}
	if err = tx.QueryRow(`SELECT verification_status FROM credentials WHERE id=1`).Scan(&first); err != nil || first != "failed" {
		t.Fatal("restart overwrote new verification state")
	}
}

func TestCredentialOldArchiveRestoresWithNewColumns(t *testing.T) {
	d := portTestDB(t)
	tx, err := d.BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if _, err = tx.Exec(`CREATE TEMP TABLE credentials (LIKE public.credentials INCLUDING ALL) ON COMMIT DROP`); err != nil {
		t.Fatal(err)
	}
	raw := json.RawMessage(`[{"id":991,"task_id":992,"host_asset_id":null,"username":"fixture","cred_type":"token","secret":"gcm1:opaque","domain":"","source":"legacy source","verified":false,"created_at":"2026-01-01T00:00:00Z"}]`)
	if err = insertArchiveRows(tx, "credentials", raw); err != nil {
		t.Fatal(err)
	}
	var status, target, source string
	var history []byte
	var verified bool
	if err = tx.QueryRow(`SELECT verification_status,verified,target,source,verification_history FROM credentials WHERE id=991`).Scan(&status, &verified, &target, &source, &history); err != nil {
		t.Fatal(err)
	}
	if status != "unverified" || verified || target != "" || source != "legacy source" || string(history) != "[]" {
		t.Fatal("legacy archive semantics changed")
	}
}

func TestCredentialManagementHistoryAndTaskIsolation(t *testing.T) {
	d := portTestDB(t)
	task, err := d.CreateTask("credential management fixture", "fixture", nil, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = d.Exec(`DELETE FROM credentials WHERE task_id=$1`, task.ID)
		_, _ = d.Exec(`DELETE FROM task_archives WHERE task_id=$1`, task.ID)
		_ = d.DeleteTask(task.ID)
	})
	store := NewCredentialStore(d, []byte("credential-management-fixture-key"))
	id, err := store.Create(t.Context(), &CredentialRecord{TaskID: task.ID, CredType: CredToken, Secret: "fixture-original-secret", Source: "original free-text source", Target: "target.invalid", Domain: "CORP", SourceURL: "https://source.invalid/evidence?token=discard"})
	if err != nil {
		t.Fatal(err)
	}
	initial, err := store.Get(t.Context(), id)
	if err != nil || initial == nil {
		t.Fatal("initial credential read failed", err)
	}
	if !initial.TaskAvailable || initial.TargetLabel != "target.invalid" || initial.TargetURL != "" || initial.SourceURL != "https://source.invalid/evidence" {
		t.Fatal("target/source/task attribution incorrect")
	}
	if err = store.SetVerification(t.Context(), id, "verified", "manual", "contains fixture-original-secret", "fixture-admin"); !errors.Is(err, ErrCredentialVerificationContainsSecret) {
		t.Fatal("verification leaked raw credential", err)
	}
	rejected, err := store.Get(t.Context(), id)
	if err != nil || rejected.VerificationStatus != "unverified" || len(rejected.VerificationHistory) != 0 {
		t.Fatal("rejected evidence changed status/history")
	}
	for _, status := range []string{"verified", "failed", "unverified"} {
		if err = store.SetVerification(t.Context(), id, status, "manual fixture", "fixture evidence", "fixture-admin"); err != nil {
			t.Fatal(err)
		}
	}
	current, err := store.Get(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	if current.Verified || current.VerificationStatus != "unverified" || current.VerificationAt != nil || current.VerificationMethod != "" || current.VerificationEvidence != "" || len(current.VerificationHistory) != 3 || current.VerificationHistory[0].Status != "verified" {
		t.Fatal("reset lost history or retained current verification")
	}
	if err = store.UpdateMetadata(t.Context(), id, "https://target.invalid/path?token=discard", "REALM", "https://source.invalid/updated?token=discard"); err != nil {
		t.Fatal(err)
	}
	current, err = store.Get(t.Context(), id)
	if err != nil || current.Secret != "fixture-original-secret" || current.Source != "original free-text source" || current.TargetURL != "https://target.invalid/path" {
		t.Fatal("metadata changed secret/source or unsafe target URL")
	}
	// A source URL alone must never be presented as a target.
	if err = store.UpdateMetadata(t.Context(), id, "", "", "https://other.invalid/evidence"); err != nil {
		t.Fatal(err)
	}
	current, err = store.Get(t.Context(), id)
	if err != nil || current.TargetLabel != "" || current.TargetURL != "" {
		t.Fatal("source URL was guessed to be target")
	}
	if err = store.SetVerification(t.Context(), id, "verified", "", "", "fixture-admin"); !errors.Is(err, ErrCredentialVerificationInvalid) {
		t.Fatal("evidence-free verification accepted")
	}
	if err = d.SetPaused(task.ID, true); err != nil {
		t.Fatal(err)
	}
	if _, err = d.QueueTaskArchive(task.ID); err != nil {
		t.Fatal(err)
	}
	if err = store.SetVerification(t.Context(), id, "verified", "manual", "evidence", "fixture-admin"); !errors.Is(err, ErrCredentialUnavailable) {
		t.Fatal("verification crossed archive barrier", err)
	}
	if err = store.UpdateMetadata(t.Context(), id, "other.invalid", "", ""); !errors.Is(err, ErrCredentialUnavailable) {
		t.Fatal("metadata crossed archive barrier", err)
	}
}

// An exploration omitted by partial RunInfo is resolved from the trusted task
// row; the intent must still be one of that task's actual intent nodes.
func TestCredentialCreateResolvesPartialSourceContext(t *testing.T) {
	d := portTestDB(t)
	task, err := d.CreateTask("partial credential source fixture", "fixture", nil, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = d.Exec(`DELETE FROM credentials WHERE task_id=$1`, task.ID); _ = d.DeleteTask(task.ID) })
	other, err := d.CreateTask("other credential source fixture", "fixture", nil, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.DeleteTask(other.ID) })
	intent, err := d.Exploration(task.ExplorationID).AddIntent(map[string]any{"task": "fixture"}, 1, nil, "human")
	if err != nil {
		t.Fatal(err)
	}
	otherIntent, err := d.Exploration(other.ExplorationID).AddIntent(map[string]any{"task": "fixture"}, 1, nil, "human")
	if err != nil {
		t.Fatal(err)
	}
	store := NewCredentialStore(d, []byte("partial-source-fixture-key"))
	id, err := store.Create(t.Context(), &CredentialRecord{TaskID: task.ID, CredType: CredToken, Secret: "fixture-source-secret", Source: "fixture", SourceIntentID: intent})
	if err != nil {
		t.Fatal("partial source context was rejected", err)
	}
	rec, err := store.Get(t.Context(), id)
	if err != nil || rec == nil || rec.SourceIntentID != intent || rec.SourceExplorationID != task.ExplorationID {
		t.Fatal("resolved source context was not persisted")
	}
	for _, source := range []struct{ intent, exploration int64 }{
		{otherIntent, 0},
		{intent, other.ExplorationID},
		{otherIntent, task.ExplorationID},
		{0, other.ExplorationID},
	} {
		_, err := store.Create(t.Context(), &CredentialRecord{TaskID: task.ID, CredType: CredToken, Secret: "fixture-rejected-source-secret", Source: "fixture", SourceIntentID: source.intent, SourceExplorationID: source.exploration})
		if !errors.Is(err, ErrCredentialMetadataInvalid) {
			t.Fatal("cross-task source context was accepted", err)
		}
	}
	rows, err := store.ListByTask(t.Context(), task.ID)
	if err != nil || len(rows) != 1 {
		t.Fatal("rejected source context unexpectedly inserted a credential")
	}
}

func TestCredentialRevealLocksAndRetainedRecords(t *testing.T) {
	d := portTestDB(t)
	task, err := d.CreateTask("retained credential read fixture", "local fixture", nil, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = d.Exec(`DELETE FROM credentials WHERE task_id=$1`, task.ID)
		_, _ = d.Exec(`DELETE FROM task_archives WHERE task_id=$1`, task.ID)
		_ = d.DeleteTask(task.ID)
	})
	store := NewCredentialStore(d, []byte("retained-read-fixture-key"))
	id, err := store.Create(t.Context(), &CredentialRecord{TaskID: task.ID, CredType: CredToken, Secret: "synthetic-retained-value", Source: "synthetic source"})
	if err != nil {
		t.Fatal(err)
	}

	record, release, err := store.GetForReveal(t.Context(), id)
	if err != nil || record == nil || release == nil || !record.TaskAvailable || !record.RevealAvailable {
		t.Fatal("live credential read failed", err)
	}
	defer release()
	for _, query := range []string{
		`SELECT id FROM tasks WHERE id=$1 FOR UPDATE NOWAIT`,
		`SELECT id FROM credentials WHERE task_id=$1 FOR UPDATE NOWAIT`,
	} {
		probe, err := d.BeginTx(t.Context(), nil)
		if err != nil {
			t.Fatal(err)
		}
		_, lockErr := probe.ExecContext(t.Context(), query, task.ID)
		_ = probe.Rollback()
		var pgErr *pgconn.PgError
		if !errors.As(lockErr, &pgErr) || pgErr.Code != "55P03" {
			t.Fatal("reveal did not keep task and credential write locks excluded")
		}
	}
	release()
	probe, err := d.BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = probe.ExecContext(t.Context(), `SELECT id FROM tasks WHERE id=$1 FOR UPDATE NOWAIT`, task.ID); err != nil {
		_ = probe.Rollback()
		t.Fatal("reveal did not release the task lock", err)
	}
	_ = probe.Rollback()

	if err = d.SetPaused(task.ID, true); err != nil {
		t.Fatal(err)
	}
	if _, err = d.QueueTaskArchive(task.ID); err != nil {
		t.Fatal(err)
	}
	record, release, err = store.GetForReveal(t.Context(), id)
	if !errors.Is(err, ErrCredentialUnavailable) || release != nil {
		if release != nil {
			release()
		}
		t.Fatal("reveal crossed an archive queue", err)
	}
	if _, err = d.Exec(`DELETE FROM task_archives WHERE task_id=$1`, task.ID); err != nil {
		t.Fatal(err)
	}
	if _, err = d.Exec(`UPDATE tasks SET deleted_at=now() WHERE id=$1`, task.ID); err != nil {
		t.Fatal(err)
	}
	assertRetainedRead := func() {
		t.Helper()
		record, done, err := store.GetForReveal(t.Context(), id)
		if err != nil || record == nil || done == nil {
			t.Fatal("retained credential read failed", err)
		}
		defer done()
		if record.TaskAvailable || !record.RevealAvailable || record.Secret != "synthetic-retained-value" || record.TaskID != task.ID {
			t.Fatal("retained credential read lost its value or provenance")
		}
	}
	assertRetainedRead()
	if err = store.UpdateMetadata(t.Context(), id, "fixture.invalid", "", ""); !errors.Is(err, ErrCredentialUnavailable) {
		t.Fatal("deleted task allowed credential editing", err)
	}
	if err = d.DeleteTask(task.ID); err != nil {
		t.Fatal(err)
	}
	assertRetainedRead()
	if err = store.Delete(t.Context(), id); err != nil {
		t.Fatal(err)
	}
	if _, done, err := store.GetForReveal(t.Context(), id); !errors.Is(err, ErrCredentialNotFound) || done != nil {
		if done != nil {
			done()
		}
		t.Fatal("removed credential was still readable", err)
	}
}

func TestCredentialRevealCancellationReleasesReadLocks(t *testing.T) {
	d := portTestDB(t)
	task, err := d.CreateTask("credential canceled read fixture", "local fixture", nil, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = d.Exec(`DELETE FROM credentials WHERE task_id=$1`, task.ID); _ = d.DeleteTask(task.ID) })
	store := NewCredentialStore(d, []byte("canceled-read-fixture-key"))
	id, err := store.Create(t.Context(), &CredentialRecord{TaskID: task.ID, CredType: CredToken, Secret: "synthetic-cancel-value", Source: "synthetic source"})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	_, done, err := store.GetForReveal(ctx, id)
	if err != nil || done == nil {
		t.Fatal("cannot obtain read locks", err)
	}
	defer done()
	cancel()
	probeCtx, probeCancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer probeCancel()
	probe, err := d.BeginTx(probeCtx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer probe.Rollback()
	if _, err = probe.ExecContext(probeCtx, `SELECT id FROM tasks WHERE id=$1 FOR UPDATE`, task.ID); err != nil {
		t.Fatal("cancellation left a read lock behind", err)
	}
}
