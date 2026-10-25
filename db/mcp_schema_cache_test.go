package db

import (
	"encoding/json"
	"testing"
)

func TestMCPToolSchemasPersistAndInvalidateOnConfigChange(t *testing.T) {
	d, err := Open(testDSN(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.Close() })
	m := &MCPServer{Name: "t-mcp-schema-cache", Transport: "stdio", Command: "fixture", Args: json.RawMessage(`[]`), Env: json.RawMessage(`{}`), Enabled: true}
	_, _ = d.Exec(`DELETE FROM mcp_servers WHERE name=$1`, m.Name)
	m.ID, err = d.SaveMCP(m)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.DeleteMCP(m.ID) })
	schema := json.RawMessage(`{"type":"object","properties":{"query":{"type":"string"}},"required":["query"]}`)
	if err = d.SaveMCPTools(m.ID, []MCPTool{{Name: "mcp__fixture__search", Description: "search fixture", Schema: schema}}); err != nil {
		t.Fatal(err)
	}
	check := func(want int) {
		t.Helper()
		tools, e := d.MCPToolsDetailed(m.ID)
		if e != nil {
			t.Fatal(e)
		}
		if len(tools) != want {
			t.Fatalf("cached tools=%d, want %d", len(tools), want)
		}
		if want > 0 {
			var got map[string]any
			if json.Unmarshal(tools[0].Schema, &got) != nil || got["type"] != "object" {
				t.Fatal("schema lost")
			}
		}
	}
	check(1)
	m.Enabled = false
	if _, err = d.SaveMCP(m); err != nil {
		t.Fatal(err)
	}
	check(1)
	m.Enabled = true
	m.Args = json.RawMessage(`["--different-profile"]`)
	if _, err = d.SaveMCP(m); err != nil {
		t.Fatal(err)
	}
	check(0)
	// Old callers may still cache only names. SQL NULL distinguishes that from
	// a real empty object schema, triggering a one-time metadata hydration.
	if err = d.SaveMCPTools(m.ID, []MCPTool{{Name: "mcp__fixture__legacy"}}); err != nil {
		t.Fatal(err)
	}
	tools, err := d.MCPToolsDetailed(m.ID)
	if err != nil || len(tools) != 1 || len(tools[0].Schema) != 0 {
		t.Fatalf("legacy cache: %+v %v", tools, err)
	}
}
