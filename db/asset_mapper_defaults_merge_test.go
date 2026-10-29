package db

import (
	"encoding/json"
	"reflect"
	"slices"
	"testing"
)

func mapperDefaultReadersFixture() []Tool {
	return []Tool{
		{Key: "list_assets", Description: "public asset reader fixture", Schema: json.RawMessage(`{"type":"object","properties":{"page":{"type":"integer"}}}`), Agents: []string{"mainagent", "worker", "asset_mapper"}},
		{Key: "list_companies", Description: "public company reader fixture", Schema: json.RawMessage(`{"type":"object","properties":{"query":{"type":"string"}}}`), Agents: []string{"mainagent", "asset_mapper"}},
	}
}

func mapperDefaultsFixtureDB(t *testing.T) (*DB, string) {
	t.Helper()
	dsn := upstreamMergeFixtureDSN(t)
	d, err := Open(dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	return d, dsn
}

func TestAssetMapperDefaultsFirstCreationPreservesExistingToolSettings(t *testing.T) {
	d, dsn := mapperDefaultsFixtureDB(t)
	defaults := mapperDefaultReadersFixture()
	original := map[string]*Tool{}
	for _, reader := range defaults {
		if err := d.SeedTool(reader.Key, "edited description", json.RawMessage(`{"type":"object","properties":{"custom":{"type":"string"}}}`), json.RawMessage(`["worker","pentest"]`)); err != nil {
			t.Fatal(err)
		}
		if _, err := d.Exec(`UPDATE tools SET enabled=false WHERE key=$1`, reader.Key); err != nil {
			t.Fatal(err)
		}
		row, err := d.GetTool(reader.Key)
		if err != nil {
			t.Fatal(err)
		}
		original[reader.Key] = row
	}
	created, err := d.SeedAssetMapperIfMissing("public mapper prompt fixture", defaults)
	if err != nil || !created {
		t.Fatalf("first mapper creation=%v err=%v", created, err)
	}
	a, err := d.GetAgentByKey("asset_mapper")
	if err != nil || a == nil || !a.Builtin || !a.Enabled || a.InteractiveShell || a.WebSearch {
		t.Fatal("new mapper role is missing or has execution switches", err)
	}
	if prompt, err := d.CurrentPrompt(a.ID); err != nil || prompt != "public mapper prompt fixture" {
		t.Fatalf("first mapper prompt=%q err=%v", prompt, err)
	}
	var bindings int
	if err := d.QueryRow(`SELECT count(*) FROM tools WHERE agents ? 'asset_mapper'`).Scan(&bindings); err != nil || bindings != 2 {
		t.Fatalf("new mapper has %d tool bindings instead of two: %v", bindings, err)
	}
	for key, before := range original {
		after, err := d.GetTool(key)
		if err != nil || after == nil || after.Enabled != before.Enabled || after.Description != before.Description || !reflect.DeepEqual(after.Schema, before.Schema) || !reflect.DeepEqual(after.Agents, []string{"worker", "pentest", "asset_mapper"}) {
			t.Fatalf("existing read tool customization was overwritten: key=%s row=%+v err=%v", key, after, err)
		}
	}
	var marker string
	if err := d.QueryRow(`SELECT value FROM settings WHERE key=$1`, assetMapperDefaultsKey(a.ID)).Scan(&marker); err != nil || marker != "true" {
		t.Fatal("mapper initialization marker not committed", err)
	}
	// A later explicit empty selection, disabled role and custom prompt are not
	// repaired or reset by either DB startup or catalog initialization.
	if _, err := d.Exec(`UPDATE tools SET agents=agents-'asset_mapper'; UPDATE agents SET enabled=false WHERE key='asset_mapper'`); err != nil {
		t.Fatal(err)
	}
	if _, err := d.SavePrompt(a.ID, "human customized fixture", "user fixture", "human"); err != nil {
		t.Fatal(err)
	}
	if created, err := d.SeedAssetMapperIfMissing("replacement default", defaults); err != nil || created {
		t.Fatalf("existing mapper was recreated: %v %v", created, err)
	}
	d.Close()
	reopened, err := Open(dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if created, err := reopened.SeedAssetMapperIfMissing("replacement default", defaults); err != nil || created {
		t.Fatalf("restart recreated mapper: %v %v", created, err)
	}
	afterRole, err := reopened.GetAgentByKey("asset_mapper")
	if err != nil || afterRole == nil || afterRole.Enabled {
		t.Fatal("restart re-enabled the mapper", err)
	}
	if prompt, err := reopened.CurrentPrompt(a.ID); err != nil || prompt != "human customized fixture" {
		t.Fatal("restart replaced the custom mapper prompt", err)
	}
	if err := reopened.QueryRow(`SELECT count(*) FROM tools WHERE agents ? 'asset_mapper'`).Scan(&bindings); err != nil || bindings != 0 {
		t.Fatalf("restart restored an explicit empty mapper selection: %d %v", bindings, err)
	}
}

func TestAssetMapperDefaultsExistingCustomRoleIsUntouched(t *testing.T) {
	d, _ := mapperDefaultsFixtureDB(t)
	a, err := d.CreateAgent("asset_mapper", "user-owned mapper", "custom role fixture")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.Exec(`UPDATE agents SET enabled=false WHERE id=$1`, a.ID); err != nil {
		t.Fatal(err)
	}
	if created, err := d.SeedAssetMapperIfMissing("new default", mapperDefaultReadersFixture()); err != nil || created {
		t.Fatalf("existing custom mapper was seeded: %v %v", created, err)
	}
	got, err := d.GetAgentByKey("asset_mapper")
	if err != nil || got == nil || got.Builtin || got.Enabled || got.Name != "user-owned mapper" {
		t.Fatal("existing custom role identity was changed", err)
	}
	if prompt, err := d.CurrentPrompt(a.ID); err != nil || prompt != "" {
		t.Fatal("an intentionally empty existing prompt was filled", err)
	}
	var bindings, markers int
	if err := d.QueryRow(`SELECT count(*) FROM tools WHERE agents ? 'asset_mapper'`).Scan(&bindings); err != nil {
		t.Fatal(err)
	}
	if err := d.QueryRow(`SELECT count(*) FROM settings WHERE key=$1`, assetMapperDefaultsKey(a.ID)).Scan(&markers); err != nil {
		t.Fatal(err)
	}
	if bindings != 0 || markers != 0 {
		t.Fatalf("existing custom role gained defaults: bindings=%d marker=%d", bindings, markers)
	}
}

func TestAssetMapperDefaultsFailureRollsBackRolePromptAndBindings(t *testing.T) {
	d, _ := mapperDefaultsFixtureDB(t)
	for _, reader := range mapperDefaultReadersFixture() {
		if err := d.SeedTool(reader.Key, "existing reader", reader.Schema, json.RawMessage(`["worker"]`)); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := d.Exec(`CREATE FUNCTION mapper_seed_failure_fixture() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
 IF NEW.key='list_companies' AND NEW.agents ? 'asset_mapper' THEN RAISE EXCEPTION 'mapper fixture rejects second binding'; END IF;
 RETURN NEW;
END $$;
CREATE TRIGGER mapper_seed_failure_fixture BEFORE INSERT OR UPDATE ON tools FOR EACH ROW EXECUTE FUNCTION mapper_seed_failure_fixture()`); err != nil {
		t.Fatal(err)
	}
	if created, err := d.SeedAssetMapperIfMissing("fixture prompt", mapperDefaultReadersFixture()); err == nil || created {
		t.Fatalf("injected second-read failure was not atomic: created=%v err=%v", created, err)
	}
	var roles, prompts, markers int
	if err := d.QueryRow(`SELECT count(*) FROM agents WHERE key='asset_mapper'`).Scan(&roles); err != nil {
		t.Fatal(err)
	}
	if err := d.QueryRow(`SELECT count(*) FROM agent_prompts WHERE template_text='fixture prompt'`).Scan(&prompts); err != nil {
		t.Fatal(err)
	}
	if err := d.QueryRow(`SELECT count(*) FROM settings WHERE key LIKE 'asset_mapper_defaults_initialized_v1:%'`).Scan(&markers); err != nil {
		t.Fatal(err)
	}
	if roles != 0 || prompts != 0 || markers != 0 {
		t.Fatalf("failed seed left partial role state: roles=%d prompts=%d markers=%d", roles, prompts, markers)
	}
	for _, key := range []string{"list_assets", "list_companies"} {
		row, err := d.GetTool(key)
		if err != nil || row == nil || slices.Contains(row.Agents, "asset_mapper") || !reflect.DeepEqual(row.Agents, []string{"worker"}) {
			t.Fatalf("failed seed changed %s bindings: %+v %v", key, row, err)
		}
	}
}
