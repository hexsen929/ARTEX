package server

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log"
	"path/filepath"
	"strings"
	"unicode/utf8"

	"github.com/Autumn-27/norma/skill"
	actool "github.com/Autumn-27/norma/tool"

	"github.com/Autumn-27/artex/agent"
	"github.com/Autumn-27/artex/db"
)

// maxLedgerSkillName caps the skill name stored for a MISS — that string comes
// straight from the model and is otherwise unbounded.
const maxLedgerSkillName = 128

// meteredSkill wraps the Skill meta-tool to append one skill_usage row per call.
// The metering lives here rather than in Registry.OnInvoke because OnInvoke only
// receives the resolved Skill: it never fires for an unknown skill name (norma
// returns a tool error before the hook) and it cannot see the caller's args. Both
// matter — a miss tells you which procedure the agents wished existed.
type meteredSkill struct {
	actool.CoreTool
	recorder skillUsageRecorder
	reg      *skill.Registry
	agentKey string
	ri       agent.RunInfo
}

type skillUsageRecorder interface {
	InsertSkillUsage(*db.SkillUsage) error
}

// meterSkillTool returns t unchanged when there is no DB to meter into.
func meterSkillTool(t actool.CoreTool, pg *db.DB, reg *skill.Registry, agentKey string, ri agent.RunInfo) actool.CoreTool {
	if pg == nil {
		return t
	}
	return &meteredSkill{CoreTool: t, recorder: pg, reg: reg, agentKey: agentKey, ri: ri}
}

func (m *meteredSkill) Call(ctx context.Context, input json.RawMessage, tc *actool.ToolContext) (actool.Result, error) {
	// The lookup recorded below and the SDK's real lookup must receive the same
	// name. Otherwise whitespace can be counted as found but rejected by the SDK.
	input = normalizeSkillInvocation(input)
	m.record(input)
	return m.CoreTool.Call(ctx, input, tc)
}

// normalizeSkillInvocation trims only top-level name string values. All other
// bytes (args, unknown fields, nested objects, and duplicate-key order) stay
// untouched. Invalid JSON is left to the real tool's ordinary validation.
func normalizeSkillInvocation(input json.RawMessage) json.RawMessage {
	decoder := json.NewDecoder(bytes.NewReader(input))
	opening, err := decoder.Token()
	if err != nil || opening != json.Delim('{') {
		return input
	}
	type replacement struct {
		start, end int
		value      []byte
	}
	var replacements []replacement
	for decoder.More() {
		key, err := decoder.Token()
		if err != nil {
			return input
		}
		var raw json.RawMessage
		if err := decoder.Decode(&raw); err != nil {
			return input
		}
		name, isName := key.(string)
		if !isName || !strings.EqualFold(name, "name") {
			continue
		}
		var value string
		if json.Unmarshal(raw, &value) != nil || value == strings.TrimSpace(value) {
			continue
		}
		normalized, err := json.Marshal(strings.TrimSpace(value))
		if err != nil {
			return input
		}
		end := int(decoder.InputOffset())
		replacements = append(replacements, replacement{start: end - len(raw), end: end, value: normalized})
	}
	if closing, err := decoder.Token(); err != nil || closing != json.Delim('}') {
		return input
	}
	if err := decoder.Decode(new(any)); err != io.EOF || len(replacements) == 0 {
		return input
	}
	var output bytes.Buffer
	previous := 0
	for _, change := range replacements {
		output.Write(input[previous:change.start])
		output.Write(change.value)
		previous = change.end
	}
	output.Write(input[previous:])
	return output.Bytes()
}

// record appends the ledger row. Best-effort by design: a metering failure must
// never turn into a failed skill invocation, so every error path just logs.
func (m *meteredSkill) record(input json.RawMessage) {
	var in struct {
		Name string `json:"name"`
		Args string `json:"args"`
	}
	if err := json.Unmarshal(input, &in); err != nil {
		return // malformed call; norma will reject it too
	}
	name := strings.TrimSpace(in.Name)
	if name == "" {
		return
	}
	// Resolve to the directory name so the ledger keys match agent_skill_visibility
	// and the skills page (a skill's display Name can differ from its directory).
	found := false
	if m.reg != nil {
		if s, ok := m.reg.Get(name); ok {
			found = true
			if s.Dir != "" {
				name = filepath.Base(s.Dir)
			}
		}
	}
	if !found && len(name) > maxLedgerSkillName {
		name = name[:maxLedgerSkillName]
		for !utf8.ValidString(name) {
			name = name[:len(name)-1]
		}
	}
	err := m.recorder.InsertSkillUsage(&db.SkillUsage{
		Skill:         name,
		AgentKey:      m.agentKey,
		TaskID:        m.ri.TaskID,
		ExplorationID: m.ri.ExplorationID,
		IntentID:      m.ri.IntentID,
		SessionID:     m.ri.SessionID,
		ArgsLen:       len(in.Args),
		Found:         found,
	})
	if err != nil {
		log.Printf("[skillusage] insert: %v", err)
	}
}
