package server

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Autumn-27/artex/db"
	"github.com/Autumn-27/artex/guard"
	"github.com/Autumn-27/artex/memcontext"
	longmemory "github.com/Autumn-27/artex/memory"
	"github.com/Autumn-27/norma/llm"
)

// All worker integration fixtures use CI-only scratch databases. These tests
// never send a real model request or inspect the local deployment's database.
func memoryOutcomeStage(t *testing.T, f *memoryHTTPFixture) (*db.Task, int64, int64) {
	t.Helper()
	task := experienceTask(t, f, t.Name())
	store := f.pg.Exploration(task.ExplorationID)
	intent, err := store.AddIntent(map[string]any{"summary": "synthetic extraction fixture"}, 0, nil, "planner")
	if err != nil {
		t.Fatal(err)
	}
	// A worker can finish only an intent it actually claimed. Match the native
	// open -> running -> done lifecycle instead of bypassing its state guard.
	if err := store.SetIntentState(intent, "running"); err != nil {
		t.Fatal(err)
	}
	fact, err := store.AddNode(db.KindFact, map[string]any{
		"summary": "Structured output compatibility was observed only under a synthetic harness.", "confidence": "observed",
	}, 0, "confirmed", "worker", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Link(intent, db.RelYields, fact); err != nil {
		t.Fatal(err)
	}
	if err := store.SetIntentOutcome(intent, "done", "completed", "synthetic stage ended"); err != nil {
		t.Fatal(err)
	}
	if _, err := f.pg.Exec(`UPDATE exploration_nodes SET completed_at=now()-interval '31 seconds' WHERE id=$1`, intent); err != nil {
		t.Fatal(err)
	}
	return task, intent, fact
}

func claimedMemoryOutcomeJob(t *testing.T, f *memoryHTTPFixture, intent int64) (db.MemorySettings, db.MemoryJob) {
	t.Helper()
	source, err := f.pg.GetMemorySource(t.Context(), "intent_stage", strconv.FormatInt(intent, 10))
	if err != nil || source == nil {
		t.Fatalf("stage source unavailable: %v", err)
	}
	if _, err := f.pg.QueueMemoryExtraction(t.Context(), *source); err != nil {
		t.Fatal(err)
	}
	job, err := f.pg.ClaimMemoryJob(t.Context(), 90*time.Second)
	if err != nil || job == nil {
		t.Fatalf("stage claim unavailable: %v", err)
	}
	settings, err := f.pg.GetMemorySettings(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	return settings, *job
}

func memoryOutcomeJobState(t *testing.T, f *memoryHTTPFixture, taskID int64) (int64, string, string, int) {
	t.Helper()
	var id int64
	var state, result string
	var attempts int
	if err := f.pg.QueryRow(`SELECT job.id,job.state,job.result,job.attempts FROM memory_jobs job
JOIN memory_sources source ON source.id=job.source_ref WHERE source.origin_task_id=$1 ORDER BY job.id DESC LIMIT 1`, taskID).Scan(&id, &state, &result, &attempts); err != nil {
		t.Fatal(err)
	}
	return id, state, result, attempts
}

func memoryOutcomeResponse(t *testing.T, fact int64, content, target string) string {
	t.Helper()
	data, err := json.Marshal([]longmemory.Draft{{Title: "Conditional extraction method", Content: content,
		Kind: "procedure", Target: target, Applicability: "Only synthetic harness observations.", Tags: []string{}, EvidenceIDs: []string{strconv.FormatInt(fact, 10)}}})
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func TestMemoryWorkerOutcomeClassificationNeverReportsRejectedAsEmpty(t *testing.T) {
	f := isolatedExperienceWorkerFixture(t)
	for _, tc := range []struct {
		name, stop, wantState, wantResult string
		body                              func(*testing.T, int64) string
		providerErr                       error
	}{
		{name: "valid empty", stop: "end_turn", wantState: "done", wantResult: "empty", body: func(*testing.T, int64) string { return "[]" }},
		{name: "malformed JSON", stop: "end_turn", wantState: "failed", wantResult: "output_invalid", body: func(*testing.T, int64) string { return "not JSON" }},
		{name: "truncated output", stop: "max_tokens", wantState: "failed", wantResult: "output_truncated", body: func(*testing.T, int64) string { return "[]" }},
		{name: "provider failure", wantState: "failed", wantResult: "provider_error", providerErr: errors.New("PRIVATE_PROVIDER_BODY_CANARY_DO_NOT_RECORD")},
		{name: "provider timeout", wantState: "failed", wantResult: "timeout", providerErr: context.DeadlineExceeded},
		{name: "target rejection", stop: "end_turn", wantState: "skipped", wantResult: "output_rejected", body: func(t *testing.T, fact int64) string {
			return memoryOutcomeResponse(t, fact, "Check structured output compatibility before reuse.", "fixture.invalid")
		}},
		{name: "sensitive response", stop: "end_turn", wantState: "skipped", wantResult: "output_sensitive", body: func(t *testing.T, fact int64) string {
			return memoryOutcomeResponse(t, fact, "Authorization: Bearer sk-private-memory-canary-1234567890", "")
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			task, _, fact := memoryOutcomeStage(t, f)
			calls := 0
			f.server.llmDirect = retestProvider{complete: func(ctx context.Context, req llm.CompletionRequest) (llm.Message, string, llm.Usage, error) {
				if !memcontext.MetadataOnly(ctx) || len(req.Tools) != 0 {
					t.Fatal("extractor lost its metadata-only/no-tools boundary")
				}
				if err := memcontext.Validate(ctx); err != nil {
					return llm.Message{}, "", llm.Usage{}, err
				}
				calls++
				if tc.providerErr != nil {
					return llm.Message{}, "", llm.Usage{}, tc.providerErr
				}
				return llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{llm.TextBlock(tc.body(t, fact))}}, tc.stop, llm.Usage{}, nil
			}}
			if err := f.server.memoryWorkerTickResult(t.Context()); err != nil {
				t.Fatalf("classified outcome failed persistence: %v", err)
			}
			_, state, result, attempts := memoryOutcomeJobState(t, f, task.ID)
			if state != tc.wantState || result != tc.wantResult || attempts != 1 || calls != 1 {
				t.Fatalf("outcome state=%s result=%s attempts=%d calls=%d", state, result, attempts, calls)
			}
			if result == "no_experience" || strings.Contains(result, "CANARY") {
				t.Fatal("new classified result retained an ambiguous outcome or provider text")
			}
			var entries int
			if err := f.pg.QueryRow(`SELECT count(*) FROM memory_entries WHERE origin_task_id=$1`, task.ID).Scan(&entries); err != nil || entries != 0 {
				t.Fatalf("invalid/empty extraction created entries: %d %v", entries, err)
			}
			if err := f.server.memoryWorkerTickResult(t.Context()); err != nil || calls != 1 {
				t.Fatalf("outcome caused an immediate duplicate call: calls=%d err=%v", calls, err)
			}
		})
	}
}

func TestMemoryWorkerMalformedOutputHasBoundedRetryAndCanRecover(t *testing.T) {
	f := isolatedExperienceWorkerFixture(t)
	task, _, _ := memoryOutcomeStage(t, f)
	calls := 0
	f.server.llmDirect = retestProvider{complete: func(ctx context.Context, _ llm.CompletionRequest) (llm.Message, string, llm.Usage, error) {
		if err := memcontext.Validate(ctx); err != nil {
			return llm.Message{}, "", llm.Usage{}, err
		}
		calls++
		body := "malformed"
		if calls == 2 {
			body = "[]"
		}
		return llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{llm.TextBlock(body)}}, "end_turn", llm.Usage{}, nil
	}}
	if err := f.server.memoryWorkerTickResult(t.Context()); err != nil {
		t.Fatal(err)
	}
	id, state, result, _ := memoryOutcomeJobState(t, f, task.ID)
	if state != "failed" || result != "output_invalid" {
		t.Fatalf("invalid response was not a retryable failure: %s/%s", state, result)
	}
	var retryLater bool
	if err := f.pg.QueryRow(`SELECT retry_at>now()+interval '4 minutes' FROM memory_jobs WHERE id=$1`, id).Scan(&retryLater); err != nil || !retryLater {
		t.Fatalf("retry is not bounded by existing backoff: %v %v", retryLater, err)
	}
	if err := f.server.memoryWorkerTickResult(t.Context()); err != nil || calls != 1 {
		t.Fatalf("failed result retried immediately: calls=%d %v", calls, err)
	}
	if _, err := f.pg.Exec(`UPDATE memory_jobs SET retry_at=now()-interval '1 second' WHERE id=$1`, id); err != nil {
		t.Fatal(err)
	}
	if err := f.server.memoryWorkerTickResult(t.Context()); err != nil {
		t.Fatal(err)
	}
	_, state, result, attempts := memoryOutcomeJobState(t, f, task.ID)
	if state != "done" || result != "empty" || attempts != 2 || calls != 2 {
		t.Fatalf("retry failed to recover with a classified empty response: %s/%s attempts=%d calls=%d", state, result, attempts, calls)
	}
}

func TestMemoryWorkerUsesNewlyClaimedEpochProviderWithoutDroppingJob(t *testing.T) {
	f := isolatedExperienceWorkerFixture(t)
	task, intent, _ := memoryOutcomeStage(t, f)
	oldSettings, err := f.pg.GetMemorySettings(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	oldCalls, newCalls := 0, 0
	oldProvider := retestProvider{complete: func(context.Context, llm.CompletionRequest) (llm.Message, string, llm.Usage, error) {
		oldCalls++
		return llm.Message{}, "", llm.Usage{}, errors.New("old provider must not be used")
	}}
	oldConfig := f.server.llmCfg
	updated := oldSettings
	updated.MaxEntries = 4
	if _, err := f.pg.UpdateMemorySettings(t.Context(), updated, oldSettings.Epoch); err != nil {
		t.Fatal(err)
	}
	f.server.llmCfg.Model = "new-epoch-fixture-model"
	f.server.llmDirect = retestProvider{complete: func(ctx context.Context, _ llm.CompletionRequest) (llm.Message, string, llm.Usage, error) {
		if err := memcontext.Validate(ctx); err != nil {
			return llm.Message{}, "", llm.Usage{}, err
		}
		newCalls++
		return llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{llm.TextBlock("[]")}}, "end_turn", llm.Usage{}, nil
	}}
	_, job := claimedMemoryOutcomeJob(t, f, intent)
	if err := f.server.runClaimedMemoryJob(t.Context(), oldSettings, oldProvider, oldConfig, job); err != nil {
		t.Fatal(err)
	}
	_, state, result, attempts := memoryOutcomeJobState(t, f, task.ID)
	var claims int
	if err := f.pg.QueryRow(`SELECT count(*) FROM memory_audit WHERE job_id=$1 AND action='extract_start'`, job.ID).Scan(&claims); err != nil {
		t.Fatal(err)
	}
	if oldCalls != 0 || newCalls != 1 || state != "done" || result != "empty" || attempts != 1 || claims != 1 {
		t.Fatalf("new epoch job discarded/reclaimed/misrouted: old=%d new=%d %s/%s attempts=%d claims=%d", oldCalls, newCalls, state, result, attempts, claims)
	}
}

func TestMemoryWorkerNewEpochUnavailableProfileDoesNotFallbackOrComplete(t *testing.T) {
	f := isolatedExperienceWorkerFixture(t)
	task, intent, _ := memoryOutcomeStage(t, f)
	oldSettings, err := f.pg.GetMemorySettings(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	updated := oldSettings
	updated.MaxEntries = 4
	if _, err := f.pg.UpdateMemorySettings(t.Context(), updated, oldSettings.Epoch); err != nil {
		t.Fatal(err)
	}
	_, job := claimedMemoryOutcomeJob(t, f, intent)
	f.server.llmOn = false
	if err := f.server.runClaimedMemoryJob(t.Context(), oldSettings, f.server.llmDirect, f.server.llmCfg, job); err != nil {
		t.Fatal(err)
	}
	_, state, result, attempts := memoryOutcomeJobState(t, f, task.ID)
	if state != "failed" || result != "settings_changed" || attempts != 1 || f.calls != 0 {
		t.Fatalf("unavailable new route silently used old provider or discarded stage: %s/%s attempts=%d calls=%d", state, result, attempts, f.calls)
	}
	var completed sql.NullInt64
	if err := f.pg.QueryRow(`SELECT source.last_completed_version FROM memory_sources source JOIN memory_jobs job ON job.source_ref=source.id WHERE job.id=$1`, job.ID).Scan(&completed); err != nil || completed.Valid {
		t.Fatalf("model-resolution failure advanced stage completion: %v %v", completed.Valid, err)
	}
}

func TestMemoryWorkerMalformedOutputStopsBeforeFourthAttempt(t *testing.T) {
	f := isolatedExperienceWorkerFixture(t)
	task, _, _ := memoryOutcomeStage(t, f)
	calls := 0
	f.server.llmDirect = retestProvider{complete: func(ctx context.Context, _ llm.CompletionRequest) (llm.Message, string, llm.Usage, error) {
		if err := memcontext.Validate(ctx); err != nil {
			return llm.Message{}, "", llm.Usage{}, err
		}
		calls++
		return llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{llm.TextBlock("not JSON")}}, "end_turn", llm.Usage{}, nil
	}}
	for attempt := 1; attempt <= 3; attempt++ {
		if err := f.server.memoryWorkerTickResult(t.Context()); err != nil {
			t.Fatal(err)
		}
		id, state, result, attempts := memoryOutcomeJobState(t, f, task.ID)
		wantState := "failed"
		if attempt == 3 {
			wantState = "skipped"
		}
		if calls != attempt || attempts != attempt || state != wantState || result != "output_invalid" {
			t.Fatalf("malformed output retry boundary: calls=%d attempts=%d %s/%s", calls, attempts, state, result)
		}
		if _, err := f.pg.Exec(`UPDATE memory_jobs SET retry_at=now()-interval '1 second' WHERE id=$1`, id); err != nil {
			t.Fatal(err)
		}
	}
	if err := f.server.memoryWorkerTickResult(t.Context()); err != nil || calls != 3 {
		t.Fatalf("exhausted stage made a fourth model call: calls=%d err=%v", calls, err)
	}
}

func TestMemoryWorkerRejectsSensitiveSourceWithoutModelCall(t *testing.T) {
	f := isolatedExperienceWorkerFixture(t)
	task, intent, fact := memoryOutcomeStage(t, f)
	const secret = "memory-input-private-fingerprint-123456"
	if _, err := f.pg.Exec(`UPDATE exploration_nodes SET payload=jsonb_set(payload,'{summary}',to_jsonb($2::text)),content_version=content_version+1 WHERE id=$1`, fact, "Compatibility observation includes "+secret); err != nil {
		t.Fatal(err)
	}
	f.server.m.egress = guard.NewEgressGuard()
	f.server.m.egress.AddFingerprint("llm_api_key", secret)
	settings, job := claimedMemoryOutcomeJob(t, f, intent)
	if err := f.server.runClaimedMemoryJob(t.Context(), settings, f.server.llmDirect, f.server.llmCfg, job); err != nil {
		t.Fatal(err)
	}
	_, state, result, _ := memoryOutcomeJobState(t, f, task.ID)
	if state != "skipped" || result != "input_sensitive" || f.calls != 0 {
		t.Fatalf("sensitive source misclassified/called model: %s/%s calls=%d", state, result, f.calls)
	}
}

func TestMemoryWorkerBudgetAdmissionIsNotAnAPIError(t *testing.T) {
	f := isolatedExperienceWorkerFixture(t)
	task, intent, _ := memoryOutcomeStage(t, f)
	settings, job := claimedMemoryOutcomeJob(t, f, intent)
	if _, err := f.pg.Exec(`INSERT INTO memory_audit(task_id,action) SELECT $1,'extract_admitted' FROM generate_series(1,$2::int)`, task.ID, settings.MaxJobsPerHour); err != nil {
		t.Fatal(err)
	}
	invocations, attempts := 0, 0
	provider := retestProvider{complete: func(ctx context.Context, _ llm.CompletionRequest) (llm.Message, string, llm.Usage, error) {
		invocations++
		if err := memcontext.Validate(ctx); err != nil {
			return llm.Message{}, "", llm.Usage{}, err
		}
		attempts++
		return llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{llm.TextBlock("[]")}}, "end_turn", llm.Usage{}, nil
	}}
	if err := f.server.runClaimedMemoryJob(t.Context(), settings, provider, f.server.llmCfg, job); err != nil {
		t.Fatal(err)
	}
	_, state, result, claims := memoryOutcomeJobState(t, f, task.ID)
	if state != "failed" || result != "budget_deferred" || claims != 0 || invocations != 1 || attempts != 0 {
		t.Fatalf("budget gate misreported API outcome: %s/%s claims=%d invocations=%d attempts=%d", state, result, claims, invocations, attempts)
	}
}

func TestMemoryWorkerLocalAdmissionRejectionsDoNotSpendHTTPBudget(t *testing.T) {
	f := isolatedExperienceWorkerFixture(t)
	for _, tc := range []struct {
		name, wantState, wantResult string
		change                      func()
	}{
		{name: "changed model", wantState: "failed", wantResult: "settings_changed", change: func() {
			f.server.cfgMu.Lock()
			f.server.llmCfg.Model += "-edited-before-http"
			f.server.cfgMu.Unlock()
		}},
		{name: "new sensitive fingerprint", wantState: "skipped", wantResult: "input_sensitive", change: func() {
			f.server.m.egress.AddFingerprint("llm_api_key", "Structured output compatibility")
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			task, intent, _ := memoryOutcomeStage(t, f)
			settings, job := claimedMemoryOutcomeJob(t, f, intent)
			originalConfig := f.server.llmCfg
			f.server.m.egress = guard.NewEgressGuard()
			t.Cleanup(func() {
				f.server.cfgMu.Lock()
				f.server.llmCfg = originalConfig
				f.server.cfgMu.Unlock()
				f.server.m.egress = nil
			})
			invocations, attempts := 0, 0
			provider := retestProvider{complete: func(ctx context.Context, _ llm.CompletionRequest) (llm.Message, string, llm.Usage, error) {
				invocations++
				// Simulate an edit after input preparation but before the real
				// provider transport's final request-admission callback.
				tc.change()
				if err := memcontext.Validate(ctx); err != nil {
					return llm.Message{}, "", llm.Usage{}, err
				}
				attempts++
				return llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{llm.TextBlock("[]")}}, "end_turn", llm.Usage{}, nil
			}}
			if err := f.server.runClaimedMemoryJob(t.Context(), settings, provider, originalConfig, job); err != nil {
				t.Fatal(err)
			}
			_, state, result, claims := memoryOutcomeJobState(t, f, task.ID)
			var admitted int
			if err := f.pg.QueryRow(`SELECT count(*) FROM memory_audit WHERE job_id=$1 AND action='extract_admitted'`, job.ID).Scan(&admitted); err != nil {
				t.Fatal(err)
			}
			if state != tc.wantState || result != tc.wantResult || claims != 1 || invocations != 1 || attempts != 0 || admitted != 0 {
				t.Fatalf("local rejection consumed HTTP budget: %s/%s claims=%d invocations=%d attempts=%d admitted=%d", state, result, claims, invocations, attempts, admitted)
			}
		})
	}
}

func TestMemoryWorkerParentCancellationDoesNotWriteDetachedFailure(t *testing.T) {
	f := isolatedExperienceWorkerFixture(t)
	task, intent, _ := memoryOutcomeStage(t, f)
	settings, job := claimedMemoryOutcomeJob(t, f, intent)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	provider := retestProvider{complete: func(callCtx context.Context, _ llm.CompletionRequest) (llm.Message, string, llm.Usage, error) {
		cancel()
		return llm.Message{}, "", llm.Usage{}, callCtx.Err()
	}}
	if err := f.server.runClaimedMemoryJob(ctx, settings, provider, f.server.llmCfg, job); !errors.Is(err, context.Canceled) {
		t.Fatalf("parent cancellation lost: %v", err)
	}
	_, state, result, _ := memoryOutcomeJobState(t, f, task.ID)
	if state != "running" || result != "" {
		t.Fatalf("shutdown used a detached write: %s/%s", state, result)
	}
}

func TestMemoryWorkerClaimStorageErrorIsSafeAndBacksOff(t *testing.T) {
	f := isolatedExperienceWorkerFixture(t)
	task, _, _ := memoryOutcomeStage(t, f)
	const canary = "PRIVATE_DATABASE_DETAIL_CANARY_DO_NOT_LOG"
	if _, err := f.pg.Exec(`CREATE FUNCTION memory_fixture_claim_failure() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN
IF NEW.state='running' THEN RAISE EXCEPTION 'PRIVATE_DATABASE_DETAIL_CANARY_DO_NOT_LOG'; END IF;
RETURN NEW; END $$; CREATE TRIGGER memory_fixture_claim_failure BEFORE UPDATE ON memory_jobs
FOR EACH ROW EXECUTE PROCEDURE memory_fixture_claim_failure()`); err != nil {
		t.Fatal(err)
	}
	defer func() {
		_, _ = f.pg.Exec(`DROP TRIGGER memory_fixture_claim_failure ON memory_jobs; DROP FUNCTION memory_fixture_claim_failure()`)
	}()
	err := f.server.memoryWorkerTickResult(t.Context())
	if err == nil || memoryWorkerErrorCategory(err) != "storage_error" || strings.Contains(err.Error(), canary) {
		t.Fatalf("claim storage error swallowed or exposed: %v", err)
	}
	_, state, result, attempts := memoryOutcomeJobState(t, f, task.ID)
	if state != "queued" || result != "" || attempts != 0 || f.calls != 0 {
		t.Fatalf("failed claim created a completion/call: %s/%s attempts=%d calls=%d", state, result, attempts, f.calls)
	}
}

func TestMemoryWorkerSourceStorageErrorIsNotEmptyExperience(t *testing.T) {
	f := isolatedExperienceWorkerFixture(t)
	task, intent, _ := memoryOutcomeStage(t, f)
	settings, job := claimedMemoryOutcomeJob(t, f, intent)
	if _, err := f.pg.Exec(`ALTER FUNCTION memory_build_stage_source(text,text) RENAME TO memory_fixture_unavailable_source`); err != nil {
		t.Fatal(err)
	}
	defer func() {
		_, _ = f.pg.Exec(`ALTER FUNCTION memory_fixture_unavailable_source(text,text) RENAME TO memory_build_stage_source`)
	}()
	err := f.server.runClaimedMemoryJob(t.Context(), settings, f.server.llmDirect, f.server.llmCfg, job)
	if err == nil || memoryWorkerErrorCategory(err) != "storage_error" {
		t.Fatalf("source read failure was swallowed: %v", err)
	}
	_, state, result, _ := memoryOutcomeJobState(t, f, task.ID)
	if state != "failed" || result != "storage_error" || f.calls != 0 {
		t.Fatalf("source storage failure became normal empty/call: %s/%s calls=%d", state, result, f.calls)
	}
}

func TestMemoryWorkerCompletionStorageFailureRollsBackEmptyWatermark(t *testing.T) {
	f := isolatedExperienceWorkerFixture(t)
	task, intent, _ := memoryOutcomeStage(t, f)
	settings, job := claimedMemoryOutcomeJob(t, f, intent)
	if _, err := f.pg.Exec(`CREATE FUNCTION memory_fixture_completion_failure() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN
IF NEW.state='done' THEN RAISE EXCEPTION 'PRIVATE_COMPLETION_DETAIL_CANARY_DO_NOT_LOG'; END IF;
RETURN NEW; END $$; CREATE TRIGGER memory_fixture_completion_failure BEFORE UPDATE ON memory_jobs
FOR EACH ROW EXECUTE PROCEDURE memory_fixture_completion_failure()`); err != nil {
		t.Fatal(err)
	}
	defer func() {
		_, _ = f.pg.Exec(`DROP TRIGGER memory_fixture_completion_failure ON memory_jobs; DROP FUNCTION memory_fixture_completion_failure()`)
	}()
	calls := 0
	provider := retestProvider{complete: func(ctx context.Context, _ llm.CompletionRequest) (llm.Message, string, llm.Usage, error) {
		if err := memcontext.Validate(ctx); err != nil {
			return llm.Message{}, "", llm.Usage{}, err
		}
		calls++
		return llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{llm.TextBlock("[]")}}, "end_turn", llm.Usage{}, nil
	}}
	err := f.server.runClaimedMemoryJob(t.Context(), settings, provider, f.server.llmCfg, job)
	if err == nil || memoryWorkerErrorCategory(err) != "storage_error" || strings.Contains(err.Error(), "PRIVATE_COMPLETION") {
		t.Fatalf("completion storage failure swallowed or exposed: %v", err)
	}
	_, state, result, _ := memoryOutcomeJobState(t, f, task.ID)
	var completed sql.NullInt64
	if err := f.pg.QueryRow(`SELECT source.last_completed_version FROM memory_sources source JOIN memory_jobs job ON job.source_ref=source.id WHERE job.id=$1`, job.ID).Scan(&completed); err != nil {
		t.Fatal(err)
	}
	if state != "failed" || result != "storage_error" || calls != 1 || completed.Valid {
		t.Fatalf("failed commit falsely advanced empty completion: %s/%s calls=%d completed=%v", state, result, calls, completed.Valid)
	}
}

func TestMemoryWorkerStorageErrorNeverFormatsPrivateCause(t *testing.T) {
	const canary = "PRIVATE_STORAGE_ERROR_CANARY"
	cause := errors.New(canary)
	err := memoryWorkerStorageError(t.Context(), cause)
	if !errors.Is(err, cause) || strings.Contains(err.Error(), canary) || memoryWorkerErrorCategory(err) != "storage_error" {
		t.Fatal("safe storage classification lost its cause or exposed its text")
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if got := memoryWorkerStorageError(ctx, cause); !errors.Is(got, context.Canceled) {
		t.Fatal("storage wrapper replaced parent cancellation")
	}
}
