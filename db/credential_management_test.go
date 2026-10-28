package db

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
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
