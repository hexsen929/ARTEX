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
	if err := d.SetStatus(task.ID, "running"); err != nil {
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
	intent, _ := memoryFixtureIntent(t, d, task, "running", 1)
	return memoryFinishStage(t, d, task, intent, "done")
}

// A stage uses native graph attribution, not the model's interpretation of a
// title or an assistant's terminal response. The task brief remains a canary.
func memoryFixtureIntent(t *testing.T, d *DB, task *Task, state string, count int) (int64, []int64) {
	t.Helper()
	store := d.Exploration(task.ExplorationID)
	intent, err := store.AddNode(KindIntent, map[string]any{"summary": "PRIVATE_TASK_BRIEF_CANARY; task instructions are not observed experience"}, 0, state, "planner", nil)
	if err != nil {
		t.Fatal(err)
	}
	facts := make([]int64, 0, count)
	for i := 0; i < count; i++ {
		fact, err := store.AddNode(KindFact, map[string]any{"summary": fmt.Sprintf("fixture tool supports bounded structured output, observation %d", i), "confidence": "observed", "tool": "fixture-tool", "tool_version": "v1.2", "platform": "fixture-os", "evidence": "PRIVATE_RAW_PACKET_CANARY"}, 0, "confirmed", "worker", nil)
		if err != nil {
			t.Fatal(err)
		}
		if err := store.Link(intent, RelYields, fact); err != nil {
			t.Fatal(err)
		}
		facts = append(facts, fact)
	}
	return intent, facts
}

func memoryFinishStage(t *testing.T, d *DB, task *Task, intent int64, state string) MemorySource {
	t.Helper()
	if err := d.Exploration(task.ExplorationID).SetIntentState(intent, state); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Exec(`UPDATE exploration_nodes SET completed_at=now()-interval '31 seconds' WHERE id=$1`, intent); err != nil {
		t.Fatal(err)
	}
	source, err := d.GetMemorySource(t.Context(), "intent_stage", fmt.Sprint(intent))
	if err != nil || source == nil {
		t.Fatalf("stage source=%+v %v", source, err)
	}
	source.Historical = true // fixture is explicitly opting into the bounded history path
	return *source
}

type memoryStageProjection struct {
	Stage struct {
		Kind            string `json:"kind"`
		ID              string `json:"id"`
		State           string `json:"state"`
		TerminationCode string `json:"termination_code"`
		CompletedAt     string `json:"completed_at"`
	} `json:"stage"`
	Coverage struct {
		Included int  `json:"included"`
		Total    int  `json:"total"`
		Partial  bool `json:"partial"`
	} `json:"coverage"`
	Facts []struct {
		ID         string   `json:"id"`
		Version    int64    `json:"version"`
		Confidence string   `json:"confidence"`
		Summary    string   `json:"summary"`
		Target     string   `json:"target"`
		Conditions []string `json:"conditions"`
	} `json:"facts"`
}

func memoryStage(t *testing.T, source MemorySource) memoryStageProjection {
	t.Helper()
	var out memoryStageProjection
	if err := json.Unmarshal([]byte(source.Text), &out); err != nil || len(out.Facts) == 0 {
		t.Fatalf("invalid stage projection: %s %v", source.Text, err)
	}
	if out.Stage.Kind != source.Kind || out.Stage.ID != source.ID {
		t.Fatalf("stage attribution changed: %+v %+v", out.Stage, source)
	}
	return out
}

func memorySourceFactID(t *testing.T, source MemorySource) int64 {
	t.Helper()
	return mustMemoryNodeID(t, memoryStage(t, source).Facts[0].ID)
}

func memoryComplete(t *testing.T, d *DB, job MemoryJob, inputs []MemoryInput) ([]MemoryEntry, error) {
	t.Helper()
	if len(inputs) > 0 {
		facts := memoryStage(t, job.Source).Facts
		for i := range inputs {
			if inputs[i].EvidenceIDs == nil {
				inputs[i].EvidenceIDs = []string{facts[0].ID}
			}
			if inputs[i].Applicability == "" {
				inputs[i].Applicability = strings.Join(facts[0].Conditions, "; ")
				if inputs[i].Applicability == "" {
					inputs[i].Applicability = "来源阶段限定，版本/环境未完整登记，使用前复核"
				}
			}
		}
	}
	return d.CompleteMemoryJob(t.Context(), job, inputs)
}

func memoryEnableIncrementalHistory(t *testing.T, d *DB) {
	t.Helper()
	if _, err := d.Exec(`UPDATE memory_settings SET learn_after=now()-interval '2 hours',epoch=epoch+1`); err != nil {
		t.Fatal(err)
	}
}

func memoryContainsSource(sources []MemorySource, want MemorySource) bool {
	for _, source := range sources {
		if source.Kind == want.Kind && source.ID == want.ID && source.Version == want.Version {
			return true
		}
	}
	return false
}

func memoryFixtureConversation(t *testing.T, d *DB) *Conversation {
	t.Helper()
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
	return conversation
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
	automatic := base
	automatic.EvidenceIDs = []string{"1"}
	if _, err := validateMemoryInput(automatic, true); err != nil {
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
	entries, err := memoryComplete(t, d, job, []MemoryInput{{Title: "learned fixture", Content: "learned bounded structured output", Kind: "procedure"}})
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
	if err := d.Exploration(task.ExplorationID).SetNodeState(memorySourceFactID(t, source), "dismissed"); err != nil {
		t.Fatal(err)
	}
	if _, err := memoryComplete(t, d, job, []MemoryInput{{Title: "late", Content: "late output", Kind: "lesson"}}); !errors.Is(err, ErrMemoryStale) {
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
	if _, err := memoryComplete(t, d, job, []MemoryInput{{Title: "late", Content: "late output", Kind: "lesson"}}); !errors.Is(err, ErrMemoryStale) {
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
	store := d.Exploration(task.ExplorationID)
	intent, facts := memoryFixtureIntent(t, d, task, "running", 1)
	// Neither running work nor freshly finished settlement is a learning phase.
	if source, err := d.GetMemorySource(t.Context(), "intent_stage", fmt.Sprint(intent)); err != nil || source != nil {
		t.Fatalf("running phase entered learning: %+v %v", source, err)
	}
	if err := store.SetIntentOutcome(intent, "done", "completed", "PRIVATE_END_REASON_CANARY"); err != nil {
		t.Fatal(err)
	}
	if source, err := d.GetMemorySource(t.Context(), "intent_stage", fmt.Sprint(intent)); err != nil || source != nil {
		t.Fatalf("unsettled phase entered learning: %+v %v", source, err)
	}
	if _, err := d.Exec(`UPDATE exploration_nodes SET completed_at=now()-interval '31 seconds' WHERE id=$1`, intent); err != nil {
		t.Fatal(err)
	}
	source, err := d.GetMemorySource(t.Context(), "intent_stage", fmt.Sprint(intent))
	if err != nil || source == nil {
		t.Fatalf("stage source=%+v %v", source, err)
	}
	projection := memoryStage(t, *source)
	if len(projection.Facts) != 1 || projection.Facts[0].ID != fmt.Sprint(facts[0]) || projection.Facts[0].Confidence != "observed" || projection.Stage.TerminationCode != "completed" {
		t.Fatalf("native attribution/confidence=%+v", projection)
	}
	for _, canary := range []string{"PRIVATE_TASK_BRIEF_CANARY", "PRIVATE_RAW_PACKET_CANARY", "PRIVATE_END_REASON_CANARY"} {
		if strings.Contains(source.Text, canary) {
			t.Fatalf("non-allowlisted source retained %s", canary)
		}
	}
	digestID, err := store.AddNode(KindDigest, map[string]any{"body": "derived digest without member lineage"}, 0, "active", "compactor", nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, legacy := range []MemorySource{{Kind: "node", ID: fmt.Sprint(facts[0])}, {Kind: "node", ID: fmt.Sprint(digestID)}, {Kind: "web", ID: "fixture.invalid"}} {
		settings, _ := d.GetMemorySettings(t.Context())
		legacy.SettingsEpoch = settings.Epoch
		if queued, err := d.QueueMemoryExtraction(t.Context(), legacy); !errors.Is(err, ErrMemoryInput) || queued {
			t.Fatalf("raw record queued instead of phase: %s %v %v", legacy.Kind, queued, err)
		}
	}
	if _, err := d.Exec(`UPDATE memory_settings SET learn_after=now()+interval '1 hour',epoch=epoch+1`); err != nil {
		t.Fatal(err)
	}
	incremental, err := d.FindMemorySources(t.Context(), 100)
	if err != nil {
		t.Fatal(err)
	}
	if memoryContainsSource(incremental, *source) {
		t.Fatal("old phase entered incremental history")
	}
	history, err := d.FindMemoryHistorySources(t.Context(), task.ID, 50)
	if err != nil || len(history) != 1 || !history[0].Historical {
		t.Fatalf("explicit bounded phase history=%+v %v", history, err)
	}
	job := memoryQueueClaim(t, d, history[0])
	for _, input := range []MemoryInput{
		{Title: "target cannot be shared", Content: "historical fixture result", Kind: "target", Target: "fixture.invalid"},
		{Title: "kind does not launder targets", Content: "historical fixture result", Kind: "lesson", Target: "fixture.invalid"},
		{Title: "invented evidence", Content: "unsupported fixture method", Kind: "procedure", EvidenceIDs: []string{"999999999"}},
	} {
		if _, err := memoryComplete(t, d, job, []MemoryInput{input}); !errors.Is(err, ErrMemoryInput) {
			t.Fatalf("invalid automatic output admitted: %+v %v", input, err)
		}
	}
	entries, err := memoryComplete(t, d, job, []MemoryInput{{Title: "bounded tool compatibility", Content: "Use bounded structured output in the recorded fixture environment", Kind: "environment"}})
	if err != nil || len(entries) != 1 || entries[0].ShareScope != "global_experience" || len(entries[0].EvidenceIDs) != 1 {
		t.Fatalf("bounded phase learning=%+v %v", entries, err)
	}
}

func TestMemoryConversationDeletionInvalidatesDerivedContent(t *testing.T) {
	d := memoryFixtureDB(t)
	conversation := memoryFixtureConversation(t, d)
	// Human-created notes may retain a source conversation; ordinary conversation
	// content itself no longer becomes an automatic experience source.
	entry, err := d.CreateManualMemory(t.Context(), MemoryInput{Title: "conversation note", Content: "fixture reusable human-authored note", Kind: "lesson", OriginConversationID: conversation.ID, SessionID: fmt.Sprintf("conv-%d", conversation.ID)})
	if err != nil {
		t.Fatal(err)
	}
	if err := d.DeleteConversation(conversation.ID); err != nil {
		t.Fatal(err)
	}
	stored, err := d.GetMemoryEntry(context.Background(), entry.ID)
	if err != nil || stored.State != "source_invalid" || stored.Content != "" || stored.Title != "" {
		t.Fatalf("deleted conversation retained note: %+v %v", stored, err)
	}
}

func TestMemoryAssistantEchoAndUnknownUserAreNotSources(t *testing.T) {
	d := memoryFixtureDB(t)
	conversation := memoryFixtureConversation(t, d)
	for _, kind := range []string{"user", "text", "result"} {
		id, err := d.AppendConvActivity(conversation.ID, Activity{Worker: "chat", Kind: kind, Detail: "HUMAN_SOURCE_FIXTURE repeated bounded output lesson"})
		if err != nil {
			t.Fatal(err)
		}
		if kind == "user" {
			if err := d.MarkMemoryHumanActivity(t.Context(), id); err != nil {
				t.Fatal(err)
			}
		}
		settings, _ := d.GetMemorySettings(t.Context())
		candidate := MemorySource{Kind: "conversation_activity", ID: fmt.Sprint(id), SettingsEpoch: settings.Epoch, Historical: true}
		if queued, err := d.QueueMemoryExtraction(t.Context(), candidate); !errors.Is(err, ErrMemoryInput) || queued {
			t.Fatalf("chat/%s bypassed phase-only extraction: %v %v", kind, queued, err)
		}
	}
	history, err := d.FindMemoryHistorySources(t.Context(), 0, 50)
	if err != nil {
		t.Fatal(err)
	}
	for _, source := range history {
		if source.Kind != "intent_stage" && source.Kind != "task_stage" {
			t.Fatalf("non-phase source in history: %+v", source)
		}
	}
}

func TestMemoryHumanOriginRevocationInvalidatesLearning(t *testing.T) {
	d := memoryFixtureDB(t)
	conversation := memoryFixtureConversation(t, d)
	id, err := d.AppendConvActivity(conversation.ID, Activity{Worker: "chat", Kind: "user", Detail: "fixture human message is not a completed task phase"})
	if err != nil {
		t.Fatal(err)
	}
	if err := d.MarkMemoryHumanActivity(t.Context(), id); err != nil {
		t.Fatal(err)
	}
	for _, origin := range []string{"human", ""} {
		if _, err := d.Exec(`UPDATE conversation_activities SET memory_input_origin=$2 WHERE id=$1`, id, origin); err != nil {
			t.Fatal(err)
		}
		settings, _ := d.GetMemorySettings(t.Context())
		candidate := MemorySource{Kind: "conversation_activity", ID: fmt.Sprint(id), SettingsEpoch: settings.Epoch}
		if queued, err := d.QueueMemoryExtraction(t.Context(), candidate); !errors.Is(err, ErrMemoryInput) || queued {
			t.Fatalf("human marker changed phase boundary: %q %v %v", origin, queued, err)
		}
	}
	var jobs int
	if err := d.QueryRow(`SELECT count(*) FROM memory_jobs job JOIN memory_sources source ON source.id=job.source_ref WHERE source.origin_conversation_id=$1`, conversation.ID).Scan(&jobs); err != nil || jobs != 0 {
		t.Fatalf("conversation created model work: %d %v", jobs, err)
	}
}

func TestMemoryExtractionActualAttemptBudget(t *testing.T) {
	d := memoryFixtureDB(t)
	settings, _ := d.GetMemorySettings(t.Context())
	settings.MaxJobsPerHour = 2
	if _, err := d.UpdateMemorySettings(t.Context(), settings, settings.Epoch); err != nil {
		t.Fatal(err)
	}
	task := memoryFixtureTask(t, d)
	source := memoryFixtureSource(t, d, task)
	job := memoryQueueClaim(t, d, source)
	results := make(chan error, 8)
	for range 8 {
		go func() { results <- d.ValidateMemoryExtractionJob(t.Context(), job) }()
	}
	admitted, denied := 0, 0
	for range 8 {
		err := <-results
		switch {
		case err == nil:
			admitted++
		case errors.Is(err, ErrMemoryBudget):
			denied++
		default:
			t.Fatalf("unexpected attempt admission error: %v", err)
		}
	}
	if admitted != 2 || denied != 6 {
		t.Fatalf("non-atomic actual-attempt quota: admitted=%d denied=%d", admitted, denied)
	}
	other := memoryFixtureSource(t, d, task)
	if queued, err := d.QueueMemoryExtraction(t.Context(), other); err != nil || !queued {
		t.Fatalf("queue while attempt budget full=%v %v", queued, err)
	}
	if next, err := d.ClaimMemoryJob(t.Context(), time.Minute); err != nil || next != nil {
		t.Fatalf("budget-exhausted claim consumed an attempt: %+v %v", next, err)
	}
	var attempts int
	var state string
	if err := d.QueryRow(`SELECT job.attempts,job.state FROM memory_jobs job JOIN memory_sources source ON source.id=job.source_ref
		WHERE source.source_kind=$1 AND source.source_id=$2`, other.Kind, other.ID).Scan(&attempts, &state); err != nil {
		t.Fatal(err)
	}
	if attempts != 0 || state != "queued" {
		t.Fatalf("waiting job burned retries: attempts=%d state=%s", attempts, state)
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
	entries, err := memoryComplete(t, d, job, []MemoryInput{
		{Title: "first derived", Content: "first independent fixture method", Kind: "procedure"},
		{Title: "second derived", Content: "second independent fixture method", Kind: "procedure"},
	})
	if err != nil || len(entries) != 2 {
		t.Fatalf("multiple derived entries=%+v %v", entries, err)
	}
	// Clearing several distinct hashes to empty must not collide with the
	// automatic-entry identity index during the source's DELETE trigger.
	if _, err := d.Exec(`DELETE FROM exploration_nodes WHERE id=$1`, memorySourceFactID(t, source)); err != nil {
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
		SELECT $1,generate_series,'backlog-fixture-'||generate_series::text,$2 FROM generate_series(10001,10199)`, refID, settings.Epoch); err != nil {
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
	entries, err := memoryComplete(t, d, job, []MemoryInput{{Title: "delete fixture", Content: "must not be relearned", Kind: "lesson"}})
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

func TestMemoryTargetRecordsRemainInOriginTask(t *testing.T) {
	d := memoryFixtureDB(t)
	a, b := memoryFixtureTask(t, d), memoryFixtureTask(t, d)
	marker := "TARGET_SCOPE_FIXTURE"
	for _, input := range []MemoryInput{
		{Title: "missing task", Content: marker, Kind: "target", Target: "fixture.invalid"},
		{Title: "missing target", Content: marker, Kind: "target", OriginTaskID: a.ID},
		{Title: "unknown task", Content: marker, Kind: "target", Target: "fixture.invalid", OriginTaskID: 9223372036854775807},
		{Title: "category does not launder target", Content: marker, Kind: "procedure", Target: "fixture.invalid", OriginTaskID: a.ID},
	} {
		if _, err := d.CreateManualMemory(t.Context(), input); !errors.Is(err, ErrMemoryInput) {
			t.Fatalf("invalid target origin admitted: %+v %v", input, err)
		}
	}
	input := MemoryInput{Title: "source-specific historical result", Content: marker, Kind: "target", Target: "fixture.invalid", OriginTaskID: a.ID}
	entry, err := d.CreateManualMemory(t.Context(), input)
	if err != nil || entry.ShareScope != "source_only" {
		t.Fatalf("target scope=%+v %v", entry, err)
	}
	for _, shared := range []bool{true, false} {
		settings, err := d.GetMemorySettings(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		settings.GlobalShare = shared
		settings, err = d.UpdateMemorySettings(t.Context(), settings, settings.Epoch)
		if err != nil {
			t.Fatal(err)
		}
		own, _, err := d.SearchMemory(t.Context(), MemorySearchInput{Query: marker, TaskID: a.ID, AgentKey: "worker"})
		if err != nil || len(own) != 1 || own[0].ID != entry.ID {
			t.Fatalf("source task lost target note with sharing=%v: %+v %v", shared, own, err)
		}
		foreign, _, err := d.SearchMemory(t.Context(), MemorySearchInput{Query: marker, TaskID: b.ID, AgentKey: "worker"})
		if err != nil || len(foreign) != 0 {
			t.Fatalf("foreign task received target note with sharing=%v: %+v %v", shared, foreign, err)
		}
		admission := MemoryAdmission{SettingsEpoch: settings.Epoch, References: []MemoryRef{{ID: entry.ID, Version: entry.Version}}, TaskID: b.ID, AgentKey: "worker", SessionID: "scope-fixture", CallKey: "foreign-target"}
		if err := d.ValidateMemoryAdmission(t.Context(), admission); !errors.Is(err, ErrMemoryStale) {
			t.Fatalf("direct ref bypassed target isolation: %v", err)
		}
		admission.TaskID = a.ID
		admission.CallKey = "own-target"
		if err := d.ValidateMemoryAdmission(t.Context(), admission); err != nil {
			t.Fatalf("own target admission=%v", err)
		}
	}
	// Sharing is an explicit category change by the administrator, never inferred
	// merely from changing the global toggle. Origin metadata remains intact.
	input.Kind, input.Target = "lesson", ""
	edited, err := d.EditMemoryEntry(t.Context(), entry.ID, input, entry.Version)
	if err != nil || edited.ShareScope != "global_experience" || edited.OriginTaskID != a.ID {
		t.Fatalf("explicit generalization=%+v %v", edited, err)
	}
	settings, _ := d.GetMemorySettings(t.Context())
	settings.GlobalShare = true
	if _, err := d.UpdateMemorySettings(t.Context(), settings, settings.Epoch); err != nil {
		t.Fatal(err)
	}
	foreign, _, err := d.SearchMemory(t.Context(), MemorySearchInput{Query: marker, TaskID: b.ID, AgentKey: "worker"})
	if err != nil || len(foreign) != 1 || foreign[0].ID != edited.ID {
		t.Fatalf("explicit reusable experience not shared: %+v %v", foreign, err)
	}
}

func TestMemoryStageBundleBoundedAndUsesNativeConfidence(t *testing.T) {
	d := memoryFixtureDB(t)
	task := memoryFixtureTask(t, d)
	intent, facts := memoryFixtureIntent(t, d, task, "running", 12)
	store := d.Exploration(task.ExplorationID)
	for i, id := range facts {
		confidence := "observed"
		if i%2 == 0 {
			confidence = "inferred"
		}
		if _, err := d.Exec(`UPDATE exploration_nodes SET payload=jsonb_set(payload,'{confidence}',to_jsonb($2::text)),content_version=content_version+1 WHERE id=$1`, id, confidence); err != nil {
			t.Fatal(err)
		}
	}
	for _, payload := range []map[string]any{
		{"summary": strings.Repeat("complete qualification ", 40), "confidence": "observed"},
		{"summary": " \n\t", "confidence": "observed"},
		{"summary": map[string]any{"not": "plain text"}, "confidence": "observed"},
	} {
		id, err := store.AddNode(KindFact, payload, 0, "confirmed", "worker", nil)
		if err != nil {
			t.Fatal(err)
		}
		if err := store.Link(intent, RelYields, id); err != nil {
			t.Fatal(err)
		}
	}
	source := memoryFinishStage(t, d, task, intent, "done")
	projection := memoryStage(t, source)
	if len(projection.Facts) != 8 || projection.Coverage.Included != 8 || projection.Coverage.Total < 12 || !projection.Coverage.Partial {
		t.Fatalf("unbounded or misreported stage coverage: %+v", projection)
	}
	selected := map[string]bool{}
	inferred := false
	for _, fact := range projection.Facts {
		selected[fact.ID] = true
		if len(fact.Summary) > 600 || strings.TrimSpace(fact.Summary) == "" {
			t.Fatalf("partial/empty summary admitted: %+v", fact)
		}
		if fact.Confidence == "inferred" {
			inferred = true
		}
		if strings.Contains(fact.Summary, "PRIVATE") {
			t.Fatal("private evidence entered a stage summary")
		}
	}
	if !inferred {
		t.Fatal("native confidence was replaced by fact node confirmed state")
	}
	// Even an unselected member participates in the stage's freshness contract.
	missing := int64(0)
	for _, id := range facts {
		if !selected[fmt.Sprint(id)] {
			missing = id
			break
		}
	}
	if missing == 0 {
		t.Fatal("bounded fixture has no excluded member")
	}
	job := memoryQueueClaim(t, d, source)
	if _, err := d.Exec(`UPDATE exploration_nodes SET payload=jsonb_set(payload,'{confidence}','"confirmed"'::jsonb),content_version=content_version+1 WHERE id=$1`, missing); err != nil {
		t.Fatal(err)
	}
	updated, err := d.GetMemorySource(t.Context(), source.Kind, source.ID)
	if err != nil || updated == nil || updated.Fingerprint == source.Fingerprint || updated.Version != source.Version {
		t.Fatalf("unselected member not covered: %+v %v", updated, err)
	}
	if err := d.ValidateMemoryExtractionJob(t.Context(), job); !errors.Is(err, ErrMemoryStale) {
		t.Fatalf("unselected member change admitted stale bytes: %v", err)
	}
}

func TestMemoryCompletedStageChangesInvalidateWithoutAutomaticResummary(t *testing.T) {
	d := memoryFixtureDB(t)
	memoryEnableIncrementalHistory(t, d)
	task := memoryFixtureTask(t, d)
	intent, facts := memoryFixtureIntent(t, d, task, "running", 2)
	source := memoryFinishStage(t, d, task, intent, "done")
	source.Historical = false
	job := memoryQueueClaim(t, d, source)
	entries, err := memoryComplete(t, d, job, []MemoryInput{{Title: "phase-specific compatibility", Content: "conditional phase tool compatibility", Kind: "environment"}})
	if err != nil || len(entries) != 1 {
		t.Fatalf("phase completion=%+v %v", entries, err)
	}
	checkNoAutomatic := func(want MemorySource) {
		t.Helper()
		candidates, err := d.FindMemorySources(t.Context(), 100)
		if err != nil || memoryContainsSource(candidates, want) {
			t.Fatalf("completed native generation automatically repeated: %+v %v", candidates, err)
		}
		current, err := d.GetMemorySource(t.Context(), want.Kind, want.ID)
		if err != nil || current == nil {
			t.Fatalf("get completed phase=%+v %v", current, err)
		}
		if queued, err := d.QueueMemoryExtraction(t.Context(), *current); err != nil || queued {
			t.Fatalf("direct automatic queue repeated a completed phase: %v %v", queued, err)
		}
	}
	checkNoAutomatic(source)
	settings, _ := d.GetMemorySettings(t.Context())
	settings.MaxEntries = 4
	if _, err := d.UpdateMemorySettings(t.Context(), settings, settings.Epoch); err != nil {
		t.Fatal(err)
	}
	checkNoAutomatic(source)
	if _, err := d.Exec(`UPDATE exploration_nodes SET payload=jsonb_set(payload,'{summary}','"updated native compatibility conditions"'::jsonb),content_version=content_version+1 WHERE id=$1`, facts[0]); err != nil {
		t.Fatal(err)
	}
	old, err := d.GetMemoryEntry(t.Context(), entries[0].ID)
	if err != nil || old.State != "source_invalid" {
		t.Fatalf("changed member retained global experience: %+v %v", old, err)
	}
	changed, err := d.GetMemorySource(t.Context(), source.Kind, source.ID)
	if err != nil || changed == nil || changed.Fingerprint == source.Fingerprint || changed.Version != source.Version {
		t.Fatalf("content change manufactured new native generation: %+v %v", changed, err)
	}
	checkNoAutomatic(*changed)
	late, err := d.Exploration(task.ExplorationID).AddNode(KindFact, map[string]any{"summary": "later native fixture observation", "confidence": "observed"}, 0, "confirmed", "worker", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := d.Exploration(task.ExplorationID).Link(intent, RelYields, late); err != nil {
		t.Fatal(err)
	}
	checkNoAutomatic(*changed)
	// Only an explicit bounded history request may re-summarize the changed phase.
	history, err := d.FindMemoryHistorySources(t.Context(), task.ID, 50)
	if err != nil || len(history) != 1 || !history[0].Historical {
		t.Fatalf("changed phase not available for explicit history: %+v %v", history, err)
	}
	retry := memoryQueueClaim(t, d, history[0])
	revised, err := memoryComplete(t, d, retry, []MemoryInput{{Title: "revised phase compatibility", Content: "revised conditional compatibility in the same phase", Kind: "lesson"}})
	if err != nil || len(revised) != 1 {
		t.Fatalf("explicit phase resummary=%+v %v", revised, err)
	}
	checkNoAutomatic(history[0])
}

func TestMemoryNativeGenerationAndNoExperienceSurvivePruning(t *testing.T) {
	d := memoryFixtureDB(t)
	memoryEnableIncrementalHistory(t, d)
	task := memoryFixtureTask(t, d)
	intent, _ := memoryFixtureIntent(t, d, task, "running", 1)
	source := memoryFinishStage(t, d, task, intent, "blocked")
	source.Historical = false
	job := memoryQueueClaim(t, d, source)
	entries, err := memoryComplete(t, d, job, nil)
	if err != nil || len(entries) != 0 {
		t.Fatalf("no-experience completion=%+v %v", entries, err)
	}
	if _, err := d.Exec(`UPDATE memory_jobs SET updated_at=now()-interval '400 days' WHERE id=$1`, job.ID); err != nil {
		t.Fatal(err)
	}
	if err := d.PruneMemory(t.Context()); err != nil {
		t.Fatal(err)
	}
	var remaining int
	if err := d.QueryRow(`SELECT count(*) FROM memory_jobs WHERE id=$1`, job.ID).Scan(&remaining); err != nil || remaining != 0 {
		t.Fatalf("old job not pruned: %d %v", remaining, err)
	}
	settings, _ := d.GetMemorySettings(t.Context())
	settings.MaxEntries = 4
	if _, err := d.UpdateMemorySettings(t.Context(), settings, settings.Epoch); err != nil {
		t.Fatal(err)
	}
	candidates, err := d.FindMemorySources(t.Context(), 100)
	if err != nil || memoryContainsSource(candidates, source) {
		t.Fatalf("job pruning/settings change resurrected no-experience phase: %+v %v", candidates, err)
	}
	current, err := d.GetMemorySource(t.Context(), source.Kind, source.ID)
	if err != nil || current == nil {
		t.Fatalf("phase disappeared after job pruning: %+v %v", current, err)
	}
	if queued, err := d.QueueMemoryExtraction(t.Context(), *current); err != nil || queued {
		t.Fatalf("phase no-experience marker lost: %v %v", queued, err)
	}
	if reopened, err := d.Exploration(task.ExplorationID).ReopenIntent(intent); err != nil || !reopened {
		t.Fatalf("native intent reopen=%v %v", reopened, err)
	}
	if open, err := d.GetMemorySource(t.Context(), source.Kind, source.ID); err != nil || open != nil {
		t.Fatalf("reopened work remained completed: %+v %v", open, err)
	}
	next := memoryFinishStage(t, d, task, intent, "done")
	next.Historical = false
	if next.Version <= source.Version {
		t.Fatalf("native completion generation did not advance: %d -> %d", source.Version, next.Version)
	}
	candidates, err = d.FindMemorySources(t.Context(), 100)
	if err != nil || !memoryContainsSource(candidates, next) {
		t.Fatalf("new native generation not discovered: %+v %v", candidates, err)
	}
	nextJob := memoryQueueClaim(t, d, next)
	if _, err := memoryComplete(t, d, nextJob, nil); err != nil {
		t.Fatalf("new generation not independently learnable: %v", err)
	}
}

func TestMemoryStageEdgeRemovalAndReopenInvalidateAdmission(t *testing.T) {
	d := memoryFixtureDB(t)
	task := memoryFixtureTask(t, d)
	intent, facts := memoryFixtureIntent(t, d, task, "running", 2)
	source := memoryFinishStage(t, d, task, intent, "done")
	job := memoryQueueClaim(t, d, source)
	entries, err := memoryComplete(t, d, job, []MemoryInput{{Title: "edge-bound lesson", Content: "conditional fixture lesson", Kind: "lesson"}})
	if err != nil || len(entries) != 1 {
		t.Fatalf("complete=%+v %v", entries, err)
	}
	settings, _ := d.GetMemorySettings(t.Context())
	admission := MemoryAdmission{SettingsEpoch: settings.Epoch, References: []MemoryRef{{ID: entries[0].ID, Version: entries[0].Version}}, TaskID: task.ID, AgentKey: "worker", SessionID: "fixture-stage", CallKey: "stage-ref"}
	if err := d.ValidateMemoryAdmission(t.Context(), admission); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Exec(`DELETE FROM exploration_edges WHERE exploration_id=$1 AND src_id=$2 AND rel=$3 AND dst_id=$4`, task.ExplorationID, intent, RelYields, facts[0]); err != nil {
		t.Fatal(err)
	}
	stale, err := d.GetMemoryEntry(t.Context(), entries[0].ID)
	if err != nil || stale.State != "source_invalid" {
		t.Fatalf("edge removal left stage experience active: %+v %v", stale, err)
	}
	if err := d.ValidateMemoryAdmission(t.Context(), admission); !errors.Is(err, ErrMemoryStale) {
		t.Fatalf("edge removal retained actual-send admission: %v", err)
	}
	if err := d.Exploration(task.ExplorationID).SetIntentState(intent, "running"); err != nil {
		t.Fatal(err)
	}
	if current, err := d.GetMemorySource(t.Context(), source.Kind, source.ID); err != nil || current != nil {
		t.Fatalf("reopened stage still live: %+v %v", current, err)
	}
}

func TestMemoryTaskStageUsesOnlyEndedOrphanFacts(t *testing.T) {
	d := memoryFixtureDB(t)
	task := memoryFixtureTask(t, d)
	store := d.Exploration(task.ExplorationID)
	intent, _ := memoryFixtureIntent(t, d, task, "running", 1)
	orphan, err := store.AddNode(KindFact, map[string]any{"summary": "orphan native tool diagnostic", "confidence": "observed"}, 0, "confirmed", "mainagent", nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, state := range []string{"created", "running", "paused"} {
		if err := d.SetStatus(task.ID, state); err != nil {
			t.Fatal(err)
		}
		if source, err := d.GetMemorySource(t.Context(), "task_stage", fmt.Sprint(task.ID)); err != nil || source != nil {
			t.Fatalf("task %s prematurely summarized: %+v %v", state, source, err)
		}
	}
	if err := d.SetStatus(task.ID, "done"); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Exec(`UPDATE tasks SET completed_at=now()-interval '31 seconds' WHERE id=$1`, task.ID); err != nil {
		t.Fatal(err)
	}
	source, err := d.GetMemorySource(t.Context(), "task_stage", fmt.Sprint(task.ID))
	if err != nil || source == nil {
		t.Fatalf("ended task fallback=%+v %v", source, err)
	}
	projection := memoryStage(t, *source)
	if len(projection.Facts) != 1 || projection.Facts[0].ID != fmt.Sprint(orphan) {
		t.Fatalf("task fallback repeated worker-owned facts or root instructions: %+v", projection)
	}
	source.Historical = true
	job := memoryQueueClaim(t, d, *source)
	entries, err := memoryComplete(t, d, job, []MemoryInput{{Title: "orphan diagnostic", Content: "conditional task-fallback diagnostic", Kind: "lesson"}})
	if err != nil || len(entries) != 1 {
		t.Fatalf("task fallback completion=%+v %v", entries, err)
	}
	if err := store.Link(intent, RelYields, orphan); err != nil {
		t.Fatal(err)
	}
	invalid, err := d.GetMemoryEntry(t.Context(), entries[0].ID)
	if err != nil || invalid.State != "source_invalid" {
		t.Fatalf("new worker ownership did not invalidate orphan fallback: %+v %v", invalid, err)
	}
	if current, err := d.GetMemorySource(t.Context(), source.Kind, source.ID); err != nil || current != nil {
		t.Fatalf("empty orphan stage remained learnable: %+v %v", current, err)
	}
}

func TestMemoryLearningSnapshotDistinguishesWaitingAndNoExperience(t *testing.T) {
	d := memoryFixtureDB(t)
	task := memoryFixtureTask(t, d)
	intent, _ := memoryFixtureIntent(t, d, task, "running", 1)
	before, err := d.MemoryLearningSnapshot(t.Context())
	if err != nil || !before.WaitingStage {
		t.Fatalf("running-stage snapshot=%+v %v", before, err)
	}
	source := memoryFinishStage(t, d, task, intent, "done")
	queued, err := d.QueueMemoryExtraction(t.Context(), source)
	if err != nil || !queued {
		t.Fatalf("queue phase=%v %v", queued, err)
	}
	pending, err := d.MemoryLearningSnapshot(t.Context())
	if err != nil || pending.Queued < 1 {
		t.Fatalf("queued-stage snapshot=%+v %v", pending, err)
	}
	job, err := d.ClaimMemoryJob(t.Context(), time.Minute)
	if err != nil || job == nil {
		t.Fatalf("claim=%+v %v", job, err)
	}
	running, err := d.MemoryLearningSnapshot(t.Context())
	if err != nil || running.Running < 1 || running.StartsLastHour < 1 {
		t.Fatalf("active-stage snapshot=%+v %v", running, err)
	}
	if _, err := memoryComplete(t, d, *job, nil); err != nil {
		t.Fatal(err)
	}
	done, err := d.MemoryLearningSnapshot(t.Context())
	if err != nil || done.Done < 1 || done.LastJob == nil || done.LastJob.Result != "no_experience" {
		t.Fatalf("empty successful summary mislabeled: %+v %v", done, err)
	}
}

func TestMemoryTerminalRewriteKeepsCompletionGeneration(t *testing.T) {
	d := memoryFixtureDB(t)
	memoryEnableIncrementalHistory(t, d)
	task := memoryFixtureTask(t, d)
	source := memoryFixtureSource(t, d, task)
	source.Historical = false
	job := memoryQueueClaim(t, d, source)
	if _, err := memoryComplete(t, d, job, nil); err != nil {
		t.Fatal(err)
	}
	intent := mustMemoryNodeID(t, source.ID)
	// SetIntentState intentionally bumps generic content_version even for a
	// repeated terminal write. That is not a newly executed worker phase.
	if err := d.Exploration(task.ExplorationID).SetIntentState(intent, "done"); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Exec(`UPDATE exploration_nodes SET completed_at=now()-interval '31 seconds',payload=payload||'{"display_label":"metadata only"}'::jsonb WHERE id=$1`, intent); err != nil {
		t.Fatal(err)
	}
	rewritten, err := d.GetMemorySource(t.Context(), source.Kind, source.ID)
	if err != nil || rewritten == nil || rewritten.Version != source.Version {
		t.Fatalf("terminal rewrite manufactured native execution generation: %+v %v", rewritten, err)
	}
	if queued, err := d.QueueMemoryExtraction(t.Context(), *rewritten); err != nil || queued {
		t.Fatalf("terminal metadata rewrite triggered new model work: %v %v", queued, err)
	}
	found, err := d.FindMemorySources(t.Context(), 100)
	if err != nil || memoryContainsSource(found, *rewritten) {
		t.Fatalf("terminal rewrite re-entered automatic discovery: %+v %v", found, err)
	}
}

func TestMemoryFormingStageDoesNotPersistSourceBeforeQueue(t *testing.T) {
	d := memoryFixtureDB(t)
	task := memoryFixtureTask(t, d)
	source := memoryFixtureSource(t, d, task)
	var count int
	if err := d.QueryRow(`SELECT count(*) FROM memory_sources WHERE source_kind=$1 AND source_id=$2`, source.Kind, source.ID).Scan(&count); err != nil || count != 0 {
		t.Fatalf("intent phase bypassed bounded queue metadata: %d %v", count, err)
	}
	orphan, err := d.Exploration(task.ExplorationID).AddNode(KindFact, map[string]any{"summary": "orphan fixture observation", "confidence": "observed"}, 0, "confirmed", "mainagent", nil)
	if err != nil || orphan <= 0 {
		t.Fatalf("orphan=%d %v", orphan, err)
	}
	if err := d.SetStatus(task.ID, "done"); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Exec(`UPDATE tasks SET completed_at=now()-interval '31 seconds' WHERE id=$1`, task.ID); err != nil {
		t.Fatal(err)
	}
	fallback, err := d.GetMemorySource(t.Context(), "task_stage", fmt.Sprint(task.ID))
	if err != nil || fallback == nil {
		t.Fatalf("task fallback source=%+v %v", fallback, err)
	}
	if err := d.QueryRow(`SELECT count(*) FROM memory_sources WHERE source_kind='task_stage' AND source_id=$1`, fallback.ID).Scan(&count); err != nil || count != 0 {
		t.Fatalf("task phase bypassed bounded queue metadata: %d %v", count, err)
	}
}

func TestMemoryAttemptedGenerationDoesNotRestartAfterLateMemberChange(t *testing.T) {
	d := memoryFixtureDB(t)
	memoryEnableIncrementalHistory(t, d)
	task := memoryFixtureTask(t, d)
	source := memoryFixtureSource(t, d, task)
	source.Historical = false
	job := memoryQueueClaim(t, d, source)
	// This represents real HTTP admission. A transport retry of the SAME logical
	// job remains eligible; re-extracting a newly changed bundle is a different RPC.
	if err := d.ValidateMemoryExtractionJob(t.Context(), job); err != nil {
		t.Fatal(err)
	}
	if err := d.ValidateMemoryExtractionJob(t.Context(), job); err != nil {
		t.Fatalf("same-job bounded retry was blocked: %v", err)
	}
	fact := memorySourceFactID(t, source)
	if _, err := d.Exec(`UPDATE exploration_nodes SET payload=jsonb_set(payload,'{summary}','"late native observation after model admission"'::jsonb),content_version=content_version+1 WHERE id=$1`, fact); err != nil {
		t.Fatal(err)
	}
	if err := d.ValidateMemoryExtractionJob(t.Context(), job); !errors.Is(err, ErrMemoryStale) {
		t.Fatalf("changed source still sends old bytes: %v", err)
	}
	if _, err := memoryComplete(t, d, job, []MemoryInput{{Title: "late output", Content: "must not save stale bundle", Kind: "lesson"}}); !errors.Is(err, ErrMemoryStale) {
		t.Fatalf("changed source admitted late output: %v", err)
	}
	current, err := d.GetMemorySource(t.Context(), source.Kind, source.ID)
	if err != nil || current == nil || current.Version != source.Version || current.Fingerprint == source.Fingerprint {
		t.Fatalf("late member attribution=%+v %v", current, err)
	}
	if queued, err := d.QueueMemoryExtraction(t.Context(), *current); err != nil || queued {
		t.Fatalf("late member change restarted an already attempted phase automatically: %v %v", queued, err)
	}
	automatic, err := d.FindMemorySources(t.Context(), 100)
	if err != nil || memoryContainsSource(automatic, *current) {
		t.Fatalf("late member change re-entered default discovery: %+v %v", automatic, err)
	}
}

func TestMemoryArchiveCompletionGenerationDefaultsAndPreservation(t *testing.T) {
	d := memoryFixtureDB(t)
	if err := d.EnsureLLMRecordsTable(); err != nil {
		t.Fatal(err)
	}
	if err := d.EnsureLLMUsageTable(); err != nil {
		t.Fatal(err)
	}
	for _, mode := range []string{"legacy_missing", "legacy_null", "current_positive"} {
		t.Run(mode, func(t *testing.T) {
			var taskInput, intentInput any
			var wantTask, wantIntent int64
			if mode == "current_positive" {
				taskInput, intentInput = int64(7), int64(9)
				wantTask, wantIntent = 7, 9
			}
			expID, err := d.CreateExploration("archive memory fixture", "fixture")
			if err != nil {
				t.Fatal(err)
			}
			var taskID, taskGeneration int64
			err = d.QueryRow(`INSERT INTO tasks(description,goal,exploration_id,status,paused,completed_at,memory_completion_generation)
     VALUES('archive memory fixture','fixture',$1,'done',true,now()-interval '31 seconds',$2) RETURNING id,memory_completion_generation`, expID, taskInput).Scan(&taskID, &taskGeneration)
			if err != nil {
				_, _ = d.Exec(`DELETE FROM explorations WHERE id=$1`, expID)
				t.Fatal(err)
			}
			t.Cleanup(func() {
				_, _ = d.Exec(`DELETE FROM task_archives WHERE task_id=$1`, taskID)
				if err := d.DeleteTask(taskID); err != nil {
					t.Errorf("delete archive generation fixture: %v", err)
				}
				for _, query := range []string{
					`DELETE FROM memory_audit WHERE task_id=$1 OR entry_id IN (SELECT id FROM memory_entries WHERE origin_task_id=$1)`,
					`DELETE FROM memory_entries WHERE origin_task_id=$1`,
					`DELETE FROM memory_jobs WHERE source_ref IN (SELECT id FROM memory_sources WHERE origin_task_id=$1)`,
					`DELETE FROM memory_sources WHERE origin_task_id=$1`,
				} {
					if _, err := d.Exec(query, taskID); err != nil {
						t.Errorf("clean archive generation metadata: %v", err)
					}
				}
			})
			var intentID, intentGeneration int64
			err = d.QueryRow(`INSERT INTO exploration_nodes(exploration_id,kind,payload,state,origin,completed_at,memory_completion_generation)
     VALUES($1,'intent','{"summary":"archive phase instructions"}'::jsonb,'done','worker',now()-interval '31 seconds',$2)
     RETURNING id,memory_completion_generation`, expID, intentInput).Scan(&intentID, &intentGeneration)
			if err != nil {
				t.Fatal(err)
			}
			if taskGeneration != wantTask || intentGeneration != wantIntent {
				t.Fatalf("archive INSERT generation normalization task=%d intent=%d", taskGeneration, intentGeneration)
			}
			store := d.Exploration(expID)
			member, err := store.AddNode(KindFact, map[string]any{"summary": "archived bounded compatibility observation", "confidence": "observed"}, 0, "confirmed", "worker", nil)
			if err != nil {
				t.Fatal(err)
			}
			if err := store.Link(intentID, RelYields, member); err != nil {
				t.Fatal(err)
			}
			if _, err := store.AddNode(KindFact, map[string]any{"summary": "archived orphan diagnostic", "confidence": "observed"}, 0, "confirmed", "mainagent", nil); err != nil {
				t.Fatal(err)
			}
			archive, err := d.QueueTaskArchive(taskID)
			if err != nil {
				t.Fatal(err)
			}
			claimed, err := d.ClaimTaskArchiveJob(t.Context())
			if err != nil || claimed == nil || claimed.ID != archive.ID {
				t.Fatalf("claim archive=%+v %v", claimed, err)
			}
			snapshot, err := d.SnapshotTaskArchive(taskID)
			if err != nil {
				t.Fatal(err)
			}
			for _, globalTable := range []string{"memory_entries", "memory_sources", "memory_jobs", "memory_audit"} {
				if _, present := snapshot.Tables[globalTable]; present {
					t.Fatalf("task archive included global memory table %s", globalTable)
				}
			}
			if mode != "current_positive" {
				for _, table := range []string{"tasks", "exploration_nodes"} {
					var rows []map[string]json.RawMessage
					if err := json.Unmarshal(snapshot.Tables[table], &rows); err != nil {
						t.Fatal(err)
					}
					for _, row := range rows {
						if mode == "legacy_missing" {
							delete(row, "memory_completion_generation")
						} else {
							row["memory_completion_generation"] = json.RawMessage("null")
						}
					}
					snapshot.Tables[table], err = json.Marshal(rows)
					if err != nil {
						t.Fatal(err)
					}
				}
			}
			// These store calls use a synthetic manifest path; no archive file, process
			// or external tool is created by this regression fixture.
			if err := d.CompleteTaskArchive(archive.ID, snapshot, "/tmp/memory-generation-fixture.tar.zst", "fixture", 1, 1); err != nil {
				t.Fatal(err)
			}
			if _, err := d.QueueTaskArchiveRestore(archive.ID); err != nil {
				t.Fatal(err)
			}
			claimed, err = d.ClaimTaskArchiveJob(t.Context())
			if err != nil || claimed == nil || claimed.ID != archive.ID {
				t.Fatalf("claim restore=%+v %v", claimed, err)
			}
			if _, err := d.RestoreTaskArchive(archive.ID, snapshot, 0); err != nil {
				t.Fatal(err)
			}
			if err := d.QueryRow(`SELECT memory_completion_generation FROM tasks WHERE id=$1`, taskID).Scan(&taskGeneration); err != nil {
				t.Fatal(err)
			}
			if err := d.QueryRow(`SELECT memory_completion_generation FROM exploration_nodes WHERE id=$1`, intentID).Scan(&intentGeneration); err != nil {
				t.Fatal(err)
			}
			if taskGeneration != wantTask || intentGeneration != wantIntent {
				t.Fatalf("restore fabricated or erased completion generation: task=%d want=%d intent=%d want=%d", taskGeneration, wantTask, intentGeneration, wantIntent)
			}
			for _, sourceCase := range []struct {
				kind       string
				id         int64
				generation int64
			}{{"intent_stage", intentID, wantIntent}, {"task_stage", taskID, wantTask}} {
				source, err := d.GetMemorySource(t.Context(), sourceCase.kind, fmt.Sprint(sourceCase.id))
				if err != nil || source == nil || source.Version != sourceCase.generation+1 {
					t.Fatalf("restored phase source=%+v %v, generation=%d", source, err, sourceCase.generation)
				}
			}
		})
	}
}

func TestMemoryStageDoesNotDropUnsupportedCriticalConditions(t *testing.T) {
	d := memoryFixtureDB(t)
	for _, condition := range []struct {
		name, key string
		value     any
	}{
		{"long_preconditions", "preconditions", strings.Repeat("required prerequisite ", 12)},
		{"long_version", "tool_version", strings.Repeat("version", 24)},
		{"object_platform", "platform", map[string]any{"name": "fixture-os"}},
		{"array_preconditions", "preconditions", []string{"logged-in fixture session"}},
		{"numeric_technology", "technology", 123},
	} {
		t.Run(condition.name, func(t *testing.T) {
			task := memoryFixtureTask(t, d)
			store := d.Exploration(task.ExplorationID)
			intent, err := store.AddIntent(map[string]any{"summary": "conditioned phase fixture"}, 0, nil, "planner")
			if err != nil {
				t.Fatal(err)
			}
			payload := map[string]any{"summary": "short observation with a material environment condition", "confidence": "observed", condition.key: condition.value}
			unsupported, err := store.AddNode(KindFact, payload, 0, "confirmed", "worker", nil)
			if err != nil {
				t.Fatal(err)
			}
			if err := store.Link(intent, RelYields, unsupported); err != nil {
				t.Fatal(err)
			}
			if err := store.SetIntentState(intent, "done"); err != nil {
				t.Fatal(err)
			}
			if _, err := d.Exec(`UPDATE exploration_nodes SET completed_at=now()-interval '31 seconds' WHERE id=$1`, intent); err != nil {
				t.Fatal(err)
			}
			if source, err := d.GetMemorySource(t.Context(), "intent_stage", fmt.Sprint(intent)); err != nil || source != nil {
				t.Fatalf("sole fact lost critical condition instead of being excluded: %+v %v", source, err)
			}
			safe, err := store.AddNode(KindFact, map[string]any{"summary": "independent safe bounded observation", "confidence": "observed"}, 0, "confirmed", "worker", nil)
			if err != nil {
				t.Fatal(err)
			}
			if err := store.Link(intent, RelYields, safe); err != nil {
				t.Fatal(err)
			}
			source, err := d.GetMemorySource(t.Context(), "intent_stage", fmt.Sprint(intent))
			if err != nil || source == nil {
				t.Fatalf("valid observation unavailable: %+v %v", source, err)
			}
			projection := memoryStage(t, *source)
			if len(projection.Facts) != 1 || projection.Facts[0].ID != fmt.Sprint(safe) || !projection.Coverage.Partial || projection.Coverage.Total < 2 {
				t.Fatalf("condition omission disguised as unknown: %+v", projection)
			}
			source.Historical = true
			job := memoryQueueClaim(t, d, *source)
			// An omitted fact still participates in freshness and invalidation.
			if _, err := d.Exec(`UPDATE exploration_nodes SET payload=payload||'{"post_phase_update":"new qualification"}'::jsonb,content_version=content_version+1 WHERE id=$1`, unsupported); err != nil {
				t.Fatal(err)
			}
			if err := d.ValidateMemoryExtractionJob(t.Context(), job); !errors.Is(err, ErrMemoryStale) {
				t.Fatalf("unsupported-condition member escaped dependency checking: %v", err)
			}
		})
	}
}

func TestMemoryLeaseRecoveryReusesAttemptedJobAndRejectsStaleToken(t *testing.T) {
	d := memoryFixtureDB(t)
	memoryEnableIncrementalHistory(t, d)
	task := memoryFixtureTask(t, d)
	source := memoryFixtureSource(t, d, task)
	source.Historical = false
	original := memoryQueueClaim(t, d, source)
	if original.Historical {
		t.Fatal("automatic phase fixture unexpectedly used historical override")
	}
	if err := d.ValidateMemoryExtractionJob(t.Context(), original); err != nil {
		t.Fatal(err)
	}
	// Admission records the phase watermark before HTTP. A crash leaves this
	// durable row running; recovery must renew its lease, not invent a new job.
	if _, err := d.Exec(`UPDATE memory_jobs SET lease_until=now()-interval '1 second' WHERE id=$1`, original.ID); err != nil {
		t.Fatal(err)
	}
	if err := d.ValidateMemoryExtractionJob(t.Context(), original); !errors.Is(err, ErrMemoryStale) {
		t.Fatalf("expired lease still admits bytes: %v", err)
	}
	restarted, err := Open(testDSN(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = restarted.Close() })
	candidates, err := restarted.FindMemorySources(t.Context(), 100)
	if err != nil || memoryContainsSource(candidates, source) {
		t.Fatalf("restart rediscovered attempted generation as fresh work: %+v %v", candidates, err)
	}
	recovered, err := restarted.ClaimMemoryJob(t.Context(), time.Minute)
	if err != nil || recovered == nil {
		t.Fatalf("persistent lease recovery=%+v %v", recovered, err)
	}
	if recovered.ID != original.ID || recovered.Attempts != original.Attempts+1 || recovered.LeaseToken == "" || recovered.LeaseToken == original.LeaseToken || recovered.Historical {
		t.Fatalf("lease recovery changed job identity or bypassed watermark: old=%+v new=%+v", original, recovered)
	}
	if recovered.Source.Version != original.Source.Version || recovered.Source.Fingerprint != original.Source.Fingerprint {
		t.Fatal("unchanged phase became a different extraction source during recovery")
	}
	if err := d.ValidateMemoryExtractionJob(t.Context(), original); !errors.Is(err, ErrMemoryStale) {
		t.Fatalf("old process token admitted after takeover: %v", err)
	}
	if _, err := memoryComplete(t, d, original, []MemoryInput{{Title: "stale process result", Content: "old owner must not commit after takeover", Kind: "lesson"}}); !errors.Is(err, ErrMemoryStale) {
		t.Fatalf("old process committed after takeover: %v", err)
	}
	if err := restarted.ValidateMemoryExtractionJob(t.Context(), *recovered); err != nil {
		t.Fatalf("new lease blocked by original attempt watermark: %v", err)
	}
	entries, err := memoryComplete(t, restarted, *recovered, []MemoryInput{{Title: "recovered phase method", Content: "conditional bounded extraction after durable lease recovery", Kind: "procedure"}})
	if err != nil || len(entries) != 1 || !entries[0].Automatic {
		t.Fatalf("recovered job completion=%+v %v", entries, err)
	}
	if next, err := restarted.ClaimMemoryJob(t.Context(), time.Minute); err != nil || next != nil {
		t.Fatalf("completed recovered generation claimed again: %+v %v", next, err)
	}
	current, err := restarted.GetMemorySource(t.Context(), source.Kind, source.ID)
	if err != nil || current == nil {
		t.Fatalf("recovered source=%+v %v", current, err)
	}
	if queued, err := restarted.QueueMemoryExtraction(t.Context(), *current); err != nil || queued {
		t.Fatalf("recovered completed phase created a replacement job: %v %v", queued, err)
	}
	var jobs, starts, admissions int
	if err := d.QueryRow(`SELECT count(*) FROM memory_jobs job JOIN memory_sources source ON source.id=job.source_ref
   WHERE source.source_kind=$1 AND source.source_id=$2 AND job.source_version=$3`, source.Kind, source.ID, source.Version).Scan(&jobs); err != nil {
		t.Fatal(err)
	}
	if err := d.QueryRow(`SELECT count(*) FILTER(WHERE action='extract_start'),count(*) FILTER(WHERE action='extract_admitted') FROM memory_audit WHERE job_id=$1`, original.ID).Scan(&starts, &admissions); err != nil {
		t.Fatal(err)
	}
	if jobs != 1 || starts != 2 || admissions != 2 {
		t.Fatalf("recovery lost accounting or created duplicate work: jobs=%d starts=%d admitted=%d", jobs, starts, admissions)
	}
}
