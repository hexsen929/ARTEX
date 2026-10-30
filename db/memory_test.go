package db

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

func memoryFixtureDB(t *testing.T) *DB {
	t.Helper()
	d, err := Open(testDSN(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.Close() })
	original, err := d.GetMemorySettings(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		// Restore only singleton settings, never erase unrelated fixture data.
		_, err := d.Exec(`UPDATE memory_settings SET enabled=$1,auto_extract=$2,global_share=$3,profile_id=$4,
			max_entries=$5,max_tokens=$6,max_jobs_per_hour=$7,retention_days=$8,learn_after=$9,epoch=epoch+1`,
			original.Enabled, original.AutoExtract, original.GlobalShare, original.ProfileID, original.MaxEntries,
			original.MaxTokens, original.MaxJobsPerHour, original.RetentionDays, original.LearnAfter)
		if err != nil {
			t.Errorf("restore memory settings: %v", err)
		}
	})
	settings := original
	settings.Enabled, settings.AutoExtract, settings.GlobalShare = true, true, true
	settings.ProfileID, settings.MaxJobsPerHour = 0, 60
	if _, err := d.UpdateMemorySettings(t.Context(), settings, original.Epoch); err != nil {
		t.Fatal(err)
	}
	return d
}

func memoryFixtureTask(t *testing.T, d *DB) *Task {
	t.Helper()
	task, err := d.CreateTask("memory fixture", "memory fixture", nil, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := d.DeleteTask(task.ID); err != nil {
			t.Errorf("delete fixture task: %v", err)
		}
		for _, query := range []string{
			`DELETE FROM memory_audit WHERE task_id=$1 OR entry_id IN (SELECT id FROM memory_entries WHERE origin_task_id=$1)`,
			`DELETE FROM memory_entries WHERE origin_task_id=$1`,
			`DELETE FROM memory_jobs WHERE source_ref IN (SELECT id FROM memory_sources WHERE origin_task_id=$1)`,
			`DELETE FROM memory_sources WHERE origin_task_id=$1`,
		} {
			if _, err := d.Exec(query, task.ID); err != nil {
				t.Errorf("clean fixture memory metadata: %v", err)
			}
		}
	})
	return task
}

func memoryFixtureSource(t *testing.T, d *DB, task *Task) MemorySource {
	t.Helper()
	nodeID, err := d.Exploration(task.ExplorationID).AddNode(KindFact,
		map[string]any{"summary": "memory fixture tool supports bounded structured output", "confidence": "confirmed"},
		0, "confirmed", "worker", nil)
	if err != nil {
		t.Fatal(err)
	}
	source, err := d.GetMemorySource(t.Context(), "node", fmt.Sprint(nodeID))
	if err != nil || source == nil {
		t.Fatalf("get source: %+v %v", source, err)
	}
	return *source
}

func memoryQueueClaim(t *testing.T, d *DB, source MemorySource) MemoryJob {
	t.Helper()
	queued, err := d.QueueMemoryExtraction(t.Context(), source)
	if err != nil || !queued {
		t.Fatalf("queue=%v err=%v", queued, err)
	}
	job, err := d.ClaimMemoryJob(t.Context(), time.Minute)
	if err != nil || job == nil || job.Source.Kind != source.Kind || job.Source.ID != source.ID {
		t.Fatalf("claim=%+v err=%v", job, err)
	}
	return *job
}

func TestMemoryInputAndExactIdentity(t *testing.T) {
	base := MemoryInput{Title: "fixture", Content: "原文 未支持 v1.20", Kind: "lesson"}
	if _, err := validateMemoryInput(base, true); err != nil {
		t.Fatal(err)
	}
	for _, change := range []func(*MemoryInput){
		func(in *MemoryInput) { in.Content = "" },
		func(in *MemoryInput) { in.Kind = "preference" },
		func(in *MemoryInput) { in.Content = "api_key=abcdefghijklmnop" },
		func(in *MemoryInput) { in.Target = "https://fixture-user:fixture-pass" + "@" + "fixture.invalid" },
		func(in *MemoryInput) { in.Tags = []string{"Authorization: Bearer abcdefghijklmnop"} },
		func(in *MemoryInput) { in.Applicability = "-----BEGIN PRIVATE KEY-----" },
	} {
		input := base
		change(&input)
		if _, err := validateMemoryInput(input, false); !errors.Is(err, ErrMemoryInput) {
			t.Errorf("accepted invalid fixture: %v", err)
		}
	}
	for _, pair := range [][2]string{{"已验证", "未验证"}, {"v1.2", "v1.20"}, {"é", "e\u0301"}, {"a b", "a  b"}, {"Ａ", "A"}} {
		if memoryContentHash(pair[0]) == memoryContentHash(pair[1]) {
			t.Fatalf("collapsed original byte identity: %q %q", pair[0], pair[1])
		}
	}
	terms := memorySearchTerms("排查工具兼容性 v1.20")
	if !strings.Contains(strings.Join(terms, "|"), "兼容") || !strings.Contains(strings.Join(terms, "|"), "v1.20") {
		t.Fatalf("search terms=%v", terms)
	}
	if memoryLikePattern("a%b_c") != `%a\%b\_c%` {
		t.Fatal("search wildcards were not escaped")
	}
}

func TestMemoryMetadataSerialization(t *testing.T) {
	encoded, err := json.Marshal(MemorySource{Kind: "node", ID: "1", Text: "PRIVATE_SOURCE_CANARY", Fingerprint: "PRIVATE_FINGERPRINT_CANARY"})
	if err != nil || strings.Contains(string(encoded), "CANARY") {
		t.Fatalf("source body escaped worker-only serialization: %s %v", encoded, err)
	}
	encoded, err = json.Marshal(MemoryJob{LeaseToken: "PRIVATE_LEASE_CANARY", Source: MemorySource{Text: "PRIVATE_SOURCE_CANARY"}})
	if err != nil || strings.Contains(string(encoded), "CANARY") {
		t.Fatalf("job exposed private fields: %s %v", encoded, err)
	}
}

func TestMemorySettingsCASAndReadOnlyBoundary(t *testing.T) {
	d := memoryFixtureDB(t)
	before, err := d.GetMemorySettings(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	after, err := d.GetMemorySettings(t.Context())
	if err != nil || before.Epoch != after.Epoch || !before.LearnAfter.Equal(after.LearnAfter) {
		t.Fatalf("GET changed settings: %+v %+v %v", before, after, err)
	}
	input := before
	input.GlobalShare = false
	input.LearnAfter = time.Time{} // read-only even when supplied in an update
	stored, err := d.UpdateMemorySettings(t.Context(), input, before.Epoch)
	if err != nil || stored.Epoch != before.Epoch+1 || !stored.LearnAfter.Equal(before.LearnAfter) {
		t.Fatalf("settings update=%+v %v", stored, err)
	}
	if _, err := d.UpdateMemorySettings(t.Context(), input, before.Epoch); !errors.Is(err, ErrMemoryConflict) {
		t.Fatalf("stale settings accepted: %v", err)
	}
}

func TestMemorySharedSearchAdmissionAndDeletion(t *testing.T) {
	d := memoryFixtureDB(t)
	a, b := memoryFixtureTask(t, d), memoryFixtureTask(t, d)
	input := MemoryInput{Title: "shared compatibility", Content: "SHARED_MEMORY_FIXTURE supports a bounded request", Kind: "environment",
		OriginTaskID: a.ID, AgentKey: "worker", Applicability: "SHARED_MEMORY_FIXTURE", Tags: []string{"fixture"}}
	entry, err := d.CreateManualMemory(t.Context(), input)
	if err != nil {
		t.Fatal(err)
	}
	items, settings, err := d.SearchMemory(t.Context(), MemorySearchInput{Query: "SHARED_MEMORY_FIXTURE", TaskID: b.ID, AgentKey: "worker"})
	if err != nil || len(items) != 1 || items[0].ID != entry.ID || items[0].OriginTaskID != a.ID {
		t.Fatalf("cross-task search=%+v %v", items, err)
	}
	admission := MemoryAdmission{SettingsEpoch: settings.Epoch, References: []MemoryRef{{ID: entry.ID, Version: entry.Version}}, TaskID: b.ID, AgentKey: "worker", SessionID: "fixture", CallKey: "fixture-call"}
	if err := d.ValidateMemoryAdmission(t.Context(), admission); err != nil {
		t.Fatal(err)
	}
	used, err := d.GetMemoryEntry(t.Context(), entry.ID)
	if err != nil || used.UseCount != 1 || used.LastUsedAt == nil {
		t.Fatalf("usage metadata=%+v %v", used, err)
	}
	settings.GlobalShare = false
	settings, err = d.UpdateMemorySettings(t.Context(), settings, settings.Epoch)
	if err != nil {
		t.Fatal(err)
	}
	items, _, err = d.SearchMemory(t.Context(), MemorySearchInput{Query: "SHARED_MEMORY_FIXTURE", TaskID: b.ID})
	if err != nil || len(items) != 0 {
		t.Fatalf("disabled sharing returned foreign note: %+v %v", items, err)
	}
	if err := d.ValidateMemoryAdmission(t.Context(), admission); !errors.Is(err, ErrMemoryStale) {
		t.Fatalf("old settings admission=%v", err)
	}
	input.Content = "SHARED_MEMORY_FIXTURE corrected result"
	edited, err := d.EditMemoryEntry(t.Context(), entry.ID, input, entry.Version)
	if err != nil || edited.Version != entry.Version+1 {
		t.Fatalf("edit=%+v %v", edited, err)
	}
	if _, err := d.EditMemoryEntry(t.Context(), entry.ID, input, entry.Version); !errors.Is(err, ErrMemoryConflict) {
		t.Fatalf("stale edit=%v", err)
	}
	if err := d.DeleteMemoryEntry(t.Context(), entry.ID, edited.Version); err != nil {
		t.Fatal(err)
	}
	var title, content, hash, target, applicability, tags string
	if err := d.QueryRow(`SELECT title,content,content_hash,target,applicability,tags::text FROM memory_entries WHERE id=$1`, entry.ID).Scan(&title, &content, &hash, &target, &applicability, &tags); err != nil {
		t.Fatal(err)
	}
	if title+content+hash+target+applicability != "" || tags != "[]" {
		t.Fatal("deleted memory retained derived content")
	}
	var auditJSON string
	if err := d.QueryRow(`SELECT COALESCE(json_agg(a)::text,'[]') FROM memory_audit a WHERE entry_id=$1`, entry.ID).Scan(&auditJSON); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(auditJSON, "SHARED_MEMORY_FIXTURE") {
		t.Fatal("audit retained memory content")
	}
}

func TestMemoryExtractionSourceChangeAndSuppression(t *testing.T) {
	d := memoryFixtureDB(t)
	task := memoryFixtureTask(t, d)
	source := memoryFixtureSource(t, d, task)
	job := memoryQueueClaim(t, d, source)
	if queued, err := d.QueueMemoryExtraction(t.Context(), source); err != nil || queued {
		t.Fatalf("duplicate queue=%v %v", queued, err)
	}
	entries, err := d.CompleteMemoryJob(t.Context(), job, []MemoryInput{{Title: "learned fixture", Content: "learned bounded structured output", Kind: "procedure"}})
	if err != nil || len(entries) != 1 || !entries[0].Automatic {
		t.Fatalf("complete=%+v %v", entries, err)
	}
	if err := d.DeleteMemoryEntry(t.Context(), entries[0].ID, entries[0].Version); err != nil {
		t.Fatal(err)
	}
	current, _ := d.GetMemorySource(t.Context(), source.Kind, source.ID)
	if queued, err := d.QueueMemoryExtraction(t.Context(), *current); err != nil || queued {
		t.Fatalf("deleted source resurrected: %v %v", queued, err)
	}
	history, err := d.FindMemoryHistorySources(t.Context(), task.ID, 50)
	if err != nil {
		t.Fatal(err)
	}
	for _, item := range history {
		if item.ID == source.ID && item.Kind == source.Kind {
			t.Fatal("suppressed source re-entered discovery")
		}
	}
	// A separate in-flight source change invalidates its lease and late output.
	source = memoryFixtureSource(t, d, task)
	job = memoryQueueClaim(t, d, source)
	if err := d.Exploration(task.ExplorationID).SetNodeState(mustMemoryNodeID(t, source.ID), "dismissed"); err != nil {
		t.Fatal(err)
	}
	if _, err := d.CompleteMemoryJob(t.Context(), job, []MemoryInput{{Title: "late", Content: "late output", Kind: "lesson"}}); !errors.Is(err, ErrMemoryStale) {
		t.Fatalf("source-changed output admitted: %v", err)
	}
}

func mustMemoryNodeID(t *testing.T, id string) int64 {
	t.Helper()
	var nodeID int64
	if _, err := fmt.Sscan(id, &nodeID); err != nil {
		t.Fatal(err)
	}
	return nodeID
}

func TestMemoryDisableRejectsLateLearningAndRequest(t *testing.T) {
	d := memoryFixtureDB(t)
	task := memoryFixtureTask(t, d)
	source := memoryFixtureSource(t, d, task)
	job := memoryQueueClaim(t, d, source)
	if err := d.ValidateMemoryExtractionJob(t.Context(), job); err != nil {
		t.Fatal(err)
	}
	settings, _ := d.GetMemorySettings(t.Context())
	settings.Enabled = false
	if _, err := d.UpdateMemorySettings(t.Context(), settings, settings.Epoch); err != nil {
		t.Fatal(err)
	}
	if err := d.ValidateMemoryExtractionJob(t.Context(), job); !errors.Is(err, ErrMemoryStale) {
		t.Fatalf("disabled extraction still admitted HTTP attempt: %v", err)
	}
	if _, err := d.CompleteMemoryJob(t.Context(), job, []MemoryInput{{Title: "late", Content: "late output", Kind: "lesson"}}); !errors.Is(err, ErrMemoryStale) {
		t.Fatalf("disabled memory admitted late output: %v", err)
	}
	if claimed, err := d.ClaimMemoryJob(t.Context(), time.Minute); err != nil || claimed != nil {
		t.Fatalf("disabled learner claimed work: %+v %v", claimed, err)
	}
	var count int
	if err := d.QueryRow(`SELECT count(*) FROM memory_entries WHERE origin_task_id=$1`, task.ID).Scan(&count); err != nil || count != 0 {
		t.Fatalf("late entries=%d %v", count, err)
	}
}

func TestMemorySourceProjectionAndHistoryBoundary(t *testing.T) {
	d := memoryFixtureDB(t)
	task := memoryFixtureTask(t, d)
	nodeID, err := d.Exploration(task.ExplorationID).AddNode(KindFinding, map[string]any{
		"summary": "fixture finding summary", "evidence": "PRIVATE_RAW_PACKET_CANARY", "target": "fixture.invalid",
	}, 0, "confirmed", "worker", nil)
	if err != nil {
		t.Fatal(err)
	}
	source, err := d.GetMemorySource(t.Context(), "node", fmt.Sprint(nodeID))
	if err != nil || source == nil || strings.Contains(source.Text, "PRIVATE_RAW_PACKET_CANARY") || !strings.Contains(source.Text, "fixture.invalid") {
		t.Fatalf("source projection=%+v %v", source, err)
	}
	digestID, err := d.Exploration(task.ExplorationID).AddNode(KindDigest, map[string]any{"body": "derived digest without member invalidation"}, 0, "active", "compactor", nil)
	if err != nil {
		t.Fatal(err)
	}
	if digest, err := d.GetMemorySource(t.Context(), "node", fmt.Sprint(digestID)); err != nil || digest != nil {
		t.Fatalf("digest incorrectly admitted as independent evidence: %+v %v", digest, err)
	}
	if _, err := d.Exec(`UPDATE memory_settings SET learn_after=now()+interval '1 hour',epoch=epoch+1`); err != nil {
		t.Fatal(err)
	}
	incremental, err := d.FindMemorySources(t.Context(), 100)
	if err != nil {
		t.Fatal(err)
	}
	for _, item := range incremental {
		if item.ID == source.ID && item.Kind == source.Kind {
			t.Fatal("old source entered automatic backfill")
		}
	}
	history, err := d.FindMemoryHistorySources(t.Context(), task.ID, 50)
	if err != nil || len(history) != 1 || !history[0].Historical {
		t.Fatalf("explicit bounded history=%+v %v", history, err)
	}
	job := memoryQueueClaim(t, d, history[0])
	if _, err := d.CompleteMemoryJob(t.Context(), job, []MemoryInput{{Title: "unsupported target", Content: "historical note", Kind: "target", Target: "different.invalid"}}); !errors.Is(err, ErrMemoryInput) {
		t.Fatalf("invented target accepted: %v", err)
	}
	if _, err := d.CompleteMemoryJob(t.Context(), job, []MemoryInput{{Title: "source target", Content: "historical note, not a finding", Kind: "target", Target: "fixture.invalid"}}); err != nil {
		t.Fatal(err)
	}
}

func TestMemoryConversationDeletionInvalidatesDerivedContent(t *testing.T) {
	d := memoryFixtureDB(t)
	conversation, err := d.CreateConversation("chat", "memory fixture", nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = d.DeleteConversation(conversation.ID)
		for _, query := range []string{
			`DELETE FROM memory_audit WHERE conversation_id=$1`,
			`DELETE FROM memory_entries WHERE origin_conversation_id=$1`,
			`DELETE FROM memory_jobs WHERE source_ref IN (SELECT id FROM memory_sources WHERE origin_conversation_id=$1)`,
			`DELETE FROM memory_sources WHERE origin_conversation_id=$1`,
		} {
			if _, err := d.Exec(query, conversation.ID); err != nil {
				t.Errorf("clean conversation fixture: %v", err)
			}
		}
	})
	id, err := d.AppendConvActivity(conversation.ID, Activity{Worker: "chat", Kind: "text", Detail: "fixture final assistant response"})
	if err != nil {
		t.Fatal(err)
	}
	source, err := d.GetMemorySource(t.Context(), "conversation_activity", fmt.Sprint(id))
	if err != nil || source == nil {
		t.Fatalf("conversation source=%+v %v", source, err)
	}
	job := memoryQueueClaim(t, d, *source)
	entries, err := d.CompleteMemoryJob(t.Context(), job, []MemoryInput{{Title: "conversation lesson", Content: "fixture reusable conversation lesson", Kind: "lesson"}})
	if err != nil || len(entries) != 1 {
		t.Fatalf("conversation learning=%+v %v", entries, err)
	}
	if err := d.DeleteConversation(conversation.ID); err != nil {
		t.Fatal(err)
	}
	entry, err := d.GetMemoryEntry(context.Background(), entries[0].ID)
	if err != nil || entry.State != "source_invalid" || entry.Content != "" || entry.Title != "" {
		t.Fatalf("deleted conversation retained active memory: %+v %v", entry, err)
	}
}

func TestMemoryEffectiveExpiryAndMultipleDerivedDeletion(t *testing.T) {
	d := memoryFixtureDB(t)
	task := memoryFixtureTask(t, d)
	expired := time.Now().UTC().Add(-time.Hour)
	entry, err := d.CreateManualMemory(t.Context(), MemoryInput{Title: "expired fixture", Content: "expired fixture content", Kind: "lesson", OriginTaskID: task.ID, ExpiresAt: &expired})
	if err != nil || entry.State != "expired" {
		t.Fatalf("effective expiry=%+v %v", entry, err)
	}
	items, total, err := d.ListMemoryEntries(t.Context(), MemoryFilter{State: "expired", TaskID: task.ID}, 0, 30)
	if err != nil || total != 1 || len(items) != 1 || items[0].State != "expired" {
		t.Fatalf("expired filter=%+v %d %v", items, total, err)
	}
	if active, _, err := d.ListMemoryEntries(t.Context(), MemoryFilter{State: "active", TaskID: task.ID}, 0, 30); err != nil || len(active) != 0 {
		t.Fatalf("expired note listed as active: %+v %v", active, err)
	}
	source := memoryFixtureSource(t, d, task)
	job := memoryQueueClaim(t, d, source)
	entries, err := d.CompleteMemoryJob(t.Context(), job, []MemoryInput{
		{Title: "first derived", Content: "first independent fixture method", Kind: "procedure"},
		{Title: "second derived", Content: "second independent fixture method", Kind: "procedure"},
	})
	if err != nil || len(entries) != 2 {
		t.Fatalf("multiple derived entries=%+v %v", entries, err)
	}
	// Clearing several distinct hashes to empty must not collide with the
	// automatic-entry identity index during the source's DELETE trigger.
	if _, err := d.Exec(`DELETE FROM exploration_nodes WHERE id=$1`, mustMemoryNodeID(t, source.ID)); err != nil {
		t.Fatal(err)
	}
	for _, old := range entries {
		current, err := d.GetMemoryEntry(t.Context(), old.ID)
		if err != nil || current.State != "source_invalid" || current.Content != "" {
			t.Fatalf("source delete invalidation=%+v %v", current, err)
		}
	}
}

func TestMemoryQueueHardBacklogAndAttemptQuota(t *testing.T) {
	d := memoryFixtureDB(t)
	task := memoryFixtureTask(t, d)
	source := memoryFixtureSource(t, d, task)
	queued, err := d.QueueMemoryExtraction(t.Context(), source)
	if err != nil || !queued {
		t.Fatalf("queue fixture=%v %v", queued, err)
	}
	var refID int64
	if err := d.QueryRow(`SELECT id FROM memory_sources WHERE source_kind=$1 AND source_id=$2`, source.Kind, source.ID).Scan(&refID); err != nil {
		t.Fatal(err)
	}
	settings, _ := d.GetMemorySettings(t.Context())
	if _, err := d.Exec(`INSERT INTO memory_jobs(source_ref,source_version,source_fingerprint,settings_epoch)
		SELECT $1,0,'backlog-fixture-'||generate_series::text,$2 FROM generate_series(1,199)`, refID, settings.Epoch); err != nil {
		t.Fatal(err)
	}
	other := memoryFixtureSource(t, d, task)
	if queued, err := d.QueueMemoryExtraction(t.Context(), other); err != nil || queued {
		t.Fatalf("backlog exceeded hard cap: %v %v", queued, err)
	}
	var exists bool
	if err := d.QueryRow(`SELECT EXISTS(SELECT 1 FROM memory_sources WHERE source_kind=$1 AND source_id=$2)`, other.Kind, other.ID).Scan(&exists); err != nil || exists {
		t.Fatalf("full backlog persisted new source: %v %v", exists, err)
	}
	if _, err := d.Exec(`INSERT INTO memory_audit(task_id,action)
		SELECT $1,'extract_start' FROM generate_series(1,60)`, task.ID); err != nil {
		t.Fatal(err)
	}
	if job, err := d.ClaimMemoryJob(t.Context(), time.Minute); err != nil || job != nil {
		t.Fatalf("hourly quota ignored: %+v %v", job, err)
	}
}

func TestMemoryPruningPreservesSourceSuppression(t *testing.T) {
	d := memoryFixtureDB(t)
	task := memoryFixtureTask(t, d)
	source := memoryFixtureSource(t, d, task)
	job := memoryQueueClaim(t, d, source)
	entries, err := d.CompleteMemoryJob(t.Context(), job, []MemoryInput{{Title: "delete fixture", Content: "must not be relearned", Kind: "lesson"}})
	if err != nil || len(entries) != 1 {
		t.Fatalf("complete=%+v %v", entries, err)
	}
	if err := d.DeleteMemoryEntry(t.Context(), entries[0].ID, entries[0].Version); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Exec(`UPDATE memory_jobs SET updated_at=now()-interval '400 days' WHERE id=$1`, job.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Exec(`INSERT INTO memory_audit(task_id,action,created_at) VALUES($1,'created',now()-interval '400 days')`, task.ID); err != nil {
		t.Fatal(err)
	}
	if err := d.PruneMemory(t.Context()); err != nil {
		t.Fatal(err)
	}
	var oldAudit, oldJob int
	if err := d.QueryRow(`SELECT count(*) FROM memory_audit WHERE task_id=$1 AND created_at<now()-interval '90 days'`, task.ID).Scan(&oldAudit); err != nil {
		t.Fatal(err)
	}
	if err := d.QueryRow(`SELECT count(*) FROM memory_jobs WHERE id=$1`, job.ID).Scan(&oldJob); err != nil {
		t.Fatal(err)
	}
	if oldAudit != 0 || oldJob != 0 {
		t.Fatalf("retention cleanup=%d %d", oldAudit, oldJob)
	}
	current, err := d.GetMemorySource(t.Context(), source.Kind, source.ID)
	if err != nil || current == nil {
		t.Fatalf("source=%+v %v", current, err)
	}
	if queued, err := d.QueueMemoryExtraction(t.Context(), *current); err != nil || queued {
		t.Fatalf("pruning resurrected deleted source: %v %v", queued, err)
	}
}
