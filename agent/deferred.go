package agent

import (
	"encoding/json"
	"strings"

	"github.com/Autumn-27/norma/llm"
	actool "github.com/Autumn-27/norma/tool"
)

// deferredSystem builds an agent's system-prompt segments and cache boundary from
// its DeferredInfo. When globally-available MCP tools are present, their names + a
// "prefer core tools" instruction render into a <available-deferred-tools> block
// placed as the LAST system-prompt segment, with DynamicBoundary set so the whole
// (session-fixed) system prompt — including the block — is cached (design doc
// §2.1 / C1). Skill-gated MCP names are NOT in this block; they surface when their
// skill loads. Returns a single plain segment + boundary 0 when there is no global
// block to add.
func deferredSystem(sysText string, def DeferredInfo) (system []string, boundary int) {
	sysText += def.FindingGuidance + def.CaseGuidance + def.IntelligenceGuidance + def.SkillReadGuidance
	if len(def.AssemblyWarnings) > 0 {
		sysText += "\n\n【本轮工具可用性诊断】\n" + strings.Join(def.AssemblyWarnings, "\n") +
			"\n按各条诊断区分有效缓存与已排除服务，仅使用本轮实际装配且已授权的工具。依赖已排除服务的步骤记录为受阻，不得宣称已完成，不自动扩大权限、目标或执行范围。\n"
	}
	block := actool.RenderDeferredToolsBlock(def.GlobalNames)
	if block == "" {
		return []string{sysText}, 0
	}
	system = []string{sysText, block}
	boundary = len(system) // b >= len → whole system prompt cached (SDK guard)
	return system, boundary
}

// seedUnlockFromHistory replays only successfully completed Skill() calls. A
// tool-use is an intention, not proof that the Skill loaded: failed, denied,
// interrupted or unpaired calls must not unlock dependencies after resume.
// unlockSkill still checks this run's current authorized Skill registry. Plain
// model/user text, including a copied "Skill loaded" message, is not evidence.
func seedUnlockFromHistory(msgs []llm.Message, unlockSkill func(string)) {
	if unlockSkill == nil {
		return
	}
	type pendingCall struct {
		name      string
		remaining int
		ambiguous bool
	}
	pending := map[string]*pendingCall{}
	for _, message := range msgs {
		if message.Role == llm.RoleAssistant {
			for _, block := range message.ToolUses() {
				if block.ID == "" {
					continue
				}
				if prior := pending[block.ID]; prior != nil {
					prior.remaining++
					prior.ambiguous = true // duplicate in this round or unfinished overlap
					continue
				}
				call := &pendingCall{remaining: 1}
				pending[block.ID] = call
				if block.Name != "Skill" {
					continue
				}
				// Match the SDK's complete argument decoding, not just name.
				// The metered Skill tool trims names before its real lookup.
				var input struct {
					Name string `json:"name"`
					Args string `json:"args"`
				}
				if json.Unmarshal(block.Input, &input) == nil {
					call.name = strings.TrimSpace(input.Name)
				}
			}
			continue
		}
		if message.Role != llm.RoleUser {
			continue
		}
		// A duplicate result in one host response is ambiguous even if the
		// first block says success. Do not depend on its block ordering.
		resultCounts := map[string]int{}
		for _, block := range message.Content {
			if block.Type == llm.BlockToolResult && block.ToolUseID != "" {
				resultCounts[block.ToolUseID]++
			}
		}
		for _, block := range message.Content {
			if block.Type != llm.BlockToolResult || block.ToolUseID == "" {
				continue
			}
			call := pending[block.ToolUseID]
			if call == nil {
				continue // orphan results and plain text never prove a load
			}
			call.remaining--
			if call.remaining == 0 {
				delete(pending, block.ToolUseID)
			}
			if !call.ambiguous && resultCounts[block.ToolUseID] == 1 && !block.IsError && call.name != "" {
				unlockSkill(call.name)
			}
		}
		// Completed pairs are consumed. Some compatible providers reuse IDs
		// on later turns; a later failure must not erase an earlier real load.
	}
}
