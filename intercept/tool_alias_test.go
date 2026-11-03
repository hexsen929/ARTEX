package intercept

import (
	"reflect"
	"regexp"
	"testing"

	"github.com/Autumn-27/artex/db"
)

func TestDeepSeekApprovalAliasesRespectExistingScopeChoices(t *testing.T) {
	for _, tc := range []struct {
		name, saved   string
		fetch, search bool
	}{
		{"defaults", "", true, true},
		{"explicit off", `[]`, false, false},
		{"fetch only", `["WebFetch"]`, true, false},
		{"search only", `["web_search"]`, false, true},
		{"unrelated only", `["Bash"]`, false, false},
		{"alias input", `["deepseek_fetch","deepseek_search"]`, true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			settings := map[string]string{}
			if tc.saved != "" {
				settings["intercept_enabled_tools"] = tc.saved
			}
			i := retryTestInterceptor(t, settings)
			if _, err := i.db.Exec(`CREATE TABLE intercept_rules (
 id INTEGER, name TEXT, enabled BOOLEAN, priority INTEGER, match_target TEXT, match_type TEXT,
 pattern TEXT, action TEXT, message TEXT, timeout_enabled BOOLEAN, timeout_seconds INTEGER,
 timeout_action TEXT, builtin BOOLEAN, created_at TIMESTAMP, updated_at TIMESTAMP)`); err != nil {
				t.Fatal(err)
			}
			for _, tool := range []struct {
				name string
				want bool
			}{
				{"WebFetch", tc.fetch}, {"deepseek_fetch", tc.fetch},
				{"web_search", tc.search}, {"deepseek_search", tc.search},
			} {
				if got := i.IsToolEnabled(tool.name); got != tool.want {
					t.Fatalf("%s enabled=%v want=%v", tool.name, got, tool.want)
				}
			}
			if tc.name == "explicit off" {
				names, err := i.GetEnabledTools()
				if err != nil || len(names) != 0 {
					t.Fatal("explicit empty choices were reset", names, err)
				}
			}
		})
	}
}

func TestDeepSeekApprovalAliasSaveRoundTripUsesExistingUISwitches(t *testing.T) {
	d := openTestDB(t)
	old, exists, err := d.GetSetting("intercept_enabled_tools")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if exists {
			_ = d.SetSetting("intercept_enabled_tools", old)
		} else {
			_ = d.DeleteSetting("intercept_enabled_tools")
		}
	})
	i := New(d)
	if err := i.SetEnabledTools([]string{"deepseek_fetch", "WebFetch", "deepseek_search", "web_search", "custom_fixture"}); err != nil {
		t.Fatal(err)
	}
	got, err := i.GetEnabledTools()
	want := []string{"WebFetch", "web_search", "custom_fixture"}
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("aliases became hidden independent controls: %v %v", got, err)
	}
}

func TestDeepSeekNamesMatchExistingRulesAndKeepExplicitRules(t *testing.T) {
	for _, names := range [][2]string{{"WebFetch", "deepseek_fetch"}, {"web_search", "deepseek_search"}} {
		for _, kind := range []string{"regex", "contains"} {
			for _, patternName := range names {
				pattern := patternName
				if kind == "regex" {
					pattern = "^" + regexp.QuoteMeta(patternName) + "$"
				}
				rule := compiledRule{InterceptRule: db.InterceptRule{MatchTarget: "tool_name", MatchType: kind, Pattern: pattern}}
				if kind == "regex" {
					rule.re = regexp.MustCompile(pattern)
				}
				if !ruleMatches(rule, names[1], []byte(`{"url":"https://fixture.invalid"}`)) {
					t.Fatalf("%s rule for %s skipped %s", kind, patternName, names[1])
				}
			}
		}
	}
	// Aliasing names must never rewrite arguments or broaden input-pattern rules.
	rule := compiledRule{InterceptRule: db.InterceptRule{MatchTarget: "tool_input", MatchType: "contains", Pattern: "WebFetch"}}
	if ruleMatches(rule, "deepseek_fetch", []byte(`{"url":"https://fixture.invalid"}`)) {
		t.Fatal("name alias contaminated tool-input matching")
	}
}
