package server

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"iter"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Autumn-27/artex/agent"
	"github.com/Autumn-27/artex/db"
	"github.com/Autumn-27/artex/llmpool"
	"github.com/Autumn-27/norma/llm"
	"github.com/Autumn-27/norma/transcript"
)

type chatWiringCounterProvider struct{ calls atomic.Int64 }

func (p *chatWiringCounterProvider) Complete(context.Context, llm.CompletionRequest) (llm.Message, string, llm.Usage, error) {
	p.calls.Add(1)
	return llm.Message{}, "", llm.Usage{}, errors.New("synthetic global provider must not be called")
}

func (p *chatWiringCounterProvider) Stream(context.Context, llm.CompletionRequest) iter.Seq2[llm.StreamEvent, error] {
	return func(yield func(llm.StreamEvent, error) bool) {
		p.calls.Add(1)
		yield(llm.StreamEvent{}, errors.New("synthetic global provider must not be called"))
	}
}

func chatProfileWiringFixture(t *testing.T) (*Server, *agent.ChatAgent, *chatWiringCounterProvider, string) {
	t.Helper()
	m, err := NewManager(t.TempDir(), "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = m.Close() })
	role := fmt.Sprintf("chat_wiring_%d", time.Now().UnixNano())
	a, err := m.pg.CreateAgent(role, "chat profile wiring fixture", "synthetic route selection")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = m.pg.DeleteAgent(a.Key) })
	counter := &chatWiringCounterProvider{}
	dir := t.TempDir()
	global := agent.NewChatAgent(counter, "global-fixture", dir, transcript.NewStore(dir), 0)
	s := &Server{
		m: m, chatAgent: global,
		profChatAgents:       map[int64]*agent.ChatAgent{},
		provByProfile:        map[int64]*provEntry{},
		taskAgents:           map[string]*taskAgentBundle{},
		llmHealth:            llmpool.NewRegistry(nil, nil),
		modelInstructionsDir: t.TempDir(),
	}
	return s, global, counter, role
}

func saveChatWiringProfile(t *testing.T, s *Server, p *db.LLMProfile) int64 {
	t.Helper()
	id, err := s.m.pg.SaveProfile(p)
	if err != nil {
		t.Fatal(err)
	}
	pg := s.m.pg
	t.Cleanup(func() { _ = pg.DeleteProfile(id) })
	return id
}

func TestChatProfileWiringMissingAndKeylessSelectionNeverUsesGlobal(t *testing.T) {
	s, global, counter, role := chatProfileWiringFixture(t)
	var requests atomic.Int64
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(upstream.Close)
	keyless := saveChatWiringProfile(t, s, &db.LLMProfile{Name: role + "_keyless", Format: "openai", Model: "fixture", BaseURL: upstream.URL + "/v1"})
	missing := int64(9223372036854775000)
	for _, id := range []int64{missing, keyless} {
		conv := &db.Conversation{AgentKey: role, LLMProfileID: &id}
		if got := s.resolveChatAgent(conv); got != nil {
			t.Fatalf("explicit unavailable profile %d resolved another agent: isGlobal=%v", id, got == global)
		}
		reason := s.chatUnavailableReason(conv)
		if !strings.Contains(reason, fmt.Sprintf("#%d", id)) || !strings.Contains(reason, "未改用全局模型") {
			t.Fatalf("selected profile error lost its routing identity: %q", reason)
		}
	}
	if s.resolveChatAgent(&db.Conversation{AgentKey: role}) != global {
		t.Fatal("genuinely unselected conversation lost its existing global route")
	}
	if counter.calls.Load() != 0 || requests.Load() != 0 {
		t.Fatal("resolving an unavailable profile sent a model request")
	}
}

func TestChatProfileWiringDeletedRoleDoesNotBecomeUnbound(t *testing.T) {
	s, _, counter, role := chatProfileWiringFixture(t)
	if err := s.m.pg.DeleteAgent(role); err != nil {
		t.Fatal(err)
	}
	conv := &db.Conversation{AgentKey: role}
	if got := s.resolveChatAgent(conv); got != nil {
		t.Fatal("deleted Agent silently selected the global chat provider")
	}
	if reason := s.chatUnavailableReason(conv); !strings.Contains(reason, "已不存在") || !strings.Contains(reason, "未改用全局模型") {
		t.Fatalf("deleted role reason is not explicit: %q", reason)
	}
	if counter.calls.Load() != 0 {
		t.Fatal("deleted role invoked the global model")
	}
}

func TestChatProfileWiringAgentBindingRemainsAuthoritative(t *testing.T) {
	s, global, counter, role := chatProfileWiringFixture(t)
	bound := saveChatWiringProfile(t, s, &db.LLMProfile{Name: role + "_bound", Format: "openai", Model: "fixture"})
	pinned := saveChatWiringProfile(t, s, &db.LLMProfile{Name: role + "_pinned", Format: "openai", Model: "fixture", APIKey: "fixture-key"})
	pinnedAgent := agent.NewChatAgent(agent.ErrorProvider(errors.New("synthetic pinned provider")), "pinned-fixture", t.TempDir(), nil, 0)
	s.profChatAgents[pinned] = pinnedAgent
	if err := s.m.pg.SetAgentLLMProfile(role, &bound); err != nil {
		t.Fatal(err)
	}
	conv := &db.Conversation{AgentKey: role, LLMProfileID: &pinned}
	if got := s.resolveChatAgent(conv); got != nil {
		t.Fatalf("invalid Agent binding fell through to pin/global: pin=%v global=%v", got == pinnedAgent, got == global)
	}
	if reason := s.chatUnavailableReason(conv); !strings.Contains(reason, fmt.Sprintf("#%d", bound)) {
		t.Fatalf("binding failure was attributed to wrong profile: %s", reason)
	}
	if err := s.m.pg.SetAgentLLMProfile(role, nil); err != nil {
		t.Fatal(err)
	}
	if s.resolveChatAgent(conv) != pinnedAgent {
		t.Fatal("removing a binding did not restore the explicit conversation pin")
	}
	if counter.calls.Load() != 0 {
		t.Fatal("selection checks invoked the global provider")
	}
}

func TestChatProfileWiringBindingReadFailureIsNotUnselected(t *testing.T) {
	s, _, counter, role := chatProfileWiringFixture(t)
	closed, err := sql.Open("pgx", "postgres://chat-wiring-fixture.invalid/unused")
	if err != nil {
		t.Fatal(err)
	}
	_ = closed.Close()
	s.m = &Manager{pg: &db.DB{DB: closed}}
	conv := &db.Conversation{AgentKey: role}
	if s.resolveChatAgent(conv) != nil {
		t.Fatal("unreadable binding silently selected the global agent")
	}
	if reason := s.chatUnavailableReason(conv); !strings.Contains(reason, "绑定读取失败") || !strings.Contains(reason, "未改用全局模型") {
		t.Fatalf("binding storage failure was hidden: %s", reason)
	}
	if counter.calls.Load() != 0 {
		t.Fatal("unreadable binding invoked a model")
	}
}

func TestChatProfileWiringLocalConfigurationErrorKeepsSelectedRoute(t *testing.T) {
	s, global, counter, role := chatProfileWiringFixture(t)
	for _, key := range []string{settingLLMPoolOn, settingLLMPoolBindFallback} {
		before, found, err := s.m.pg.GetSetting(key)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			if found {
				_ = s.m.pg.SetSetting(key, before)
			} else {
				_ = s.m.pg.DeleteSetting(key)
			}
		})
		if err := s.m.pg.SetSetting(key, "true"); err != nil {
			t.Fatal(err)
		}
	}
	var requests atomic.Int64
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(upstream.Close)
	id := saveChatWiringProfile(t, s, &db.LLMProfile{
		Name: role + "_config_error", Format: "openai-responses", Model: "fixture", APIKey: "fixture-key",
		BaseURL: upstream.URL + "/v1", UserAgent: "codex", ModelInstructionsFile: "missing-fixture.md",
		ModelInstructionsFiles: []string{"missing-fixture.md"},
	})
	saveChatWiringProfile(t, s, &db.LLMProfile{Name: role + "_backup", Format: "openai", Model: "fixture", APIKey: "fixture-backup-key", BaseURL: upstream.URL + "/v1"})
	conv := &db.Conversation{AgentKey: role, LLMProfileID: &id}
	selected := s.resolveChatAgent(conv)
	if selected == nil || selected == global {
		t.Fatal("local configuration error discarded the explicit selection")
	}
	provider, cfg, ok := s.providerForProfile(id)
	if !ok || cfg.ConfigError == nil || provider == nil {
		t.Fatal("fixture must produce a selected local ErrorProvider")
	}
	if _, _, _, err := s.poolForBinding(id, provider, cfg).Complete(t.Context(), llm.CompletionRequest{}); err == nil || !strings.Contains(err.Error(), "missing-fixture.md") {
		t.Fatalf("local instruction error silently became a pool request: %v", err)
	}
	if requests.Load() != 0 || counter.calls.Load() != 0 {
		t.Fatal("local configuration error invoked selected/fallback/global HTTP")
	}
}
