package server

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/Autumn-27/artex/agent"
	"github.com/Autumn-27/artex/db"
	"github.com/Autumn-27/norma/skill"
	actool "github.com/Autumn-27/norma/tool"
)

type recordingSkillUsage struct {
	rows []*db.SkillUsage
	err  error
}

func (r *recordingSkillUsage) InsertSkillUsage(row *db.SkillUsage) error {
	copy := *row
	r.rows = append(r.rows, &copy)
	return r.err
}

func TestNormalizeSkillInvocationOnlyChangesNameValues(t *testing.T) {
	cases := []struct{ input, want string }{
		{` { "name" : " cve-search \t", "args" : " untouched \t", "extra": {"name":" nested "} } `, ` { "name" : "cve-search", "args" : " untouched \t", "extra": {"name":" nested "} } `},
		{`{"Name":" first ","name":" second ","unknown":[1,2]}`, `{"Name":"first","name":"second","unknown":[1,2]}`},
		{`{"\u006eame":" cve-search ","args":"a"}`, `{"\u006eame":"cve-search","args":"a"}`},
		{`{"name":"cve-search","args":"unchanged"}`, `{"name":"cve-search","args":"unchanged"}`},
		{`{"name":123}`, `{"name":123}`},
		{`{"name":null}`, `{"name":null}`},
		{`null`, `null`},
		{`[]`, `[]`},
		{`{"name":" cve-search "} {}`, `{"name":" cve-search "} {}`},
		{`{"name":" cve-search "`, `{"name":" cve-search "`},
	}
	for _, tc := range cases {
		if got := string(normalizeSkillInvocation(json.RawMessage(tc.input))); got != tc.want {
			t.Fatalf("normalize changed unrelated input: got %q want %q", got, tc.want)
		}
	}
}

func TestMeterSkillUsesTheSameNormalizedInputAsTheRealTool(t *testing.T) {
	registry := skill.NewRegistry()
	registry.Add(skill.Skill{Name: "display-skill", Dir: filepath.Join(t.TempDir(), "directory-skill"), Instructions: "fixture"})
	var received json.RawMessage
	calls := 0
	base := actool.Build(actool.Spec{Name: "Skill", Run: func(_ context.Context, input json.RawMessage, _ *actool.ToolContext) (actool.Result, error) {
		calls++
		received = append(json.RawMessage(nil), input...)
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(input, &fields); err != nil {
			t.Fatal(err)
		}
		var name string
		if err := json.Unmarshal(fields["name"], &name); err != nil {
			t.Fatal(err)
		}
		if _, found := registry.Get(name); !found {
			return actool.Errorf("unknown fixture skill"), nil
		}
		return actool.Text("fixture delegated"), nil
	}})
	recorder := &recordingSkillUsage{}
	ri := agent.RunInfo{TaskID: 42, ExplorationID: 7, IntentID: 8, SessionID: "conv-fixture"}
	wrapped := &meteredSkill{CoreTool: base, recorder: recorder, reg: registry, agentKey: "worker", ri: ri}
	const input = `{"name":" display-skill ","args":" context unchanged ","unknown":{"keep":true}}`
	result, err := wrapped.Call(t.Context(), json.RawMessage(input), nil)
	if err != nil || result.IsError || calls != 1 || len(recorder.rows) != 1 {
		t.Fatal("normalized skill did not reach the real tool exactly once")
	}
	if string(received) != `{"name":"display-skill","args":" context unchanged ","unknown":{"keep":true}}` {
		t.Fatal("delegation changed args or unknown extension fields")
	}
	want := &db.SkillUsage{Skill: "directory-skill", AgentKey: "worker", TaskID: 42, ExplorationID: 7, IntentID: 8, SessionID: "conv-fixture", ArgsLen: len(" context unchanged "), Found: true}
	if !reflect.DeepEqual(recorder.rows[0], want) {
		t.Fatalf("meter lookup/attribution disagreed with delegation: got %+v want %+v", recorder.rows[0], want)
	}
	result, err = wrapped.Call(t.Context(), json.RawMessage(`{"name":" missing-skill ","args":""}`), nil)
	if err != nil || !result.IsError || calls != 2 || len(recorder.rows) != 2 || recorder.rows[1].Found || recorder.rows[1].Skill != "missing-skill" {
		t.Fatal("unknown skill should remain a metered registry miss")
	}
}

func TestMeterSkillFailureIsBestEffortAndMatchedIsNotSuccessful(t *testing.T) {
	registry := skill.NewRegistry()
	registry.Add(skill.Skill{Name: "fixture", Dir: filepath.Join(t.TempDir(), "fixture")})
	recorder := &recordingSkillUsage{err: errors.New("fixture usage unavailable")}
	calls := 0
	base := actool.Build(actool.Spec{Name: "Skill", Run: func(context.Context, json.RawMessage, *actool.ToolContext) (actool.Result, error) {
		calls++
		return actool.Errorf("fixture processing failed"), nil
	}})
	wrapped := &meteredSkill{CoreTool: base, recorder: recorder, reg: registry}
	result, err := wrapped.Call(t.Context(), json.RawMessage(`{"name":" fixture "}`), nil)
	if err != nil || !result.IsError || calls != 1 || len(recorder.rows) != 1 || !recorder.rows[0].Found {
		t.Fatal("metering failure changed execution or registry match was treated as execution success")
	}
	serialized, err := json.Marshal(recorder.rows)
	if err != nil || strings.Contains(string(serialized), "fixture processing failed") {
		t.Fatal("usage ledger must contain attribution only, not tool results")
	}
}

func TestMeterSkillPreservesKnownDirectoryNamesAndBoundsUnknownUTF8(t *testing.T) {
	registry := skill.NewRegistry()
	directory := strings.Repeat("界", 50) // A valid directory may exceed the miss-only byte cap.
	registry.Add(skill.Skill{Name: "fixture-display-name", Dir: filepath.Join(t.TempDir(), directory)})
	recorder := &recordingSkillUsage{}
	base := actool.Build(actool.Spec{Name: "Skill", Run: func(context.Context, json.RawMessage, *actool.ToolContext) (actool.Result, error) {
		return actool.Text("fixture"), nil
	}})
	wrapped := &meteredSkill{CoreTool: base, recorder: recorder, reg: registry}
	if _, err := wrapped.Call(t.Context(), json.RawMessage(`{"name":"fixture-display-name"}`), nil); err != nil {
		t.Fatal(err)
	}
	if len(recorder.rows) != 1 || recorder.rows[0].Skill != directory || !recorder.rows[0].Found {
		t.Fatal("known directory identity was truncated and no longer matches the skills page")
	}
	unknown, _ := json.Marshal(map[string]string{"name": strings.Repeat("界", 100)})
	if _, err := wrapped.Call(t.Context(), unknown, nil); err != nil {
		t.Fatal(err)
	}
	if len(recorder.rows) != 2 || recorder.rows[1].Found || len(recorder.rows[1].Skill) > maxLedgerSkillName || !utf8.ValidString(recorder.rows[1].Skill) {
		t.Fatal("unknown skill names must stay bounded without producing invalid PostgreSQL UTF-8")
	}
}
