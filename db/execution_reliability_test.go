package db

import (
	"context"
	"errors"
	"regexp"
	"testing"
)

func TestExecutionHydraMigrationPreservesApprovalPolicy(t *testing.T) {
	d, err := Open(testDSN(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.Close() })
	const name = "[内置] 口令喷洒/爆破: hydra"
	var id int64
	var pattern, action string
	var enabled, timeout bool
	if err := d.QueryRow(`SELECT id,pattern,action,enabled,timeout_enabled FROM intercept_rules WHERE name=$1`, name).Scan(&id, &pattern, &action, &enabled, &timeout); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = d.Exec(`UPDATE intercept_rules SET pattern=$1,action=$2,enabled=$3,timeout_enabled=$4 WHERE id=$5`, pattern, action, enabled, timeout, id)
	})
	if _, err := d.Exec(`UPDATE intercept_rules SET pattern=$1,action='ask',enabled=false,timeout_enabled=true WHERE id=$2`, `(?i)\bhydra\b`, id); err != nil {
		t.Fatal(err)
	}
	if err := d.repairHydraNamespaceRule(); err != nil {
		t.Fatal(err)
	}
	var saved string
	var on, wait bool
	if err := d.QueryRow(`SELECT pattern,enabled,timeout_enabled FROM intercept_rules WHERE id=$1`, id).Scan(&saved, &on, &wait); err != nil {
		t.Fatal(err)
	}
	if saved != hydraCommandPattern || on || !wait {
		t.Fatal("migration reset approval preferences")
	}
	re := regexp.MustCompile(saved)
	for _, input := range []string{`{"command":"python -c 'print(d[\"hydra:member\"])'"}`, `{"content":"count=d.get('hydra:totalItems')"}`} {
		if re.MatchString(input) {
			t.Fatal("namespace data still triggers", input)
		}
	}
	for _, input := range []string{`{"command":"hydra"}`, `{"command":"/opt/bin/hydra -h"}`, `{"content":"subprocess.run(['hydra','-h'])"}`, `{"command":"echo hydra:member; hydra -h"}`} {
		if !re.MatchString(input) {
			t.Fatal("real command no longer triggers", input)
		}
	}
	if _, err := d.Exec(`UPDATE intercept_rules SET pattern='custom_hydra_rule' WHERE id=$1`, id); err != nil {
		t.Fatal(err)
	}
	if err := d.repairHydraNamespaceRule(); err != nil {
		t.Fatal(err)
	}
	_ = d.QueryRow(`SELECT pattern FROM intercept_rules WHERE id=$1`, id).Scan(&saved)
	if saved != "custom_hydra_rule" {
		t.Fatal("custom matcher overwritten")
	}
}

func TestExecutionOutcomeGenerationCannotSettleNewRun(t *testing.T) {
	d, err := Open(testDSN(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.Close() })
	exp, err := d.CreateExploration("outcome generation", "fixture")
	if err != nil {
		t.Fatal(err)
	}
	defer d.Exec(`DELETE FROM explorations WHERE id=$1`, exp)
	s := d.Exploration(exp)
	id, err := s.AddIntent(map[string]any{"summary": "fixture"}, 5, nil, "test")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.ClaimIntent(id, "worker"); err != nil {
		t.Fatal(err)
	}
	first, err := s.BindIntentRun(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SetIntentOutcome(id, "blocked", "fixture", "fixture", first); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ReopenIntent(id); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ClaimIntent(id, "worker"); err != nil {
		t.Fatal(err)
	}
	second, err := s.BindIntentRun(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	if first == second {
		t.Fatal("run generation reused")
	}
	if err := s.SetIntentOutcome(id, "done", "completed", "late", first); !errors.Is(err, ErrIntentStateConflict) {
		t.Fatalf("late completion: %v", err)
	}
	if n, _ := s.GetNode(id); n.State != "running" {
		t.Fatal("late result settled rerun")
	}
	if _, err := s.ResetRunningIntents(); err != nil {
		t.Fatal(err)
	}
	if n, _ := s.GetNode(id); n.State != "open" {
		t.Fatal("startup transcript recovery changed", n)
	}
}
