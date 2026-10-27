package db

import (
	"bytes"
	"context"
	"encoding/json"
	"testing"
)

func TestIntranetArchiveKeyMetadataDoesNotExportMasterKey(t *testing.T) {
	old := &TaskArchiveSnapshot{Tables: map[string]json.RawMessage{"findings": json.RawMessage(`[]`)}}
	if old.HasIntranetSecrets() {
		t.Fatal("legacy archive incorrectly requires encryption key")
	}
	old.Tables["credentials"] = json.RawMessage(`[{"id":1,"secret":"gcm1:fixture"}]`)
	if !old.HasIntranetSecrets() {
		t.Fatal("encrypted inventory not detected")
	}
	master := []byte("fixture-archive-master-key")
	fingerprint := ArchiveIntranetKeyFingerprint(master)
	if fingerprint == "" || fingerprint == ArchiveIntranetKeyFingerprint([]byte("other-key")) || ArchiveIntranetKeyFingerprint(nil) != "" {
		t.Fatal("key fingerprint did not distinguish encryption context")
	}
	old.IntranetKeyFingerprint = fingerprint
	raw, err := json.Marshal(old)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(raw, master) {
		t.Fatal("archive metadata exported master key")
	}
}

func TestTaskArchivePreservesIntranetInventoryAndVerificationAudit(t *testing.T) {
	d := portTestDB(t)
	task, err := d.CreateTask("intranet archive fixture", "fixture", nil, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = d.Exec(`DELETE FROM task_archives WHERE task_id=$1`, task.ID)
		_ = d.DeleteTask(task.ID)
	})
	if err = d.SetPaused(task.ID, true); err != nil {
		t.Fatal(err)
	}
	key := []byte("fixture-archive-master-key")
	sessions := NewSessionStore(d, key)
	sid, err := sessions.Create(t.Context(), &SessionRecord{Kind: "http_php", URL: "http://fixture.invalid/session", Secret: json.RawMessage(`{"token":"fixture-token"}`), Status: SessionDead, CreatedByTask: task.ID})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sessions.Delete(context.Background(), sid) })
	if err = sessions.BindTask(t.Context(), sid, task.ID); err != nil {
		t.Fatal(err)
	}
	credentials := NewCredentialStore(d, key)
	cid, err := credentials.Create(t.Context(), &CredentialRecord{TaskID: task.ID, Username: "fixture-user", CredType: CredPassword, Secret: "fixture-secret", Source: "fixture"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = credentials.Delete(context.Background(), cid) })
	if _, err = d.Assets().UpsertPendingScope(task.ID, "cidr", "192.0.2.0/24"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = d.Exec(`DELETE FROM pending_scope WHERE task_id=$1`, task.ID) })
	tunnels := NewTunnelStore(d)
	tid, err := tunnels.Create(t.Context(), &TunnelRecord{TaskID: task.ID, Kind: "socks", State: TunnelStopped, ViaSessionID: sid})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = d.Exec(`DELETE FROM tunnels WHERE id=$1`, tid) })
	f, err := d.Exploration(task.ExplorationID).RecordFinding(t.Context(), RecordFindingInput{TaskID: task.ID, Summary: "archive finding", Severity: "high", Evidence: "fixture evidence"})
	if err != nil {
		t.Fatal(err)
	}
	if err = d.SetFindingVerification(t.Context(), f.FindingID, VerificationBlocked, "fixture blocked condition", 0); err != nil {
		t.Fatal(err)
	}
	check, conv, created, err := d.CreateFindingCheck(t.Context(), f.FindingID, FindingCheckKindVerify)
	if err != nil || !created {
		t.Fatalf("create check: %v", err)
	}
	t.Cleanup(func() { _ = d.DeleteConversation(conv.ID) })
	if _, err = d.StartFindingCheck(t.Context(), check.ID); err != nil {
		t.Fatal(err)
	}
	if err = d.RecordFindingCheckResult(t.Context(), conv.ID, "inconclusive", "fixture comparison", "fixture blocked evidence"); err != nil {
		t.Fatal(err)
	}
	if err = d.FinishFindingCheck(check.ID, "completed", ""); err != nil {
		t.Fatal(err)
	}
	if _, err = d.Exec(`INSERT INTO finding_inline_merge_events(finding_id,kind,payload) VALUES($1,'original','{"fixture":true}')`, f.FindingID); err != nil {
		t.Fatal(err)
	}
	archive, err := d.QueueTaskArchive(task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = d.ClaimTaskArchiveJob(t.Context()); err != nil {
		t.Fatal(err)
	}
	snapshot, err := d.SnapshotTaskArchive(task.ID)
	if err != nil {
		t.Fatal(err)
	}
	snapshot.IntranetKeyFingerprint = ArchiveIntranetKeyFingerprint(key)
	for _, table := range []string{"sessions", "task_session_bindings", "tunnels", "credentials", "pending_scope", "finding_checks", "finding_verification_events", "finding_inline_merge_events"} {
		if snapshot.DataCounts[table] != 1 {
			t.Fatalf("snapshot %s rows=%d", table, snapshot.DataCounts[table])
		}
	}
	raw, _ := json.Marshal(snapshot)
	if !snapshot.HasIntranetSecrets() || bytes.Contains(raw, key) || bytes.Contains(raw, []byte("fixture-secret")) || bytes.Contains(raw, []byte("fixture-token")) {
		t.Fatal("archive leaked encryption material")
	}
	if err = d.CompleteTaskArchive(archive.ID, snapshot, "fixture-archive", "fixture-checksum", 100, 50); err != nil {
		t.Fatal(err)
	}
	if can, err := sessions.TaskCanUse(t.Context(), sid, task.ID); err != nil || can {
		t.Fatal("archived task retained session-control capability")
	}
	if _, err = d.QueueTaskArchiveRestore(archive.ID); err != nil {
		t.Fatal(err)
	}
	if _, err = d.ClaimTaskArchiveJob(t.Context()); err != nil {
		t.Fatal(err)
	}
	if _, err = d.RestoreTaskArchive(archive.ID, snapshot, 0); err != nil {
		t.Fatal(err)
	}
	restored, err := credentials.Get(t.Context(), cid)
	if err != nil || restored == nil || restored.Secret != "fixture-secret" {
		t.Fatalf("same-key inventory restore: %+v %v", restored, err)
	}
	if _, err = NewCredentialStore(d, []byte("wrong-key")).Get(t.Context(), cid); err == nil {
		t.Fatal("wrong encryption key accepted")
	}
	session, err := sessions.Get(t.Context(), sid)
	if err != nil || session.Status != SessionDead {
		t.Fatal("session was automatically revived")
	}
	tunnel, err := tunnels.Get(t.Context(), tid)
	if err != nil || tunnel.State != TunnelStopped {
		t.Fatal("tunnel was automatically restarted")
	}
	finding, err := d.GetFinding(f.FindingID)
	if err != nil || finding.VerificationStatus != VerificationBlocked {
		t.Fatal("verification status not preserved")
	}
	checks, err := d.ListFindingChecks(f.FindingID)
	if err != nil || len(checks) != 1 || checks[0].ConversationID != nil || checks[0].Verdict != "inconclusive" {
		t.Fatalf("verification audit not restored: %+v %v", checks, err)
	}
	events, err := d.ListFindingInlineMergeEvents(f.FindingID)
	if err != nil || len(events) != 1 {
		t.Fatal("raw submission audit lost")
	}
}
