package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Autumn-27/artex/agent"
	"github.com/Autumn-27/artex/db"
	"github.com/Autumn-27/artex/guard"
	"github.com/Autumn-27/artex/memcontext"
	longmemory "github.com/Autumn-27/artex/memory"
	"github.com/Autumn-27/norma/llm"
)

func TestMemoryWorkerRecoversTruncatedResponsesWithBoundedBudget(t *testing.T) {
	for _, finish := range []string{"valid", "still truncated", "unsupported evidence"} {
		t.Run(finish, func(t *testing.T) {
			f := isolatedExperienceWorkerFixture(t)
			task, _, fact := memoryOutcomeStage(t, f)
			var mu sync.Mutex
			var budgets []int
			var inputs []string
			fixture := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var body struct {
					MaxTokens int             `json:"max_output_tokens"`
					Input     json.RawMessage `json:"input"`
					Tools     []any           `json:"tools"`
				}
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil || len(body.Tools) != 0 {
					http.Error(w, "fixture request shape", http.StatusBadRequest)
					return
				}
				mu.Lock()
				budgets = append(budgets, body.MaxTokens)
				inputs = append(inputs, string(body.Input))
				mu.Unlock()
				status, text := "incomplete", `[{"title":"unfinished`
				if body.MaxTokens >= 6000 && finish != "still truncated" {
					id := strconv.FormatInt(fact, 10)
					if finish == "unsupported evidence" {
						id = "invented-fact"
					}
					draft := longmemory.Draft{Title: "Structured output compatibility", Content: "Structured output was observed under a synthetic harness; recheck compatibility before reuse.",
						Kind: "procedure", Applicability: "Synthetic harness only", Tags: []string{}, EvidenceIDs: []string{id}}
					encoded, _ := json.Marshal([]longmemory.Draft{draft})
					status, text = "completed", string(encoded)
				}
				response := map[string]any{"id": "fixture-response", "status": status,
					"output": []any{map[string]any{"type": "message", "role": "assistant", "content": []any{map[string]any{"type": "output_text", "text": text}}}},
					"usage":  map[string]any{"input_tokens": 100, "output_tokens": body.MaxTokens}}
				if status == "incomplete" {
					response["incomplete_details"] = map[string]any{"reason": "max_output_tokens"}
				}
				w.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(w).Encode(response)
			}))
			defer fixture.Close()
			cfg := agent.Config{Model: "fixture-model", APIKey: "fixture-key", BaseURL: fixture.URL, Format: llm.FormatOpenAIResponses}
			provider, err := cfg.NewProvider()
			if err != nil {
				t.Fatal(err)
			}
			f.server.llmDirect, f.server.llmCfg = provider, cfg
			var jobID int64
			for attempt := 1; attempt <= 3; attempt++ {
				if err := f.server.memoryWorkerTickResult(t.Context()); err != nil {
					t.Fatal(err)
				}
				id, state, result, tries := memoryOutcomeJobState(t, f, task.ID)
				jobID = id
				wantState, wantResult := "failed", "output_truncated"
				if attempt == 3 {
					wantState = "skipped"
					if finish == "valid" {
						wantState, wantResult = "done", "saved"
					} else if finish == "unsupported evidence" {
						wantResult = "output_rejected"
					}
				}
				if state != wantState || result != wantResult || tries != attempt {
					t.Fatalf("attempt %d: %s/%s/%d, want %s/%s", attempt, state, result, tries, wantState, wantResult)
				}
				if _, err := f.pg.Exec(`UPDATE memory_jobs SET retry_at=now()-interval '1 second' WHERE id=$1`, jobID); err != nil {
					t.Fatal(err)
				}
			}
			if err := f.server.memoryWorkerTickResult(t.Context()); err != nil {
				t.Fatal(err)
			}
			mu.Lock()
			defer mu.Unlock()
			if len(budgets) != 3 || budgets[0] != 1500 || budgets[1] != 3000 || budgets[2] != 6000 {
				t.Fatalf("wire budgets or three-attempt cap changed: %v", budgets)
			}
			if inputs[0] != inputs[1] || inputs[1] != inputs[2] {
				t.Fatal("recovery changed source conditions or replayed truncated output")
			}
			var admitted int
			if err := f.pg.QueryRow(`SELECT count(*) FROM memory_audit WHERE job_id=$1 AND action='extract_admitted'`, jobID).Scan(&admitted); err != nil || admitted != 3 {
				t.Fatalf("actual HTTP attempts bypassed admission: %d %v", admitted, err)
			}
			entries, _, err := f.pg.ListMemoryEntries(t.Context(), db.MemoryFilter{TaskID: task.ID, State: "active"}, 0, 20)
			if err != nil {
				t.Fatal(err)
			}
			if finish == "valid" {
				if len(entries) != 1 || entries[0].Confidence != "unverified" || !strings.Contains(entries[0].Applicability, longmemory.UnknownApplicability) {
					t.Fatal("recovered note lost evidence uncertainty or complete conditions")
				}
			} else if len(entries) != 0 {
				t.Fatal("truncated or unsupported recovery created a note")
			}
		})
	}
}

func TestMemoryWorkerRecoveryDeadlineFitsLease(t *testing.T) {
	f := isolatedExperienceWorkerFixture(t)
	task, intent, _ := memoryOutcomeStage(t, f)
	settings, job := claimedMemoryOutcomeJob(t, f, intent)
	if err := f.pg.FailMemoryJobWithResult(t.Context(), job, time.Minute, "output_truncated"); err != nil {
		t.Fatal(err)
	}
	if _, err := f.pg.Exec(`UPDATE memory_jobs SET output_truncations=2,retry_at=now()-interval '1 second' WHERE id=$1`, job.ID); err != nil {
		t.Fatal(err)
	}
	calls := 0
	f.server.llmDirect = retestProvider{complete: func(ctx context.Context, req llm.CompletionRequest) (llm.Message, string, llm.Usage, error) {
		calls++
		deadline, ok := ctx.Deadline()
		var lease time.Time
		if err := f.pg.QueryRow(`SELECT lease_until FROM memory_jobs WHERE id=$1`, job.ID).Scan(&lease); err != nil || !ok || !lease.After(deadline.Add(15*time.Second)) || time.Until(deadline) > 90*time.Second || time.Until(deadline) < 85*time.Second {
			t.Fatal("largest recovery deadline exceeded its claim lease or kept the old 45s limit")
		}
		if err := memcontext.Validate(ctx); err != nil {
			return llm.Message{}, "", llm.Usage{}, err
		}
		return llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{llm.TextBlock("[]")}}, "end_turn", llm.Usage{}, nil
	}}
	if err := f.server.memoryWorkerTickResult(t.Context()); err != nil || calls != 1 {
		t.Fatalf("recovery deadline fixture failed: %v calls=%d epoch=%d", err, calls, settings.Epoch)
	}
	_, state, result, _ := memoryOutcomeJobState(t, f, task.ID)
	if state != "done" || result != "empty" {
		t.Fatal("complete recovery was not committed within its lease")
	}
}

func TestMemoryRecallCandidatesSurviveDuplicateAndSensitiveTopHits(t *testing.T) {
	f := isolatedExperienceWorkerFixture(t)
	settings, err := f.pg.GetMemorySettings(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	settings.MaxEntries = 2
	if _, err := f.pg.UpdateMemorySettings(t.Context(), settings, settings.Epoch); err != nil {
		t.Fatal(err)
	}
	reader := experienceTask(t, f, "reader")
	// First create the lower-ranked distinct note, then two newer identical
	// notes from different tasks and a newly protected secret at the top.
	var ids []int64
	for _, body := range []string{"MEMORY_RECALL_FIXTURE distinct observation", "MEMORY_RECALL_FIXTURE duplicate observation", "MEMORY_RECALL_FIXTURE duplicate observation", "MEMORY_RECALL_FIXTURE protected-canary"} {
		owner := experienceTask(t, f, "owner")
		entry := f.create(t, "", map[string]any{"title": "MEMORY_RECALL_FIXTURE", "content": body, "origin_task_id": owner.ID})
		ids = append(ids, entry.ID)
	}
	f.server.m.egress = guard.NewEgressGuard()
	f.server.m.egress.AddFingerprint("llm_api_key", "protected-canary")
	query := llm.CompletionRequest{Messages: []llm.Message{llm.UserText("MEMORY_RECALL_FIXTURE")}}
	result, err := f.server.recallMemory(memcontext.WithByteBudget(t.Context(), 2000), agent.RunInfo{TaskID: reader.ID}, "worker", query)
	if err != nil || len(result.References) != 2 || !strings.Contains(result.Block, "distinct observation") || strings.Contains(result.Block, "protected-canary") || strings.Count(result.Block, "duplicate observation") != 1 {
		t.Fatalf("top candidates starved usable complete notes: refs=%d err=%v", len(result.References), err)
	}
	if len(memcontext.FormatBlock(result.Block, result.References)) > settings.MaxTokens || strings.Contains(result.Block, "index_only") {
		t.Fatal("candidate refill exceeded injection budget or substituted title indexes")
	}
	if err := result.Revalidate(t.Context()); err != nil {
		t.Fatalf("refilled references failed final admission: %v", err)
	}
	if _, err := f.pg.Exec(`UPDATE memory_entries SET state='disabled',version=version+1 WHERE id=$1`, ids[0]); err != nil {
		t.Fatal(err)
	}
	if err := result.Revalidate(t.Context()); err == nil {
		t.Fatal("refilling candidates bypassed final revocation checks")
	}
	settings, err = f.pg.GetMemorySettings(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	settings.GlobalShare = false
	if _, err := f.pg.UpdateMemorySettings(t.Context(), settings, settings.Epoch); err != nil {
		t.Fatal(err)
	}
	result, err = f.server.recallMemory(t.Context(), agent.RunInfo{TaskID: reader.ID}, "worker", query)
	if err != nil || len(result.References) != 0 {
		t.Fatal("larger candidate set bypassed cross-task sharing controls")
	}
}
