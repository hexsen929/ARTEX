package db

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"
	"time"
)

// TestPoolProfilesOrder pins the failover chain query: keyless profiles can't
// serve a request and excluded ones aren't fallback targets, so neither belongs
// in the chain; the rest come back by priority, highest first.
// Deliberately does NOT touch is_default — flipping the active profile would be a
// side effect on the shared dev database.
func TestPoolProfilesOrder(t *testing.T) {
	d, err := Open(testDSN(t))
	if err != nil {
		t.Skipf("postgres unavailable (%v) — skipping", err)
	}
	defer d.Close()

	mk := func(name string, priority int, exclude bool, key string) int64 {
		id, err := d.SaveProfile(&LLMProfile{
			Name: name, Format: "openai", Model: "m", APIKey: key,
			Priority: priority, PoolExclude: exclude,
		})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { d.Exec(`DELETE FROM llm_profiles WHERE id=$1`, id) })
		return id
	}
	lo := mk("t-pool-lo", 1, false, "k1")
	hi := mk("t-pool-hi", 9, false, "k2")
	mk("t-pool-excluded", 99, true, "k3") // excluded despite the top priority
	mk("t-pool-nokey", 50, false, "")     // no key → cannot serve anything

	chain, err := d.PoolProfiles()
	if err != nil {
		t.Fatal(err)
	}
	var got []int64
	for _, p := range chain {
		switch p.Name {
		case "t-pool-lo", "t-pool-hi":
			got = append(got, p.ID)
		case "t-pool-excluded":
			t.Fatal("pool_exclude profile entered the failover chain")
		case "t-pool-nokey":
			t.Fatal("keyless profile entered the failover chain")
		}
	}
	if len(got) != 2 || got[0] != hi || got[1] != lo {
		t.Fatalf("chain order = %v, want [hi=%d lo=%d]", got, hi, lo)
	}
}

func TestDeleteProfileContextHonorsCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	var d DB
	if err := d.DeleteProfileContext(ctx, 1); !errors.Is(err, context.Canceled) {
		t.Fatalf("DeleteProfileContext error=%v, want context cancellation", err)
	}
}

func TestConfigStores(t *testing.T) {
	d, err := Open(testDSN(t))
	if err != nil {
		t.Skipf("postgres unavailable (%v) — skipping", err)
	}
	defer d.Close()

	// LLM profile: save, active, key never serialized
	pid, err := d.SaveProfile(&LLMProfile{Name: "t-default", Format: "openai", Model: "gpt-x", APIKey: "secret123"})
	if err != nil {
		t.Fatal(err)
	}
	defer d.Exec(`DELETE FROM llm_profiles WHERE id=$1`, pid)
	if err := d.SetActiveProfile(pid); err != nil {
		t.Fatal(err)
	}
	if err := d.DeleteProfile(pid); !errors.Is(err, ErrActiveLLMProfileDelete) {
		t.Fatalf("deleting active profile error=%v, want %v", err, ErrActiveLLMProfileDelete)
	}
	act, err := d.ActiveProfile()
	if err != nil || act == nil || act.APIKey != "secret123" {
		t.Fatalf("active profile/key: %+v err=%v", act, err)
	}
	// list must hide the key, expose hint
	list, _ := d.ListProfiles()
	for _, p := range list {
		if p.ID == pid {
			b, _ := json.Marshal(p)
			if string(b) == "" || contains(string(b), "secret123") {
				t.Fatalf("api key leaked in list json: %s", b)
			}
			if p.APIKeyHint != "…t123" {
				t.Fatalf("hint want …t123, got %q", p.APIKeyHint)
			}
		}
	}

	// agents seeded; prompt versioning
	ag, err := d.GetAgentByKey("planner")
	if err != nil || ag == nil {
		t.Fatalf("planner agent: %v", err)
	}
	v1, err := d.SavePrompt(ag.ID, "你是规划者 {{.Goal}}", "init", "test")
	if err != nil {
		t.Fatal(err)
	}
	v2, _ := d.SavePrompt(ag.ID, "你是规划者 v2 {{.Goal}} {{.Scope}}", "edit", "test")
	if v2 != v1+1 {
		t.Fatalf("version should increment: %d -> %d", v1, v2)
	}
	cur, _ := d.CurrentPrompt(ag.ID)
	if cur != "你是规划者 v2 {{.Goal}} {{.Scope}}" {
		t.Fatalf("current prompt wrong: %q", cur)
	}
	vers, _ := d.ListPromptVersions(ag.ID)
	if len(vers) < 2 {
		t.Fatalf("want >=2 versions, got %d", len(vers))
	}
	pv, _ := d.PromptVars(ag.ID)
	// planner must have at least the seeded catalog vars (Goal, AssetSummary)
	if len(pv) < 2 {
		t.Fatalf("planner catalog want >=2 vars, got %d", len(pv))
	}
	d.Exec(`DELETE FROM agent_prompts WHERE agent_id=$1`, ag.ID)
	d.Exec(`UPDATE agents SET current_prompt_id=NULL WHERE id=$1`, ag.ID)

	// mcp + skill + visibility (bidirectional via one join)
	// Clean up any leftover MCP from prior runs to keep this test idempotent.
	d.Exec(`DELETE FROM mcp_servers WHERE name = 't-gh'`)
	mid, err := d.SaveMCP(&MCPServer{Name: "t-gh", Transport: "stdio", Command: "npx", Args: json.RawMessage(`["server-github"]`), Env: json.RawMessage(`{"GITHUB_TOKEN":"x"}`), Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	// agent-side write. MCP is id-keyed (generic visibility join); skills are now
	// filesystem-based, so their visibility is keyed by skill (directory) name in a
	// dedicated table. Clear any pre-existing MCP visibility rows so the assertions
	// below isolate on exactly what this test sets.
	d.Exec(`DELETE FROM agent_visibility WHERE agent_id = $1 AND resource_kind = 'mcp'`, ag.ID)
	if err := d.ToggleVisibility(ag.ID, "mcp", mid, true); err != nil {
		t.Fatal(err)
	}
	if err := d.SetAgentSkillVisibility(ag.ID, nil); err != nil {
		t.Fatal(err)
	}
	if err := d.ToggleSkillVisibility(ag.ID, "t-skill", true); err != nil {
		t.Fatal(err)
	}
	// agent-side read
	vm, _ := d.AgentVisible(ag.ID, "mcp")
	if len(vm) != 1 || vm[0] != mid {
		t.Fatalf("agent visible mcp: %+v", vm)
	}
	// resource-side read (same join row) → bidirectional
	ra, _ := d.ResourceAgents("mcp", mid)
	if len(ra) != 1 || ra[0] != ag.ID {
		t.Fatalf("resource agents: %+v", ra)
	}
	// toggle off
	d.ToggleVisibility(ag.ID, "mcp", mid, false)
	vm2, _ := d.AgentVisible(ag.ID, "mcp")
	if len(vm2) != 0 {
		t.Fatalf("after toggle off: %+v", vm2)
	}
	// skill visibility is name-keyed: verify the read, then deleting the skill's
	// visibility rows (called when a skill is removed) clears it.
	names, _ := d.AgentSkillNames(ag.ID)
	if len(names) != 1 || names[0] != "t-skill" {
		t.Fatalf("agent visible skills: %+v", names)
	}
	if err := d.DeleteSkillVisibility("t-skill"); err != nil {
		t.Fatal(err)
	}
	names2, _ := d.AgentSkillNames(ag.ID)
	if len(names2) != 0 {
		t.Fatalf("skill visibility should be cleared on delete: %+v", names2)
	}
	d.DeleteMCP(mid)
}

func TestJevProfileCannotBeActivatedOrAgentBound(t *testing.T) {
	d, err := Open(testDSN(t))
	if err != nil {
		t.Skipf("postgres unavailable (%v) — skipping", err)
	}
	defer d.Close()
	id, err := d.SaveProfile(&LLMProfile{Name: "t-jev-only", Format: "jev", Model: "jev-latest", APIKey: "jev-key"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = d.Exec(`DELETE FROM llm_profiles WHERE id=$1`, id) })
	if err := d.SetActiveProfile(id); !errors.Is(err, ErrJevProfileJudgeOnly) {
		t.Fatalf("activate Jev error=%v, want %v", err, ErrJevProfileJudgeOnly)
	}
	if err := d.SetAgentLLMProfile("planner", &id); !errors.Is(err, ErrJevProfileJudgeOnly) {
		t.Fatalf("bind Jev error=%v, want %v", err, ErrJevProfileJudgeOnly)
	}
}

func TestJevProfileCannotBeSavedAsDefault(t *testing.T) {
	d, err := Open(testDSN(t))
	if err != nil {
		t.Skipf("postgres unavailable (%v) — skipping", err)
	}
	defer d.Close()
	if _, err := d.SaveProfile(&LLMProfile{Name: "t-jev-default", Format: "jev", Model: "jev-latest", APIKey: "jev-key", IsDefault: true}); !errors.Is(err, ErrJevProfileJudgeOnly) {
		t.Fatalf("SaveProfile Jev default error=%v, want %v", err, ErrJevProfileJudgeOnly)
	}
}

// The writer must re-read is_default after acquiring its lock, rather than use
// the value observed before a concurrent activation committed.
func TestJevProfileConversionWaitsForActivation(t *testing.T) {
	dsn := testDSN(t)
	d, err := Open(dsn)
	if err != nil {
		t.Skipf("postgres unavailable (%v) — skipping", err)
	}
	t.Cleanup(func() { _ = d.Close() })
	previous, err := d.ActiveProfile()
	if err != nil {
		t.Fatal(err)
	}
	profile := &LLMProfile{Name: fmt.Sprintf("jev-convert-active-%d", time.Now().UnixNano()), Format: "openai", Model: "fixture", APIKey: "fixture-key"}
	profile.ID, err = d.SaveProfile(profile)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = d.Exec(`UPDATE llm_profiles SET is_default=false WHERE id=$1`, profile.ID)
		if previous != nil {
			_ = d.SetActiveProfile(previous.ID)
		}
		_, _ = d.Exec(`DELETE FROM llm_profiles WHERE id=$1`, profile.ID)
	})
	blocker, err := d.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer blocker.Rollback()
	if _, err := blocker.Exec(`SELECT id FROM llm_profiles WHERE id=$1 FOR UPDATE`, profile.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := blocker.Exec(`UPDATE llm_profiles SET is_default=false WHERE is_default`); err != nil {
		t.Fatal(err)
	}
	if _, err := blocker.Exec(`UPDATE llm_profiles SET is_default=true WHERE id=$1`, profile.ID); err != nil {
		t.Fatal(err)
	}
	writer, pid := openSingleConnectionTestDB(t, dsn)
	done := make(chan error, 1)
	profile.Format = "jev"
	go func() { _, err := writer.SaveProfile(profile); done <- err }()
	if err := waitForBackendBlock(d, pid, done); err != nil {
		t.Fatal(err)
	}
	if err := blocker.Commit(); err != nil {
		t.Fatal(err)
	}
	if err := waitForMutationResult(done); !errors.Is(err, ErrJevProfileJudgeOnly) {
		t.Fatalf("format conversion error=%v, want %v", err, ErrJevProfileJudgeOnly)
	}
	got, err := d.ProfileByID(profile.ID)
	if err != nil || got == nil || !got.IsDefault || got.Format != "openai" {
		t.Fatalf("concurrent activation was changed by rejected conversion: %+v err=%v", got, err)
	}
}

// New bindings use a key-share profile lock. Conversion must exclude that lock,
// wait for its commit, and only then check live references without locking any
// child row in the opposite order.
func TestJevProfileConversionWaitsForReferenceCommit(t *testing.T) {
	dsn := testDSN(t)
	d, err := Open(dsn)
	if err != nil {
		t.Skipf("postgres unavailable (%v) — skipping", err)
	}
	t.Cleanup(func() { _ = d.Close() })
	suffix := time.Now().UnixNano()
	profile := &LLMProfile{Name: fmt.Sprintf("jev-convert-bound-%d", suffix), Format: "openai", Model: "fixture", APIKey: "fixture-key"}
	profile.ID, err = d.SaveProfile(profile)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = d.Exec(`DELETE FROM llm_profiles WHERE id=$1`, profile.ID) })
	agent, err := d.CreateAgent(fmt.Sprintf("jev_bound_%d", suffix), "fixture", "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.DeleteAgent(agent.Key) })
	blocker, err := d.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer blocker.Rollback()
	if _, err := blocker.Exec(`SELECT id FROM agents WHERE id=$1 FOR UPDATE`, agent.ID); err != nil {
		t.Fatal(err)
	}
	if err := lockLLMProfileForReference(blocker, &profile.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := blocker.Exec(`UPDATE agents SET llm_profile_id=$1 WHERE id=$2`, profile.ID, agent.ID); err != nil {
		t.Fatal(err)
	}
	writer, pid := openSingleConnectionTestDB(t, dsn)
	done := make(chan error, 1)
	profile.Format = "jev"
	go func() { _, err := writer.SaveProfile(profile); done <- err }()
	if err := waitForBackendBlock(d, pid, done); err != nil {
		t.Fatal(err)
	}
	if err := blocker.Commit(); err != nil {
		t.Fatal(err)
	}
	if err := waitForMutationResult(done); !errors.Is(err, ErrJevProfileJudgeOnly) {
		t.Fatalf("bound format conversion error=%v, want %v", err, ErrJevProfileJudgeOnly)
	}
	got, err := d.ProfileByID(profile.ID)
	if err != nil || got == nil || got.Format != "openai" {
		t.Fatalf("bound profile changed on rejection: %+v err=%v", got, err)
	}
}

func TestJevProfileReferenceChecksCommittedFormat(t *testing.T) {
	dsn := testDSN(t)
	d, err := Open(dsn)
	if err != nil {
		t.Skipf("postgres unavailable (%v) — skipping", err)
	}
	t.Cleanup(func() { _ = d.Close() })
	profile := &LLMProfile{Name: fmt.Sprintf("jev-convert-admit-%d", time.Now().UnixNano()), Format: "openai", Model: "fixture", APIKey: "fixture-key"}
	profile.ID, err = d.SaveProfile(profile)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = d.Exec(`DELETE FROM llm_profiles WHERE id=$1`, profile.ID) })
	blocker, err := d.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer blocker.Rollback()
	if _, err := blocker.Exec(`SELECT id FROM llm_profiles WHERE id=$1 FOR UPDATE`, profile.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := blocker.Exec(`UPDATE llm_profiles SET format='jev' WHERE id=$1`, profile.ID); err != nil {
		t.Fatal(err)
	}
	writer, pid := openSingleConnectionTestDB(t, dsn)
	done := make(chan error, 1)
	go func() {
		_, err := writer.CreateConversation("auto", "must not bind Jev", &profile.ID)
		done <- err
	}()
	if err := waitForBackendBlock(d, pid, done); err != nil {
		t.Fatal(err)
	}
	if err := blocker.Commit(); err != nil {
		t.Fatal(err)
	}
	if err := waitForMutationResult(done); !errors.Is(err, ErrJevProfileJudgeOnly) {
		t.Fatalf("conversation admission error=%v, want %v", err, ErrJevProfileJudgeOnly)
	}
	var count int
	if err := d.QueryRow(`SELECT count(*) FROM conversations WHERE llm_profile_id=$1`, profile.ID).Scan(&count); err != nil || count != 0 {
		t.Fatalf("rejected conversation persisted a binding: count=%d err=%v", count, err)
	}
}

func TestJevProfileUnboundConversionPreservesInstructions(t *testing.T) {
	d, err := Open(testDSN(t))
	if err != nil {
		t.Skipf("postgres unavailable (%v) — skipping", err)
	}
	t.Cleanup(func() { _ = d.Close() })
	profile := &LLMProfile{
		Name: fmt.Sprintf("jev-convert-free-%d", time.Now().UnixNano()), Format: "openai-responses", Model: "fixture", APIKey: "fixture-key",
		ModelInstructionsFile: "fixture.md", ModelInstructionsFiles: []string{"fixture.md", "second.md"}, CodexConfigFile: "fixture.toml",
	}
	profile.ID, err = d.SaveProfile(profile)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = d.Exec(`DELETE FROM llm_profiles WHERE id=$1`, profile.ID) })
	profile.Format = "jev"
	profile.APIKey = "" // Empty on update must retain the stored key.
	if _, err := d.SaveProfile(profile); err != nil {
		t.Fatal(err)
	}
	got, err := d.ProfileByID(profile.ID)
	if err != nil || got == nil || got.Format != "jev" || got.APIKey != "fixture-key" || got.ModelInstructionsFile != "fixture.md" || got.CodexConfigFile != "fixture.toml" || len(got.ModelInstructionsFiles) != 2 || got.ModelInstructionsFiles[1] != "second.md" {
		t.Fatalf("unbound conversion lost profile fields: %+v err=%v", got, err)
	}
}

func TestActiveLLMProfileLockNamespace(t *testing.T) {
	for _, reserved := range []int64{schemaMigrationLockKey, 7337741002, companyScopeMutationLock, findingEvidenceLockKey, 7337741006} {
		if activeLLMProfileLockKey == reserved {
			t.Fatal("active profile lock collides with an infrastructure lock")
		}
	}
}

func TestActiveProfileActivationSerializesBeforeRowLocks(t *testing.T) {
	dsn := testDSN(t)
	d, err := Open(dsn)
	if err != nil {
		t.Skipf("postgres unavailable (%v) — skipping", err)
	}
	t.Cleanup(func() { _ = d.Close() })
	previous, err := d.ActiveProfile()
	if err != nil {
		t.Fatal(err)
	}
	var ids []int64
	t.Cleanup(func() {
		for _, id := range ids {
			_, _ = d.Exec(`UPDATE llm_profiles SET is_default=false WHERE id=$1`, id)
		}
		if previous != nil {
			_ = d.SetActiveProfile(previous.ID)
		}
		for _, id := range ids {
			_, _ = d.Exec(`DELETE FROM llm_profiles WHERE id=$1`, id)
		}
	})
	for i := 0; i < 2; i++ {
		id, err := d.SaveProfile(&LLMProfile{Name: fmt.Sprintf("activate-serial-%d-%d", time.Now().UnixNano(), i), Format: "openai", Model: "fixture"})
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}
	blocker, err := d.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer blocker.Rollback()
	if _, err := blocker.Exec(`SELECT pg_advisory_xact_lock($1)`, activeLLMProfileLockKey); err != nil {
		t.Fatal(err)
	}
	dones := make([]chan error, 0, 2)
	for _, id := range ids {
		writer, pid := openSingleConnectionTestDB(t, dsn)
		done := make(chan error, 1)
		dones = append(dones, done)
		go func() { done <- writer.SetActiveProfile(id) }()
		if err := waitForBackendBlock(d, pid, done); err != nil {
			t.Fatal(err)
		}
	}
	// Waiting on the global switch gate must not already own either target.
	assertProfilesUnlocked(t, d, ids...)
	if err := blocker.Rollback(); err != nil {
		t.Fatal(err)
	}
	for _, done := range dones {
		if err := waitForMutationResult(done); err != nil {
			t.Fatalf("concurrent activation failed: %v", err)
		}
	}
	var count int
	if err := d.QueryRow(`SELECT count(*) FROM llm_profiles WHERE is_default`).Scan(&count); err != nil || count != 1 {
		t.Fatalf("single-default invariant: count=%d err=%v", count, err)
	}
	got, err := d.ActiveProfile()
	if err != nil || got == nil || (got.ID != ids[0] && got.ID != ids[1]) {
		t.Fatalf("concurrent activation selected an unexpected profile: %+v err=%v", got, err)
	}
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && (indexOf(s, sub) >= 0)
}
func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
