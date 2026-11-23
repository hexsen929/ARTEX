package db

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"
)

func TestTaskArchiveApprovalsSurviveDeletedRules(t *testing.T) {
	for _, version := range []int{5, TaskArchiveFormatVersion} {
		t.Run(fmt.Sprintf("format-%d", version), func(t *testing.T) {
			f := newRetestArchiveFixture(t)
			makeRule := func(name string) InterceptRule {
				t.Helper()
				rule, err := f.d.CreateInterceptRule(name, "tool_name", "regex", "^FixtureOnly$", "deny", "synthetic rule", 0, false, true, 60, "deny")
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _, _ = f.d.Exec(`DELETE FROM intercept_rules WHERE id=$1`, rule.ID) })
				return rule
			}
			deletedRule := makeRule("archive-deleted-rule-fixture")
			liveRule := makeRule("archive-surviving-rule-fixture")
			makeApproval := func(ruleID int64, status string, audit *InterceptAudit) int64 {
				t.Helper()
				id, err := f.d.CreateInterceptPending(ruleID, f.conversation.ID, fmt.Sprint(f.task.ID), "fixture", "FixtureOnly", []byte(`{}`), "synthetic decision", audit)
				if err != nil {
					t.Fatal(err)
				}
				if _, err := f.d.Exec(`UPDATE intercept_pending SET status=$2 WHERE id=$1`, id, status); err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _, _ = f.d.Exec(`DELETE FROM intercept_pending WHERE id=$1`, id) })
				return id
			}
			audit := &InterceptAudit{RuleName: deletedRule.Name, ConfigDigest: "synthetic-rule-snapshot", InitialAction: "deny", InitialReason: "synthetic original reason", EffectiveAction: "deny", ExecutionStatus: "not_executed"}
			denied := makeApproval(deletedRule.ID, "denied", audit)
			pending := makeApproval(deletedRule.ID, "pending", nil)
			kept := makeApproval(liveRule.ID, "denied", nil)
			unbound := makeApproval(0, "denied", nil)
			if _, err := f.d.Exec(`UPDATE intercept_pending SET decision_source='' WHERE id=$1`, pending); err != nil {
				t.Fatal(err)
			}
			a, snapshot := f.snapshot(t)
			if snapshot.DataCounts["intercept_pending"] != 4 {
				t.Fatal("synthetic approvals were not archived")
			}
			if err := f.d.CompleteTaskArchive(a.ID, snapshot, "synthetic-package", "synthetic-digest", 100, 50); err != nil {
				t.Fatal(err)
			}
			if _, err := f.d.Exec(`DELETE FROM intercept_rules WHERE id=$1`, deletedRule.ID); err != nil {
				t.Fatal(err)
			}
			// Existing v5 archives can contain the same rule IDs even though the
			// global rule was removed after the task was archived.
			snapshot.FormatVersion = version
			warnings := f.restore(t, a, snapshot)
			found := false
			for _, warning := range warnings {
				if strings.Contains(warning, fmt.Sprintf("审批规则 %d 已删除，2 条历史审批", deletedRule.ID)) {
					found = true
				}
			}
			if !found {
				t.Fatalf("missing rule downgrade was not reported: %v", warnings)
			}
			history, err := f.d.GetInterceptDetail(denied)
			if err != nil || history == nil || history.RuleID != nil || history.Status != "denied" || history.DecisionSource != "rule" || !reflect.DeepEqual(history.Audit, audit) {
				t.Fatal("deleted rule changed the retained decision or snapshot", err)
			}
			legacy, err := f.d.GetInterceptDetail(pending)
			if err != nil || legacy == nil || legacy.RuleID != nil || legacy.Status != "timeout" || legacy.DecisionSource != "rule" || legacy.Audit != nil {
				t.Fatal("pending legacy approval was revived or given a fabricated audit", err)
			}
			live, err := f.d.GetInterceptDetail(kept)
			if err != nil || live == nil || live.RuleID == nil || *live.RuleID != liveRule.ID || live.Status != "denied" {
				t.Fatal("surviving rule association was lost", err)
			}
			plain, err := f.d.GetInterceptDetail(unbound)
			if err != nil || plain == nil || plain.RuleID != nil || plain.Status != "denied" {
				t.Fatal("unbound history changed", err)
			}
			var exists bool
			if err := f.d.QueryRow(`SELECT EXISTS(SELECT 1 FROM intercept_rules WHERE id=$1)`, deletedRule.ID).Scan(&exists); err != nil || exists {
				t.Fatal("restore recreated a deleted global rule", err)
			}
		})
	}
}

func TestRestoreInterceptRejectsMalformedRuleReferences(t *testing.T) {
	d := portTestDB(t)
	for _, value := range []any{"not-a-rule", 1.5, -1} {
		tx, err := d.Begin()
		if err != nil {
			t.Fatal(err)
		}
		raw, err := json.Marshal([]map[string]any{{"rule_id": value}})
		if err != nil {
			_ = tx.Rollback()
			t.Fatal(err)
		}
		_, restoreErr := restoreInterceptRows(tx, raw)
		_ = tx.Rollback()
		if restoreErr == nil {
			t.Fatal("malformed archived rule ID was silently discarded")
		}
	}
}
