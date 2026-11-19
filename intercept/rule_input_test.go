package intercept

import (
	"regexp"
	"testing"

	"github.com/Autumn-27/artex/db"
)

func TestDecodedToolInputEnforcesDenyWithoutExpandingOtherRules(t *testing.T) {
	deny := compiledRule{
		InterceptRule: db.InterceptRule{Name: "deny", Action: "deny", MatchTarget: "tool_input", MatchType: "regex", Pattern: "(?m)^notice$"},
		re:            regexp.MustCompile("(?m)^notice$"),
	}
	allow, ask := deny, deny
	allow.Name, allow.Action = "allow", "allow"
	ask.Name, ask.Action = "ask", "ask"
	rawAllow := compiledRule{InterceptRule: db.InterceptRule{Name: "raw-allow", Action: "allow", MatchTarget: "tool_input", MatchType: "contains", Pattern: "labels"}}

	input := []byte("{\"labels\":[\"alpha\\nnotice\"],\"total\":1e999}")
	for _, c := range []struct {
		name  string
		rules []compiledRule
		want  string
	}{
		{"decoded deny after permission rules", []compiledRule{allow, ask, deny}, "deny"},
		{"decoded values cannot grant permission", []compiledRule{allow, ask}, ""},
		{"existing raw priority is preserved", []compiledRule{rawAllow, deny}, "raw-allow"},
	} {
		t.Run(c.name, func(t *testing.T) {
			rule, matched := firstMatchingRule(c.rules, "sample", input)
			if rule.Name != c.want || matched != (c.want != "") {
				t.Fatalf("rule = %q, matched = %v, want %q", rule.Name, matched, c.want)
			}
		})
	}
}
