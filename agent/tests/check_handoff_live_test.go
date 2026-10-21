//go:build ignore

package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"iter"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Autumn-27/artex/db"
	"github.com/Autumn-27/norma/llm"
	actool "github.com/Autumn-27/norma/tool"
)

type liveMeasuredProvider struct {
	llm.Provider
	calls int
	usage llm.Usage
}

func (p *liveMeasuredProvider) Stream(ctx context.Context, req llm.CompletionRequest) iter.Seq2[llm.StreamEvent, error] {
	return func(yield func(llm.StreamEvent, error) bool) {
		p.calls++
		acc := llm.NewAccumulator()
		defer func() { p.usage.Add(acc.Usage) }()
		for event, err := range p.Provider.Stream(ctx, req) {
			if err == nil {
				acc.Add(event)
			}
			if !yield(event, err) {
				return
			}
		}
	}
}

func (p *liveMeasuredProvider) Complete(ctx context.Context, req llm.CompletionRequest) (llm.Message, string, llm.Usage, error) {
	p.calls++
	message, reason, usage, err := p.Provider.Complete(ctx, req)
	p.usage.Add(usage)
	return message, reason, usage, err
}

// Opt-in, isolated model handoff matrix. It exercises real provider responses
// against a local fixture, never a production target or credential.
func TestCheckHandoffLive(t *testing.T) {
	profilePath := os.Getenv("ARTEX_P0_LIVE_PROFILE")
	if profilePath == "" {
		t.Skip("requires an explicitly selected live profile")
	}
	raw, err := os.ReadFile(profilePath)
	if err != nil {
		t.Fatal(err)
	}
	var profile db.LLMProfile
	if err = json.Unmarshal(raw, &profile); err != nil {
		t.Fatal(err)
	}
	var credentials struct {
		APIKey string `json:"api_key"`
	}
	if json.Unmarshal(raw, &credentials) != nil || credentials.APIKey == "" {
		t.Fatal("selected profile credential unavailable")
	}
	profile.APIKey = credentials.APIKey
	// Match runtime profile normalization, including trailing slash handling.
	cfg := ConfigFrom(profile.Format, profile.Model, profile.BaseURL, profile.APIKey, profile.Proxy)
	cfg.Stream, cfg.ThinkingType, cfg.ReasoningEffort = profile.Streaming, profile.ThinkingType, profile.ReasoningEffort
	cfg.MaxTokens, cfg.MaxTokensField, cfg.SessionHeaderKey = profile.MaxTokens, profile.MaxTokensField, profile.SessionHeaderKey
	cfg.Retry = RetryConfig{ConnectAttempts: -1, EmptyAttempts: -1, StreamAttempts: -1}
	provider, err := cfg.NewProvider()
	if err != nil {
		t.Fatal("live provider configuration unavailable")
	}
	oldResolve, oldAugment, oldPrompt := ToolResolve, ToolAugment, PromptOverride
	ToolAugment, PromptOverride = nil, nil
	ToolResolve = func(_ context.Context, _ string, tools []actool.CoreTool) []actool.CoreTool {
		allowed := map[string]bool{"fixture_login": true, "record_fact": true, "node_detail": true, "list_facts": true, "list_findings": true, "Read": true}
		var out []actool.CoreTool
		for _, tool := range tools {
			if allowed[tool.Name()] {
				out = append(out, tool)
			}
		}
		return out
	}
	t.Cleanup(func() { ToolResolve, ToolAugment, PromptOverride = oldResolve, oldAugment, oldPrompt })
	for _, scenario := range []string{"positive", "negative", "interrupted"} {
		t.Run(scenario, func(t *testing.T) {
			measured := &liveMeasuredProvider{Provider: provider}
			d := testDB(t)
			t.Cleanup(func() { _ = d.Close() })
			goalType, goal := "inspection", "完成固定两项授权候选的登录检查，保存实际覆盖；若全部失败可用有范围的阴性结论完成。"
			if scenario == "positive" {
				goalType, goal = "outcome", "取得 fixture-user 的有效身份，并用认证后的 /me 读取证明身份。"
			}
			task, err := d.CreateTask("isolated live "+scenario, goal, nil, 0, 0)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = d.DeleteTask(task.ID) })
			es := d.Exploration(task.ExplorationID)
			g, err := es.AddGoal(map[string]any{"text": goal, "goal_type": goalType, "required_checks": []string{"仅 cand-a、cand-b 两项；授权尝试上限2；HTTP 200 不代表成功；保存未执行项与中断原因"}}, "human")
			if err != nil {
				t.Fatal(err)
			}
			var mu sync.Mutex
			requests := map[string]int{}
			fixture := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				if r.URL.Path == "/me" {
					if r.Header.Get("Authorization") != "Bearer fixture-only" {
						w.WriteHeader(401)
						fmt.Fprint(w, `{"authenticated":false}`)
						return
					}
					fmt.Fprint(w, `{"authenticated":true,"id":"fixture-user","role":"ordinary"}`)
					return
				}
				var body struct {
					Candidate string `json:"candidate"`
				}
				_ = json.NewDecoder(r.Body).Decode(&body)
				if scenario == "interrupted" && body.Candidate == "cand-b" {
					w.WriteHeader(429)
					fmt.Fprint(w, `{"error":"rate_limit","retry_after":"new authorization required"}`)
					return
				}
				if scenario == "positive" && body.Candidate == "cand-b" {
					fmt.Fprint(w, `{"success":true,"token":"fixture-only"}`)
					return
				}
				fmt.Fprint(w, `{"success":false,"error":"invalid_candidate"}`)
			}))
			t.Cleanup(fixture.Close)
			payload := map[string]any{"summary": "在隔离登录夹具完成两项固定授权候选检查。只用 fixture_login；不得外网/命令探测。工具 cand-a/cand-b 是固定候选引用，不含真实口令。每次尝试后立即用 record_fact.checkpoint 保存 request=POST /login JSON candidate，budget=2，完整 candidates（failed/remaining/verified）；success 后用工具返回的 /me 响应保存 identity 和 identity_evidence。HTTP200但success=false就是失败。429 停止，未执行项保留 remaining，result=blocked 且保存 gaps/barrier/next_action。两项均失败只能说明本集合阴性。不要重试已执行候选。", "check": map[string]any{"target": fixture.URL + "/login", "mechanism": "authentication"}, "check_key": "fixture-auth"}
			iid, err := es.AddIntent(payload, 5, nil, "human")
			if err != nil {
				t.Fatal(err)
			}
			if err = es.SetIntentState(iid, "running"); err != nil {
				t.Fatal(err)
			}
			intent, err := es.GetNode(iid)
			if err != nil {
				t.Fatal(err)
			}
			loginTool := actool.Build(actool.Spec{Name: "fixture_login", Description: "仅测试固定授权候选 cand-a/cand-b。POST /login JSON candidate；返回状态与业务响应，成功时追加 GET /me 的身份读取。先读原检查点，执行后立刻保存检查点。遇429停止。", Schema: obj(map[string]any{"candidate": str("固定候选引用 cand-a/cand-b")}, "candidate"), Run: func(ctx context.Context, input json.RawMessage, _ *actool.ToolContext) (actool.Result, error) {
				var args struct {
					Candidate string `json:"candidate"`
				}
				if json.Unmarshal(input, &args) != nil || (args.Candidate != "cand-a" && args.Candidate != "cand-b") {
					return actool.Errorf("only fixed authorized candidates"), nil
				}
				mu.Lock()
				defer mu.Unlock()
				if requests[args.Candidate] > 0 {
					return actool.Errorf("candidate already executed; resume saved checkpoint instead of retry"), nil
				}
				requests[args.Candidate]++
				req, _ := http.NewRequestWithContext(ctx, http.MethodPost, fixture.URL+"/login", strings.NewReader(string(input)))
				req.Header.Set("Content-Type", "application/json")
				resp, err := http.DefaultClient.Do(req)
				if err != nil {
					return actool.Result{}, err
				}
				body, err := io.ReadAll(resp.Body)
				resp.Body.Close()
				if err != nil {
					return actool.Result{}, err
				}
				result := fmt.Sprintf("POST /login JSON candidate=%s HTTP=%d body=%s", args.Candidate, resp.StatusCode, body)
				if strings.Contains(string(body), `"success":true`) {
					req, _ := http.NewRequestWithContext(ctx, http.MethodGet, fixture.URL+"/me", nil)
					req.Header.Set("Authorization", "Bearer fixture-only")
					identity, err := http.DefaultClient.Do(req)
					if err != nil {
						return actool.Result{}, err
					}
					me, _ := io.ReadAll(identity.Body)
					identity.Body.Close()
					result += fmt.Sprintf("; GET /me Authorization=fixture-only HTTP=%d body=%s", identity.StatusCode, me)
				}
				return actool.Text(result), nil
			}})
			ctx, cancel := context.WithTimeout(t.Context(), 3*time.Minute)
			defer cancel()
			worker := NewWorker(measured, profile.Model, t.TempDir(), nil, 64<<10, 10, loginTool)
			worker.SetNonStreaming(func() bool { return !profile.Streaming })
			worker.SetRunTimeout(2 * time.Minute)
			var activities []db.Activity
			start := time.Now()
			reason, writes, runErr := worker.Execute(ctx, "worker", task.ID, nil, es, intent, nil, func(a db.Activity) { activities = append(activities, a) }, nil, nil)
			facts, _ := es.ListByKind(db.KindFact, 100)
			var saved *db.Node
			var checkpoint struct {
				Checkpoint json.RawMessage `json:"checkpoint"`
			}
			for _, fact := range facts {
				var p struct {
					Checkpoint json.RawMessage `json:"checkpoint"`
				}
				if json.Unmarshal(fact.Payload, &p) == nil && len(p.Checkpoint) > 0 && (saved == nil || fact.ID > saved.ID) {
					saved = fact
					checkpoint = p
				}
			}
			result := map[string]any{"scenario": scenario, "task_id": task.ID, "terminal_reason": reason, "writes": writes, "worker_error": runErr != nil, "worker_elapsed_ms": time.Since(start).Milliseconds(), "requests": requests, "checkpoint": checkpoint.Checkpoint, "model": profile.Model, "format": profile.Format, "streaming": profile.Streaming, "revision": os.Getenv("ARTEX_P0_LIVE_REVISION")}
			if runErr != nil {
				result["error"] = strings.ReplaceAll(runErr.Error(), profile.APIKey, "[redacted]")
			}
			if saved != nil {
				snapshot, err := es.GoalProofSnapshot(ctx, g, saved.ID)
				if err != nil {
					t.Fatal(err)
				}
				verdict, reviewErr := NewGoalProofReviewer(measured)(ctx, snapshot, "judge only the saved evidence")
				result["review_error"], result["verdict"] = reviewErr != nil, verdict
				if reviewErr != nil {
					t.Errorf("goal reviewer failed")
				}
				if reviewErr == nil && (verdict.Supported == nil || *verdict.Supported != (scenario != "interrupted")) {
					t.Errorf("unexpected supported verdict: %+v", verdict)
				}
				if reviewErr == nil && verdict.Supported != nil && *verdict.Supported {
					if err = es.CommitGoalProof(ctx, snapshot, true, "planner", verdict.Reason); err != nil {
						t.Errorf("saved goal commit: %v", err)
					}
				}
			} else {
				t.Errorf("Worker omitted structured coverage checkpoint")
			}
			if requests["cand-a"] != 1 || requests["cand-b"] != 1 {
				t.Errorf("actual candidate coverage: %+v", requests)
			}
			if runErr != nil {
				t.Errorf("worker run failed; details in local result")
			}
			finalGoal, _ := es.GetNode(g)
			result["goal_state"], result["model_calls"], result["usage"], result["elapsed_ms"] = finalGoal.State, measured.calls, measured.usage, time.Since(start).Milliseconds()
			if output := os.Getenv("ARTEX_P0_LIVE_OUTPUT"); output != "" {
				_ = os.MkdirAll(output, 0700)
				raw, _ := json.MarshalIndent(result, "", "  ")
				filename := fmt.Sprintf("%s-%d.json", scenario, start.UnixNano())
				_ = os.WriteFile(filepath.Join(output, filename), raw, 0600)
				trace, _ := json.Marshal(activities)
				_ = os.WriteFile(filepath.Join(output, filename+".trace.json"), trace, 0600)
			}
		})
	}
}
