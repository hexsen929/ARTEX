package server

import (
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/Autumn-27/artex/db"
	"github.com/Autumn-27/norma/skill"
)

const maxSkillDependencyWarnings = 16

// skillMCPDependencyWarnings observes the exact execution-role/variant rule used
// by wireAgentAugment. It neither changes grants nor connects/starts a service.
// Transport/schema failures of enabled and authorized dependencies are reported
// by the ordinary MCP assembly path instead of being duplicated here.
func skillMCPDependencyWarnings(reg *skill.Registry, servers []*db.MCPServer, direct map[int64]bool, separateSkillRole bool, baseSkillMCPs map[string]bool) []string {
	warnings := []string{}
	if reg == nil {
		return warnings
	}
	byName := make(map[string]*db.MCPServer, len(servers))
	for _, server := range servers {
		if server != nil {
			byName[server.Name] = server
		}
	}
	seen := map[string]bool{}
	omitted := 0
	for _, entry := range reg.List() {
		for _, dependency := range entry.MCPs {
			category := ""
			server := byName[dependency]
			switch {
			case server == nil:
				category = "服务未登记"
			case !server.Enabled:
				category = "服务已停用"
			case separateSkillRole && !direct[server.ID] && !baseSkillMCPs[server.Name]:
				category = "执行角色未获该依赖授权"
			}
			if category == "" {
				continue
			}
			key := entry.Name + "\x00" + dependency
			if seen[key] {
				continue
			}
			seen[key] = true
			if len(warnings) >= maxSkillDependencyWarnings {
				omitted++
				continue
			}
			warnings = append(warnings, fmt.Sprintf("Skill %q 的 MCP 依赖 %q 未装配（%s）；指导可加载，依赖工具不可用，不会自动启用服务或扩大授权。",
				dependencyDisplayName(entry.Name), dependencyDisplayName(dependency), category))
		}
	}
	if omitted > 0 {
		warnings = append(warnings, fmt.Sprintf("另有 %d 项 Skill MCP 依赖未装配；请检查各 Skill 依赖声明、服务开关及执行角色授权。", omitted))
	}
	return warnings
}

// Dependency declarations are index text, not trusted diagnostic content. Do
// not echo URLs, authorization fragments, controls or unbounded strings into
// the system prompt/log. Ordinary server/Skill identifiers remain actionable.
func dependencyDisplayName(name string) string {
	if name == "" || len(name) > 96 || !utf8.ValidString(name) || strings.TrimSpace(name) != name {
		return "[名称未通过显示校验]"
	}
	for _, r := range name {
		if !unicode.IsLetter(r) && !unicode.IsDigit(r) && !strings.ContainsRune("_.-", r) {
			return "[名称未通过显示校验]"
		}
	}
	return name
}
