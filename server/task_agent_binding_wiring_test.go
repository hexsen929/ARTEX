package server

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/Autumn-27/artex/agent"
	"github.com/Autumn-27/artex/db"
	"github.com/Autumn-27/artex/llmpool"
	"github.com/Autumn-27/norma/harness"
	"github.com/Autumn-27/norma/llm"
)

func preserveTaskBindingWiringSetting(t *testing.T, s *Server, key, value string) {
	t.Helper()
	prior, existed, err := s.m.pg.GetSetting(key)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if existed {
			_ = s.m.pg.SetSetting(key, prior)
		} else {
			_ = s.m.pg.DeleteSetting(key)
		}
	})
	if err := s.m.pg.SetSetting(key, value); err != nil {
		t.Fatal(err)
	}
}

func TestTaskAgentBindingWiringDistinguishesAbsenceFromSelectionFailure(t *testing.T) {
	id := int64(81)
	for _, tc := range []struct {
		name       string
		selected   *int64
		selectErr  error
		ready      bool
		configErr  error
		wantErr    bool
		wantLookup bool
	}{
		{name: "unbound"},
		{name: "binding_read_failed", selectErr: errors.New("fixture read failed"), wantErr: true},
		{name: "selected_missing", selected: &id, wantErr: true, wantLookup: true},
		{name: "selected_keyless", selected: &id, wantErr: true, wantLookup: true},
		{name: "selected_local_config_failed", selected: &id, ready: true, configErr: errors.New("fixture configuration failed"), wantErr: true, wantLookup: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			lookups, pools := 0, 0
			provider, _, bound, err := resolveAgentBindingProvider("worker", func() (*int64, error) { return tc.selected, tc.selectErr },
				func(profile int64) (llm.Provider, agent.Config, bool) {
					lookups++
					if profile != id {
						t.Fatal("explicit profile changed")
					}
					return agent.ErrorProvider(errors.New("fixture provider must not run")), agent.Config{ConfigError: tc.configErr}, tc.ready
				}, func(int64, llm.Provider, agent.Config) llm.Provider {
					pools++
					t.Fatal("unavailable selection entered a fallback pool")
					return nil
				})
			if (err != nil) != tc.wantErr || provider != nil || pools != 0 || (lookups > 0) != tc.wantLookup || bound != (tc.selected != nil) {
				t.Fatalf("provider=%v bound=%v lookup=%d pools=%d err=%v", provider, bound, lookups, pools, err)
			}
		})
	}
}

func TestTaskAgentBindingWiringInvalidBindingsNeverUseTaskChainOrGlobal(t *testing.T) {
	s, _, global, role := chatProfileWiringFixture(t)
	s.llmOn, s.llmProv, s.llmCfg = true, global, agent.Config{Model: "global-fixture", MaxTokens: 777}
	for _, key := range []string{settingLLMPoolOn, settingLLMPoolBindFallback} {
		preserveTaskBindingWiringSetting(t, s, key, "true")
	}
	var requests atomic.Int64
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(upstream.Close)
	chainID := saveChatWiringProfile(t, s, &db.LLMProfile{Name: role + "_task_chain", Format: "openai", Model: "fixture", APIKey: "fixture-key", BaseURL: upstream.URL + "/v1"})
	chain := &scriptedLLMProvider{events: []llm.StreamEvent{{Type: llm.SETextDelta, Text: "fixture"}, {Type: llm.SEMessageStop}}}
	s.provByProfile[chainID] = &provEntry{prov: chain, cfg: agent.Config{Model: "chain-fixture", MaxTokens: 655}}
	task, err := s.m.CreateTask("binding wiring fixture", "synthetic configuration only", nil, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = s.m.DeleteTask(task.ID, DeleteTaskOptions{}) })
	taskID, _ := strconv.ParseInt(task.ID, 10, 64)
	if err := s.m.pg.ReplaceTaskLLMProfiles(taskID, []int64{chainID}, chainID); err != nil {
		t.Fatal(err)
	}
	before, err := s.m.pg.TaskLLMProfiles(taskID)
	if err != nil {
		t.Fatal(err)
	}
	runtime := &taskLLMRuntime{s: s, taskID: task.ID, agentKey: role}
	for _, profile := range []*db.LLMProfile{
		{Name: role + "_task_keyless", Format: "openai", Model: "fixture", BaseURL: upstream.URL + "/v1"},
		{Name: role + "_task_bad_proxy", Format: "openai", Model: "fixture", BaseURL: upstream.URL + "/v1", APIKey: "fixture-key", Proxy: "fixture-invalid://proxy.invalid"},
		{Name: role + "_task_bad_instructions", Format: "openai-responses", Model: "fixture", BaseURL: upstream.URL + "/v1", APIKey: "fixture-key", UserAgent: "codex", ModelInstructionsFile: "missing-task-fixture.md", ModelInstructionsFiles: []string{"missing-task-fixture.md"}},
	} {
		t.Run(profile.Name, func(t *testing.T) {
			id := saveChatWiringProfile(t, s, profile)
			if _, err := s.m.pg.Exec(`UPDATE agents SET llm_profile_id=$2 WHERE key=$1`, role, id); err != nil {
				t.Fatal(err)
			}
			_, err := runtime.current()
			if err == nil || !isTaskLLMRuntimeError(err) || !strings.Contains(err.Error(), "未改用") || retryableWorkerModelError(harness.ReasonModelError, err) {
				t.Fatalf("binding failure was not router-owned: %v", err)
			}
			if _, ok := runtime.activeCfg(); ok || s.taskRuntimeAvailable(task, role) {
				t.Fatal("unusable explicit binding was reported ready")
			}
			_, _, _, completeErr := runtime.Complete(t.Context(), llm.CompletionRequest{})
			_, streamErr := collectTaskLLMStream(runtime.Stream(t.Context(), llm.CompletionRequest{}))
			if !isTaskLLMRuntimeError(completeErr) || !isTaskLLMRuntimeError(streamErr) {
				t.Fatalf("model paths discarded route ownership: complete=%v stream=%v", completeErr, streamErr)
			}
			provider, cfg, selected := s.agentBindingProvider(role)
			if !selected || cfg.ConfigError == nil || provider == nil {
				t.Fatal("compatibility caller would fall back to global")
			}
			if _, _, _, err := provider.Complete(t.Context(), llm.CompletionRequest{}); err == nil {
				t.Fatal("selected error provider hid configuration failure")
			}
			if requests.Load() != 0 || global.calls.Load() != 0 || chain.calls != 0 {
				t.Fatalf("invalid route invoked a model: HTTP=%d global=%d chain=%d", requests.Load(), global.calls.Load(), chain.calls)
			}
		})
	}
	after, err := s.m.pg.TaskLLMProfiles(taskID)
	if err != nil || !reflect.DeepEqual(before, after) {
		t.Fatalf("binding failure changed the task's failover chain: %v", err)
	}
}

func TestTaskAgentBindingWiringReadFailureIsRouterOwned(t *testing.T) {
	closed, err := sql.Open("pgx", "postgres://binding-wiring.invalid/unused")
	if err != nil {
		t.Fatal(err)
	}
	_ = closed.Close()
	global := &chatWiringCounterProvider{}
	s := &Server{m: &Manager{pg: &db.DB{DB: closed}}, llmOn: true, llmProv: global}
	if _, _, _, err := s.agentBindingProviderChecked("worker"); err == nil {
		t.Fatal("unreadable binding became unselected")
	}
	provider, cfg, selected := s.agentBindingProvider("worker")
	if !selected || provider == nil || cfg.ConfigError == nil {
		t.Fatal("compatibility path would select global on storage failure")
	}
	if _, _, _, err := provider.Complete(context.Background(), llm.CompletionRequest{}); err == nil {
		t.Fatal("binding lookup failure was not retained")
	}
	runtime := &taskLLMRuntime{s: s, taskID: "1", agentKey: "worker"}
	_, err = runtime.current()
	if !isTaskLLMRuntimeError(err) || retryableWorkerModelError(harness.ReasonModelError, err) || global.calls.Load() != 0 {
		t.Fatalf("storage failure could replay or use global: %v calls=%d", err, global.calls.Load())
	}
}

func TestTaskAgentBindingWiringUnboundAndValidRoutesKeepPrecedence(t *testing.T) {
	s, _, global, role := chatProfileWiringFixture(t)
	s.llmOn, s.llmProv, s.llmCfg = true, global, agent.Config{Model: "global-fixture", MaxTokens: 777}
	for _, key := range []string{settingLLMPoolOn, settingLLMPoolBindFallback} {
		preserveTaskBindingWiringSetting(t, s, key, "false")
	}
	task, err := s.m.CreateTask("binding precedence fixture", "synthetic data only", nil, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = s.m.DeleteTask(task.ID, DeleteTaskOptions{}) })
	runtime := &taskLLMRuntime{s: s, taskID: task.ID, agentKey: role}
	selected, err := runtime.current()
	if err != nil || selected.provider != global || selected.profileID != 0 {
		t.Fatalf("unbound legacy global route changed: %+v %v", selected, err)
	}
	chainID := saveChatWiringProfile(t, s, &db.LLMProfile{Name: role + "_valid_chain", Format: "openai", Model: "fixture", APIKey: "fixture-key"})
	boundID := saveChatWiringProfile(t, s, &db.LLMProfile{Name: role + "_valid_bound", Format: "openai", Model: "fixture", APIKey: "fixture-key"})
	chain, bound := &scriptedLLMProvider{}, &scriptedLLMProvider{}
	s.provByProfile[chainID] = &provEntry{prov: chain, cfg: agent.Config{Model: "chain-fixture", MaxTokens: 655}}
	s.provByProfile[boundID] = &provEntry{prov: bound, cfg: agent.Config{Model: "bound-fixture", MaxTokens: 17}}
	taskID, _ := strconv.ParseInt(task.ID, 10, 64)
	if err := s.m.pg.ReplaceTaskLLMProfiles(taskID, []int64{chainID}, chainID); err != nil {
		t.Fatal(err)
	}
	selected, err = runtime.current()
	if err != nil || selected.provider != chain || selected.profileID != chainID {
		t.Fatalf("unbound task chain route changed: %+v %v", selected, err)
	}
	if _, err := s.m.pg.Exec(`UPDATE agents SET llm_profile_id=$2 WHERE key=$1`, role, boundID); err != nil {
		t.Fatal(err)
	}
	selected, err = runtime.current()
	if err != nil || selected.provider != bound || selected.profileID != 0 || selected.retry != (agent.RetryConfig{}) {
		t.Fatalf("valid binding did not win over task chain/global: %+v %v", selected, err)
	}
	if cfg, ok := runtime.activeCfg(); !ok || cfg.MaxTokens != 17 {
		t.Fatalf("valid source configuration changed: cfg=%+v ok=%v", cfg, ok)
	}
	if !s.taskRuntimeAvailable(task, role) || global.calls.Load() != 0 || chain.calls != 0 || bound.calls != 0 {
		t.Fatal("route selection performed model work or lost readiness")
	}
	// The valid binding still follows the existing explicit two-switch policy.
	if err := s.m.pg.SetSetting(settingLLMPoolOn, "true"); err != nil {
		t.Fatal(err)
	}
	if provider, _, _, err := s.agentBindingProviderChecked(role); err != nil || provider != bound {
		t.Fatal("enabling only the master switch changed an exclusive binding")
	}
	if err := s.m.pg.SetSetting(settingLLMPoolBindFallback, "true"); err != nil {
		t.Fatal(err)
	}
	provider, _, selectedBinding, err := s.agentBindingProviderChecked(role)
	if _, isPool := provider.(*llmpool.Pool); err != nil || !selectedBinding || !isPool {
		t.Fatalf("explicit valid binding fallback policy stopped working: pool=%v selected=%v err=%v", isPool, selectedBinding, err)
	}
}
