package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"runtime/debug"

	"github.com/Autumn-27/artex/db"
	actool "github.com/Autumn-27/norma/tool"
)

// DeferredInfo carries the deferred-tools wiring an agent needs to build its
// Options: the MCP tool names whose schemas are withheld, the subset listed in the
// global system-prompt block (non-skill-gated), and the shared session unlock set.
// UnlockSkill unlocks a named skill's MCPs — hosts call it to rebuild the unlock set
// from history on a resumed session (design doc C2).
type DeferredInfo struct {
	AssemblyError        error             // explicit per-run authorization metadata failures; caller must stop before model execution
	AssemblyWarnings     []string          // authorized optional services omitted from this run; never raw transport errors
	SkillDirs            map[string]string // authorized and loadable directory name -> absolute Skill directory
	SkillReadGuidance    string            // limited goals knowledge reader, never generic filesystem access
	IntelligenceGuidance string
	CaseGuidance         string // independent of optional traffic binding

	FindingGuidance string            // derived from the final permitted tools, including DB overrides
	Deferred        []string          // all MCP tool names (schema withheld)
	GlobalNames     []string          // MCP names to list in the system-prompt block
	Unlock          *actool.UnlockSet // shared call-gate; nil when no MCP tools
	UnlockSkill     func(skillName string)
}

// ToolAugment, if set, returns the EXTRA tools an agent should see beyond its
// built-in base set — the agent's visible skills (packed into one Skill meta-tool)
// and visible MCP servers (expanded to mcp__server__tool). It also returns the
// DeferredInfo describing how those MCP tools are deferred/gated. The server wires
// it to the PG agent_visibility table. cleanup releases any spawned MCP clients.
//
// When nil, agents run with only their built-in tools — behavior is unchanged
// until a user assigns a skill/MCP to the agent in the UI.
var ToolAugment func(ctx context.Context, agentKey string) (extra []actool.CoreTool, def DeferredInfo, cleanup func())

// AugmentTools returns base plus the agent's visible skill/MCP tools, the
// DeferredInfo, and a cleanup func the caller must defer (closes MCP clients).
// The catalog can narrow built-in capabilities. Metadata failures are explicit
// errors and never restore an unfiltered base or the SDK's default toolset.
func AugmentTools(ctx context.Context, agentKey string, base []actool.CoreTool) ([]actool.CoreTool, DeferredInfo, func()) {
	if agentKey == "worker.intranet" {
		return []actool.CoreTool{}, DeferredInfo{AssemblyError: fmt.Errorf("worker.intranet 是 worker 的任务提示词变体，不支持独立执行")}, func() {}
	}
	if agentKey == db.CredentialAgentKey {
		// The processor's contract is data-only even when a conversation or
		// custom trigger reaches the generic chat runner. Do not even initialize
		// Skill/MCP/host dependencies for legacy misconfigured grants.
		return []actool.CoreTool{}, DeferredInfo{}, func() {}
	}
	var (
		def     DeferredInfo
		cleanup = func() {}
		out     = base
	)
	if ToolAugment != nil {
		var extra []actool.CoreTool
		var cl func()
		extra, def, cl = ToolAugment(ctx, agentKey)
		if cl != nil {
			cleanup = cl
		}
		if def.AssemblyError != nil {
			return []actool.CoreTool{}, def, cleanup
		}
		if len(extra) > 0 {
			out = append(append([]actool.CoreTool{}, base...), extra...)
		}
	}
	if err := toolAssemblyError(out); err != nil {
		def.AssemblyError = err
		return []actool.CoreTool{}, def, cleanup
	}
	// DB tools table has the final say on the built-in tools: drop the ones this
	// agent isn't bound to (or that are disabled) and swap in overridden
	// descriptions/schemas + default injection. MCP/skill/host tools have no row
	// and pass through untouched, so deferred/unlock wiring stays consistent.
	if ToolResolve != nil {
		out = ToolResolve(ctx, agentKey, out)
	}
	if err := toolAssemblyError(out); err != nil {
		def.AssemblyError = err
		return []actool.CoreTool{}, def, cleanup
	}
	if agentKey == "goals" {
		limited := out[:0:0]
		for _, tool := range out {
			if IsGoalsToolAllowed(tool.Name()) {
				limited = append(limited, tool)
				if tool.Name() == "read_skill_reference" {
					def.SkillReadGuidance = "\n\n【目标拆解的受控知识读取】Skill 索引中的 Read 文档请使用 read_skill_reference，将已展开的绝对路径传给 path；仅可读取本轮已授权 Skill 的 SKILL.md 或 refs/references 下 Markdown、文本。此工具不是通用 Read，不执行脚本、不读取配置、不解锁 MCP。\n"
				}
			}
		}
		out = limited
		// Goal decomposition can read authorized guidance, but never unlocks
		// execution dependencies while its limited write tools use bypass mode.
		def.Deferred, def.GlobalNames, def.Unlock, def.UnlockSkill = nil, nil, nil, nil
	}
	if agentKey == "reporter" && isFindingCaseReview(ctx) {
		out = historicalFindingCaseTools(out)
	}
	out, def.FindingGuidance = findingWorkflowTools(agentKey, out)
	out, def.CaseGuidance = findingCaseWorkflow(agentKey, out)
	out, def.IntelligenceGuidance = intelligenceWorkflow(ctx, out)
	if agentKey == "reporter" && isFindingCaseReview(ctx) {
		def.FindingGuidance = ""
		def.CaseGuidance += historicalFindingReviewGuidance
	}
	for i, t := range out {
		out[i] = guardPanic(t)
	}
	// norma.NewSession treats nil Tools as a request for SDK DefaultTools.
	// An intentionally empty permitted set must remain explicitly non-nil.
	if out == nil {
		out = []actool.CoreTool{}
	}
	return out, def, cleanup
}

// IsGoalsToolAllowed is the hard capability ceiling for goal decomposition.
// Catalog bindings can narrow these tools, but cannot add shell, network or
// general-purpose execution capabilities to this limited runner.
func IsGoalsToolAllowed(name string) bool {
	switch name {
	case "set_goals", "set_constraints", "add_task_scope", "Skill", "read_skill_reference":
		return true
	default:
		return false
	}
}

type assemblyFailureTool struct {
	actool.CoreTool
	cause error
}

func toolAssemblyError(tools []actool.CoreTool) error {
	for _, tool := range tools {
		if failure, ok := tool.(*assemblyFailureTool); ok {
			return failure.cause
		}
	}
	return nil
}

// ToolAssemblyFailure preserves the legacy resolver signature without treating
// a metadata failure as an empty successful toolset or returning unfiltered
// capabilities. AugmentTools converts this inert marker into AssemblyError.
func ToolAssemblyFailure(err error) []actool.CoreTool {
	if err == nil {
		err = fmt.Errorf("工具装配元数据不可用")
	}
	tool := actool.Build(actool.Spec{
		Name: "tool_assembly_unavailable", Description: "工具装配未完成，本轮已停止。",
		Schema:   map[string]any{"type": "object", "properties": map[string]any{}},
		ReadOnly: func(json.RawMessage) bool { return true },
		Run: func(context.Context, json.RawMessage, *actool.ToolContext) (actool.Result, error) {
			return actool.Errorf("工具装配未完成，本轮已停止。"), nil
		},
	})
	return []actool.CoreTool{&assemblyFailureTool{CoreTool: tool, cause: err}}
}

// guardPanic turns a panicking tool handler into an ordinary tool error. The
// harness runs each tool on its own goroutine, so a panic inside a handler can't
// be recovered by the caller that started the run — it takes the whole process
// down, and on restart the agent replays the same call and crashes again. Applied
// last, so it covers every tool the agent can reach: domain, SDK, MCP and skill.
func guardPanic(t actool.CoreTool) actool.CoreTool { return &guardedTool{CoreTool: t} }

type guardedTool struct{ actool.CoreTool }

func (g *guardedTool) Call(ctx context.Context, in json.RawMessage, tc *actool.ToolContext) (res actool.Result, err error) {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("[tools] %s panic: %v\n%s", g.Name(), r, debug.Stack())
			res, err = actool.Errorf(fmt.Sprintf("工具 %s 内部错误：%v（本次调用已失败，可换个参数或改用别的工具）", g.Name(), r)), nil
		}
	}()
	return g.CoreTool.Call(ctx, in, tc)
}
