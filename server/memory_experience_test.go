package server

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Autumn-27/artex/db"
	"github.com/Autumn-27/artex/memcontext"
	longmemory "github.com/Autumn-27/artex/memory"
	"github.com/Autumn-27/norma/llm"
)

func TestMemoryExperienceLearningStatusIsAnObservation(t *testing.T) {
	settings := db.MemorySettings{Enabled: true, AutoExtract: true, GlobalShare: true, MaxJobsPerHour: 3}
	stamp := time.Date(2026, 10, 30, 3, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name       string
		mutate     func(*db.MemorySettings)
		snapshot   db.MemoryLearningSnapshot
		available  bool
		wantStatus string
		wantText   string
	}{
		{name: "enabled model alone is not learning", available: true, wantStatus: "idle", wantText: "暂无"},
		{name: "feature disabled wins", mutate: func(s *db.MemorySettings) { s.Enabled = false }, snapshot: db.MemoryLearningSnapshot{Running: 1}, wantStatus: "disabled", wantText: "关闭"},
		{name: "extraction disabled wins", mutate: func(s *db.MemorySettings) { s.AutoExtract = false }, snapshot: db.MemoryLearningSnapshot{Queued: 1}, available: true, wantStatus: "disabled"},
		{name: "selected model unavailable", snapshot: db.MemoryLearningSnapshot{Queued: 1}, wantStatus: "model_unavailable", wantText: "模型"},
		{name: "in flight before waiting budget", snapshot: db.MemoryLearningSnapshot{Running: 1, Queued: 1, StartsLastHour: 3}, available: true, wantStatus: "running"},
		{name: "start budget", snapshot: db.MemoryLearningSnapshot{Queued: 1, StartsLastHour: 3}, available: true, wantStatus: "budget_wait", wantText: "预算"},
		{name: "actual HTTP budget", snapshot: db.MemoryLearningSnapshot{RetryableFailed: 1, AttemptsLastHour: 3}, available: true, wantStatus: "budget_wait"},
		{name: "queued", snapshot: db.MemoryLearningSnapshot{Queued: 2}, available: true, wantStatus: "queued"},
		{name: "retry waits", snapshot: db.MemoryLearningSnapshot{RetryableFailed: 1, Failed: 1}, available: true, wantStatus: "retry_wait"},
		{name: "running native stage", snapshot: db.MemoryLearningSnapshot{WaitingStage: true}, available: true, wantStatus: "waiting_stage", wantText: "阶段结束"},
		{name: "processed without experience is normal", snapshot: db.MemoryLearningSnapshot{Done: 1, LastJob: &db.MemoryLearningLastJob{ID: 7, Result: "no_experience", UpdatedAt: stamp}}, available: true, wantStatus: "idle", wantText: "没有值得保留"},
		{name: "failed attempt not learned", snapshot: db.MemoryLearningSnapshot{Failed: 1, LastJob: &db.MemoryLearningLastJob{ID: 8, Result: "failed", UpdatedAt: stamp}}, available: true, wantStatus: "idle", wantText: "失败"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			configured := settings
			if tc.mutate != nil {
				tc.mutate(&configured)
			}
			view := memoryLearningStatus(configured, tc.snapshot, tc.available)
			if view.Status != tc.wantStatus || (tc.wantText != "" && !strings.Contains(view.Message, tc.wantText)) {
				t.Fatalf("status=%q message=%q, want %q containing %q", view.Status, view.Message, tc.wantStatus, tc.wantText)
			}
			if view.Processed != tc.snapshot.Done || view.StartsLastHour != tc.snapshot.StartsLastHour || view.AttemptsLastHour != tc.snapshot.AttemptsLastHour || view.MaxCallsPerHour != 3 {
				t.Fatalf("snapshot counters changed: %+v", view)
			}
			data, err := json.Marshal(view)
			if err != nil {
				t.Fatal(err)
			}
			var wire map[string]any
			if err := json.Unmarshal(data, &wire); err != nil {
				t.Fatal(err)
			}
			if tc.snapshot.LastJob == nil {
				if _, exists := wire["last_result"]; exists {
					t.Fatal("unknown result became an empty claimed result")
				}
				if _, exists := wire["last_completed_at"]; exists {
					t.Fatal("unknown completion became null or a timestamp")
				}
			} else if view.LastResult != tc.snapshot.LastJob.Result || view.LastCompletedAt == nil || !view.LastCompletedAt.Equal(stamp) {
				t.Fatalf("last result mismatch: %+v", view)
			}
		})
	}
}

func TestMemoryExperienceLearningRouteRequiresJWTAndDoesNotInvokeModel(t *testing.T) {
	s := &Server{jwtKey: testJWTKey, authkv: newMemKV()}
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/memories/learning", nil))
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated state read: %d %s", w.Code, w.Body)
	}

	f := newMemoryHTTPFixture(t)
	before, err := f.pg.GetMemorySettings(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	response := f.request(t, http.MethodGet, "/api/memories/learning", nil)
	if response.Code != http.StatusOK {
		t.Fatalf("state read: %d %s", response.Code, response.Body)
	}
	var view memoryLearningView
	if err := json.Unmarshal(response.Body.Bytes(), &view); err != nil {
		t.Fatal(err)
	}
	if view.Status == "" || view.Message == "" {
		t.Fatal("state read omitted its meaning")
	}
	after, err := f.pg.GetMemorySettings(t.Context())
	if err != nil || after != before {
		t.Fatalf("state read mutated settings: %+v %v", after, err)
	}
	if f.calls != 0 {
		t.Fatal("state read invoked inference")
	}

	f.server.llmOn = false
	response = f.request(t, http.MethodGet, "/api/memories/learning", nil)
	if response.Code != http.StatusOK || json.Unmarshal(response.Body.Bytes(), &view) != nil || view.Status != "model_unavailable" {
		t.Fatalf("unavailable model misreported progress: %d %s", response.Code, response.Body)
	}
	// A missing model selection must not prevent stopping future learning.
	after.Enabled, after.ProfileID = false, 9223372036854775807
	response = f.request(t, http.MethodPut, "/api/memories/settings", after)
	if response.Code != http.StatusOK {
		t.Fatalf("failed model prevented disabling memory: %d %s", response.Code, response.Body)
	}
	response = f.request(t, http.MethodGet, "/api/memories/learning", nil)
	if response.Code != http.StatusOK || json.Unmarshal(response.Body.Bytes(), &view) != nil || view.Status != "disabled" {
		t.Fatalf("disabled state not authoritative: %d %s", response.Code, response.Body)
	}
}

func experienceTask(t *testing.T, f *memoryHTTPFixture, suffix string) *db.Task {
	t.Helper()
	task, err := f.pg.CreateTask("STAGE_BRIEF_NOT_EXPERIENCE "+suffix, "ORIGINAL_TASK_GOAL_NOT_EXPERIENCE", nil, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := f.pg.DeleteTask(task.ID); err != nil {
			t.Errorf("delete own experience task: %v", err)
		}
		for _, statement := range []string{
			`DELETE FROM memory_audit WHERE task_id=$1 OR entry_id IN (SELECT id FROM memory_entries WHERE origin_task_id=$1)`,
			`DELETE FROM memory_entries WHERE origin_task_id=$1`,
			`DELETE FROM memory_jobs WHERE source_ref IN (SELECT id FROM memory_sources WHERE origin_task_id=$1)`,
			`DELETE FROM memory_sources WHERE origin_task_id=$1`,
		} {
			if _, err := f.pg.Exec(statement, task.ID); err != nil {
				t.Errorf("clean own experience metadata: %v", err)
			}
		}
	})
	return task
}

func TestMemoryExperienceTargetScopeIsEnforcedBySearchAndAdmission(t *testing.T) {
	f := newMemoryHTTPFixture(t)
	a, b := experienceTask(t, f, "scope-a"), experienceTask(t, f, "scope-b")
	for _, input := range []map[string]any{
		{"kind": "target", "target": "fixture.invalid"},
		{"kind": "target", "origin_task_id": a.ID},
		{"kind": "target", "target": "fixture.invalid", "origin_task_id": int64(9223372036854775807)},
		{"kind": "lesson", "target": "fixture.invalid", "origin_task_id": a.ID},
	} {
		input["title"], input["content"] = "scope boundary", "conditional historical reference"
		if w := f.request(t, http.MethodPost, "/api/memories", input); w.Code != http.StatusBadRequest {
			t.Fatalf("invalid target ownership accepted: %d %s", w.Code, w.Body)
		}
	}
	needle := "experience-scope-fixture-unique"
	target := f.create(t, "target", map[string]any{"kind": "target", "target": "fixture.invalid", "origin_task_id": a.ID, "content": needle})
	shared := f.create(t, "shared", map[string]any{"kind": "procedure", "origin_task_id": a.ID, "content": needle})
	if target.ShareScope != "source_only" || shared.ShareScope != "global_experience" {
		t.Fatalf("scope DTO mismatch: target=%q shared=%q", target.ShareScope, shared.ShareScope)
	}
	settings, err := f.pg.GetMemorySettings(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	for _, global := range []bool{true, false} {
		settings.GlobalShare = global
		settings, err = f.pg.UpdateMemorySettings(t.Context(), settings, settings.Epoch)
		if err != nil {
			t.Fatal(err)
		}
		foreign, _, err := f.pg.SearchMemory(t.Context(), db.MemorySearchInput{Query: needle, TaskID: b.ID, AgentKey: "worker"})
		if err != nil {
			t.Fatal(err)
		}
		want := 0
		if global {
			want = 1
		}
		if len(foreign) != want || (want == 1 && foreign[0].ID != shared.ID) {
			t.Fatalf("global=%v foreign recall=%+v", global, foreign)
		}
		own, _, err := f.pg.SearchMemory(t.Context(), db.MemorySearchInput{Query: needle, TaskID: a.ID, AgentKey: "worker"})
		if err != nil || len(own) != 2 {
			t.Fatalf("own source records unavailable: %+v %v", own, err)
		}
		admission := db.MemoryAdmission{SettingsEpoch: settings.Epoch, TaskID: b.ID, AgentKey: "worker", SessionID: "scope-fixture", CallKey: fmt.Sprintf("scope-%v", global), References: []db.MemoryRef{{ID: target.ID, Version: target.Version}}}
		if err := f.pg.ValidateMemoryAdmission(t.Context(), admission); !errors.Is(err, db.ErrMemoryStale) {
			t.Fatalf("target final admission crossed tasks global=%v: %v", global, err)
		}
		admission.References = []db.MemoryRef{{ID: shared.ID, Version: shared.Version}}
		err = f.pg.ValidateMemoryAdmission(t.Context(), admission)
		if global && err != nil {
			t.Fatalf("shared experience rejected: %v", err)
		}
		if !global && !errors.Is(err, db.ErrMemoryStale) {
			t.Fatalf("disabled sharing admitted foreign experience: %v", err)
		}
	}
}

// The global worker must not see stages left by other package fixtures. This
// scratch database exists only in CI and is dropped only after its fixture has
// closed. No test or production DSN is printed, and no parent database is reset.
func isolatedExperienceWorkerFixture(t *testing.T) *memoryHTTPFixture {
	t.Helper()
	if os.Getenv("CI") != "true" {
		t.Skip("worker integration uses a CI-only scratch PostgreSQL database")
	}
	base := os.Getenv("ARTEX_PG_DSN")
	parsed, err := url.Parse(base)
	if err != nil || (parsed.Scheme != "postgres" && parsed.Scheme != "postgresql") {
		t.Fatal("CI worker fixture requires the isolated PostgreSQL URL")
	}
	var nonce [8]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		t.Fatal("generate scratch database identifier")
	}
	name := "artex_memory_stage_" + hex.EncodeToString(nonce[:])
	admin, err := sql.Open("pgx", base)
	if err != nil {
		t.Fatal("open CI database administrator connection")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	_, err = admin.ExecContext(ctx, `CREATE DATABASE "`+name+`"`)
	cancel()
	_ = admin.Close()
	if err != nil {
		t.Fatal("create unique CI worker database")
	}
	t.Cleanup(func() {
		connection, err := sql.Open("pgx", base)
		if err != nil {
			t.Error("open scratch cleanup connection")
			return
		}
		defer connection.Close()
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		if _, err := connection.ExecContext(ctx, `DROP DATABASE "`+name+`" WITH (FORCE)`); err != nil {
			t.Error("drop only the unique CI worker database")
		}
	})
	parsed.Path = "/" + name
	t.Setenv("ARTEX_PG_DSN", parsed.String())
	f := newMemoryHTTPFixture(t)
	if _, err := f.pg.Exec(`UPDATE memory_settings SET learn_after=now()-interval '1 hour',epoch=epoch+1`); err != nil {
		t.Fatal(err)
	}
	return f
}

func TestMemoryExperienceWorkerSummarizesOneEndedGeneration(t *testing.T) {
	f := isolatedExperienceWorkerFixture(t)
	task := experienceTask(t, f, "worker-stage")
	store := f.pg.Exploration(task.ExplorationID)
	intentID, err := store.AddIntent(map[string]any{"summary": "INTENT_INSTRUCTIONS_NOT_EXPERIENCE"}, 0, nil, "planner")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SetIntentState(intentID, "running"); err != nil {
		t.Fatal(err)
	}
	facts := []int64{}
	for i, confidence := range []string{"observed", "inferred"} {
		fact, err := store.AddNode(db.KindFact, map[string]any{
			"summary":    fmt.Sprintf("Structured output compatibility observation %d; limitations remain relevant.", i),
			"confidence": confidence, "tool": "fixtureTool", "tool_version": "v1.2", "platform": "laboratory",
			"evidence": "RAW_EVIDENCE_NOT_A_LEARNING_SOURCE", "detail": "DETAIL_NOT_A_LEARNING_SOURCE",
		}, 0, "confirmed", "worker", nil)
		if err != nil {
			t.Fatal(err)
		}
		if err := store.Link(intentID, db.RelYields, fact); err != nil {
			t.Fatal(err)
		}
		facts = append(facts, fact)
	}
	calls := 0
	citedFactID := facts[0]
	var projections []longmemory.StageProjection
	var versions []int64
	f.server.llmDirect = retestProvider{complete: func(ctx context.Context, req llm.CompletionRequest) (llm.Message, string, llm.Usage, error) {
		if !memcontext.MetadataOnly(ctx) {
			t.Fatal("stage extraction was not metadata-only")
		}
		if err := memcontext.Validate(ctx); err != nil {
			return llm.Message{}, "", llm.Usage{}, err
		}
		calls++
		if len(req.Tools) != 0 || len(req.Messages) != 1 {
			t.Fatal("stage extractor received tools or history")
		}
		var input longmemory.Input
		if err := json.Unmarshal([]byte(req.Messages[0].Text()), &input); err != nil {
			t.Fatal(err)
		}
		if input.Kind != "intent_stage" || input.ID != strconv.FormatInt(intentID, 10) || input.TaskID != task.ID || input.Target != "" {
			t.Fatalf("untrusted stage identity: %+v", input)
		}
		for _, forbidden := range []string{"STAGE_BRIEF_NOT_EXPERIENCE", "ORIGINAL_TASK_GOAL_NOT_EXPERIENCE", "INTENT_INSTRUCTIONS_NOT_EXPERIENCE", "RAW_EVIDENCE_NOT_A_LEARNING_SOURCE", "DETAIL_NOT_A_LEARNING_SOURCE", "TERMINATION_MESSAGE_NOT_EXPERIENCE"} {
			if strings.Contains(input.Content, forbidden) {
				t.Fatalf("stage input copied %s", forbidden)
			}
		}
		var projection longmemory.StageProjection
		if err := json.Unmarshal([]byte(input.Content), &projection); err != nil {
			t.Fatal(err)
		}
		if projection.Coverage.Included != len(projection.Facts) || len(projection.Facts) < 2 {
			t.Fatalf("stage was not a fact bundle: %+v", projection)
		}
		confidenceByID := map[string]string{}
		for _, fact := range projection.Facts {
			confidenceByID[fact.ID] = fact.Confidence
		}
		if confidenceByID[strconv.FormatInt(facts[0], 10)] != "observed" || confidenceByID[strconv.FormatInt(facts[1], 10)] != "inferred" {
			t.Fatal("native fact state upgraded confidence")
		}
		version, err := strconv.ParseInt(input.Version, 10, 64)
		if err != nil || version < 1 {
			t.Fatal("missing native stage generation")
		}
		versions = append(versions, version)
		projections = append(projections, projection)
		data, err := json.Marshal([]longmemory.Draft{{Title: "Preserve compatibility conditions", Content: "Treat structured output as a conditional observation and recheck compatibility before reuse.", Kind: "procedure", Target: "", Applicability: "Reference the native source conditions before reuse.", Tags: []string{"compatibility"}, EvidenceIDs: []string{strconv.FormatInt(citedFactID, 10)}}})
		if err != nil {
			t.Fatal(err)
		}
		return llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{llm.TextBlock(string(data))}}, "end_turn", llm.Usage{InputTokens: 10, OutputTokens: 10}, nil
	}}
	f.server.memoryWorkerTick(t.Context())
	if calls != 0 {
		t.Fatal("running stage triggered inference")
	}
	finish := func() {
		t.Helper()
		if err := store.SetIntentOutcome(intentID, "done", "completed", "TERMINATION_MESSAGE_NOT_EXPERIENCE"); err != nil {
			t.Fatal(err)
		}
		if _, err := f.pg.Exec(`UPDATE exploration_nodes SET completed_at=now()-interval '31 seconds' WHERE id=$1`, intentID); err != nil {
			t.Fatal(err)
		}
	}
	finish()
	f.server.memoryWorkerTick(t.Context())
	if calls != 1 {
		t.Fatalf("ended stage calls=%d, want one summary task", calls)
	}
	entries, _, err := f.pg.ListMemoryEntries(t.Context(), db.MemoryFilter{TaskID: task.ID, State: "active"}, 0, 20)
	if err != nil || len(entries) != 1 {
		t.Fatalf("stage result missing: %+v %v", entries, err)
	}
	entry := entries[0]
	if entry.Kind != "procedure" || !entry.Automatic || entry.ShareScope != "global_experience" || entry.Target != "" || len(entry.EvidenceIDs) != 1 || entry.EvidenceIDs[0] != strconv.FormatInt(facts[0], 10) {
		t.Fatalf("result is not an attributed shared experience: %+v", entry)
	}
	for _, fact := range projections[0].Facts {
		if fact.ID != entry.EvidenceIDs[0] {
			continue
		}
		if len(fact.Conditions) == 0 && !strings.Contains(entry.Applicability, longmemory.UnknownApplicability) {
			t.Fatal("unknown source conditions became known technical versions")
		}
		for _, condition := range fact.Conditions {
			if !strings.Contains(entry.Applicability, condition) {
				t.Fatalf("source condition omitted: %q from %q", condition, entry.Applicability)
			}
		}
	}
	f.server.memoryWorkerTick(t.Context())
	if calls != 1 {
		t.Fatal("same ended generation was repeatedly summarized")
	}

	late, err := store.AddNode(db.KindFact, map[string]any{"summary": "A later observation adds limitations without ending another stage.", "confidence": "observed"}, 0, "confirmed", "worker", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Link(intentID, db.RelYields, late); err != nil {
		t.Fatal(err)
	}
	invalid, err := f.pg.GetMemoryEntry(t.Context(), entry.ID)
	if err != nil || invalid == nil || invalid.State != "source_invalid" {
		t.Fatalf("member change did not invalidate the old experience: %+v %v", invalid, err)
	}
	f.server.memoryWorkerTick(t.Context())
	if calls != 1 {
		t.Fatal("late fact generated a per-fact summary call")
	}
	settings, err := f.pg.GetMemorySettings(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	settings.MaxEntries = 4
	if _, err := f.pg.UpdateMemorySettings(t.Context(), settings, settings.Epoch); err != nil {
		t.Fatal(err)
	}
	if _, err := f.pg.Exec(`UPDATE memory_jobs SET created_at=now()-interval '100 days',updated_at=now()-interval '100 days'
WHERE source_ref IN (SELECT id FROM memory_sources WHERE source_kind='intent_stage' AND source_id=$1)`, strconv.FormatInt(intentID, 10)); err != nil {
		t.Fatal(err)
	}
	if err := f.pg.PruneMemory(t.Context()); err != nil {
		t.Fatal(err)
	}
	var retainedJobs int
	if err := f.pg.QueryRow(`SELECT count(*) FROM memory_jobs job JOIN memory_sources source ON source.id=job.source_ref
WHERE source.source_kind='intent_stage' AND source.source_id=$1`, strconv.FormatInt(intentID, 10)).Scan(&retainedJobs); err != nil || retainedJobs != 0 {
		t.Fatalf("scratch job did not reach the pruning boundary: count=%d err=%v", retainedJobs, err)
	}
	f.server.memoryWorkerTick(t.Context())
	if calls != 1 {
		t.Fatal("metadata pruning or settings epoch revived a completed generation")
	}

	if err := store.SetIntentState(intentID, "running"); err != nil {
		t.Fatal(err)
	}
	citedFactID = late // Unknown conditions must remain unknown in the next note.
	finish()
	f.server.memoryWorkerTick(t.Context())
	if calls != 2 || len(versions) != 2 || versions[1] <= versions[0] {
		t.Fatalf("new native completion generation did not learn once: calls=%d versions=%v", calls, versions)
	}
	newEntries, _, err := f.pg.ListMemoryEntries(t.Context(), db.MemoryFilter{TaskID: task.ID, State: "active"}, 0, 20)
	if err != nil || len(newEntries) != 1 || !strings.Contains(newEntries[0].Applicability, longmemory.UnknownApplicability) || len(newEntries[0].EvidenceIDs) != 1 || newEntries[0].EvidenceIDs[0] != strconv.FormatInt(late, 10) {
		t.Fatalf("unknown technical conditions were lost or invented: %+v %v", newEntries, err)
	}
	f.server.memoryWorkerTick(t.Context())
	if calls != 2 {
		t.Fatal("new native generation was summarized more than once")
	}
}
