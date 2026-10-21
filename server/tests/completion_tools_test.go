//go:build ignore

package server

import (
	"encoding/json"
	"testing"

	"github.com/Autumn-27/artex/db"
)

func TestCompletionToolUpgradePreservesUserConfiguration(t *testing.T) {
	dsn, _, err := db.DSN()
	if err != nil {
		t.Skip("postgres unavailable")
	}
	pg, err := db.Open(dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = pg.Close() })
	s := &Server{m: &Manager{pg: pg}}
	for _, key := range []string{"prove_goal", "goal_met"} {
		original, err := pg.GetTool(key)
		if err != nil {
			t.Fatal(err)
		}
		if original == nil {
			if err = pg.SeedTool(key, "custom description", json.RawMessage(`{"type":"object","properties":{"reason":{"type":"string","default":"keep me"}},"required":["reason"]}`), json.RawMessage(`["custom-agent"]`)); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _, _ = pg.Exec(`DELETE FROM tools WHERE key=$1`, key) })
		} else {
			agents, _ := json.Marshal(original.Agents)
			t.Cleanup(func() {
				_, _ = pg.Exec(`UPDATE tools SET description=$2,schema=$3,agents=$4,enabled=$5 WHERE key=$1`, key, original.Description, original.Schema, agents, original.Enabled)
			})
		}
		_, err = pg.Exec(`UPDATE tools SET description='custom description',schema=$2,agents='["custom-agent"]',enabled=false WHERE key=$1`, key, `{"type":"object","properties":{"reason":{"type":"string","default":"keep me"}},"required":["reason"]}`)
		if err != nil {
			t.Fatal(err)
		}
	}
	s.seedCompletionTools()
	s.seedCompletionTools()
	for _, key := range []string{"prove_goal", "goal_met"} {
		row, err := pg.GetTool(key)
		if err != nil {
			t.Fatal(err)
		}
		if row.Description != "custom description" || row.Enabled || len(row.Agents) != 1 || row.Agents[0] != "custom-agent" {
			t.Fatalf("upgrade reset configuration: %+v", row)
		}
		var schema map[string]any
		if err = json.Unmarshal(row.Schema, &schema); err != nil {
			t.Fatal(err)
		}
		props := schema["properties"].(map[string]any)
		if props["remaining_work"] == nil || props["reason"].(map[string]any)["default"] != "keep me" {
			t.Fatalf("schema lost property or default: %s", row.Schema)
		}
		required, ok := schema["required"].([]any)
		if !ok || len(required) != 1 || required[0] != "reason" {
			t.Fatalf("required changed: %s", row.Schema)
		}
		if _, err = pg.Exec(`UPDATE tools SET schema=jsonb_set(schema,'{properties,remaining_work,description}','"custom remaining description"'::jsonb) WHERE key=$1`, key); err != nil {
			t.Fatal(err)
		}
	}
	s.seedCompletionTools()
	for _, key := range []string{"prove_goal", "goal_met"} {
		var description string
		if err := pg.QueryRow(`SELECT schema#>>'{properties,remaining_work,description}' FROM tools WHERE key=$1`, key).Scan(&description); err != nil || description != "custom remaining description" {
			t.Fatalf("existing optional schema overwritten: %s %v", description, err)
		}
	}
}
