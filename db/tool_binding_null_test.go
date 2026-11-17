package db

import (
	"encoding/json"
	"fmt"
	"reflect"
	"testing"
	"time"
)

func TestOperatorCustomToolEmptyBindingsStoredAsArrays(t *testing.T) {
	d, err := Open(testDSN(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.Close() })
	key := fmt.Sprintf("binding-write-%d", time.Now().UnixNano())
	tool := &Tool{Key: key, Kind: "script", Description: "synthetic binding fixture", Enabled: true}
	if err := d.CreateCustomTool(tool); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.DeleteCustomTool(key) })
	check := func(want []string) {
		t.Helper()
		var kind string
		var raw []byte
		if err := d.QueryRow(`SELECT jsonb_typeof(agents),agents FROM tools WHERE key=$1`, key).Scan(&kind, &raw); err != nil {
			t.Fatal(err)
		}
		var got []string
		if err := json.Unmarshal(raw, &got); err != nil || kind != "array" || !reflect.DeepEqual(got, want) {
			t.Fatalf("stored kind=%q bindings=%v, want array %v: %v", kind, got, want, err)
		}
		if _, _, _, err := d.AgentBindingCounts(); err != nil {
			t.Fatal("saved custom tool broke binding statistics", err)
		}
	}
	check([]string{})
	for _, binding := range [][]string{{"worker", "mainagent"}, nil, {}} {
		tool.Agents = binding
		if err := d.UpdateCustomTool(tool); err != nil {
			t.Fatal(err)
		}
		want := binding
		if want == nil {
			want = []string{}
		}
		check(want)
	}
}
