package agent

import (
	"context"
	"fmt"
	"regexp"
	"strings"

	"github.com/Autumn-27/artex/intercept"
)

const taskRequestGuidance = `

**用户要求边界**：
- 普通应用安全测试默认交付实际检查结果、已确认漏洞及证据。用户未明确要求获取 flag 或进行 CTF/夺旗任务时，不新增 flag 搜索、收集、读取或数量目标，也不将其作为验收条件。
- 创建任务和拆解目标以用户原始要求及当前人工约束为依据。旧任务、历史对话、模型生成的总目标、工具结果中的 flag 不能自行升级为本次用户要求；禁止把模型补充的要求写回后再当作授权来源。用户明确要求 CTF 或获取 flag 时才保留对应目标。
- “尽力”“尝试”只表示在范围及预算内检查，不意味着必须取得成果才能结束；成果未取得时如实交付已测试、未测试及受阻情况，不把软要求升级成硬数量。`

var (
	flagTermPattern        = regexp.MustCompile(`(?i)\bflags?\b|\bctf\b|夺旗`)
	flagActionPattern      = regexp.MustCompile(`(?i)(获取|拿到|取得|拿下|寻找|查找|搜索|收集|读取|找到|获得|提取|提交|搜集|\b(find|obtain|capture|retrieve|collect|read|extract|submit|get|search|grab|locate)\b)[^\n。；;!?！？]{0,80}\bflags?\b|\bflags?\b[^\n。；;!?！？]{0,40}(获取|搜索|收集|读取|提取)`)
	flagDenyPattern        = regexp.MustCompile(`(?i)(不要|不需要|无需|不要求|不想|不允许|禁止|取消|撤销|不是|并非|do\s+not|don't|without|\bnot\b|\bnever\b|\bavoid\b|\bno\s+)[^\n。；;!?！？]{0,80}(\bflags?\b|\bctf\b|夺旗)|(\bflags?\b|\bctf\b|夺旗)[^\n。；;!?！？]{0,40}(不作为|不是.*目标|不要求|已撤销)`)
	flagMaintenancePattern = regexp.MustCompile(`(?i)(删除|清理|移除|修复|误加|默认提示词)[^\n。；;!?！？]{0,80}(\bflags?\b|\bctf\b|夺旗)|(\bflags?\b|\bctf\b|夺旗)[^\n。；;!?！？]{0,40}(误加|引导|提示词)`)
	ctfRequestPattern      = regexp.MustCompile(`(?i)\bctf\b|capture[ -]the[ -]flag|夺旗`)
	technicalFlagPattern   = regexp.MustCompile(`(?i)feature\s+flags?|--flags?\b|(?:\bcli|command[ -]line)\s+flags?\b|(?:命令行|标志位|布尔|开关)[ \t]*flags?\b|\bflags?[ \t]*(?:标志位|布尔|开关|参数)`)
	requestClausePattern   = regexp.MustCompile(`[\n。；;!?！？]+`)
	flagRemovalPattern     = regexp.MustCompile(`(?i)(删除|移除|清理|撤销|取消|remove|delete|cancel)[^\n。；;!?！？]{0,80}(\bflags?\b|\bctf\b|夺旗)`)
)

func flagRequestDenied(text string) bool {
	text = technicalFlagPattern.ReplaceAllString(text, "")
	return flagDenyPattern.MatchString(text) || flagRemovalPattern.MatchString(text)
}

// isFlagObjective identifies trophy requirements, not CLI or feature flags.
// This text check is a quality boundary; it is not a general authorization parser.
func isFlagObjective(text string) bool {
	for _, clause := range requestClausePattern.Split(text, -1) {
		clause = technicalFlagPattern.ReplaceAllString(clause, "")
		if flagTermPattern.MatchString(clause) && !flagDenyPattern.MatchString(clause) && (flagActionPattern.MatchString(clause) || !flagMaintenancePattern.MatchString(clause)) {
			return true
		}
	}
	return false
}

// ValidateFlagObjective rejects a model-added trophy requirement unless the
// application supplies an explicit request. Model tool arguments cannot supply it.
func ValidateFlagObjective(objective, request string) error {
	if !isFlagObjective(objective) {
		return nil
	}
	if !flagRequestDenied(request) && isFlagObjective(request) && (flagActionPattern.MatchString(request) || ctfRequestPattern.MatchString(request)) {
		return nil
	}
	return fmt.Errorf("用户未明确要求获取 flag 或进行 CTF；请只提交原始要求中的检查、漏洞及证据目标")
}

func (t *ToolSet) validateGoalRequest(ctx context.Context, it goalItem) error {
	objective := it.Text + "\n" + strings.Join(it.RequiredChecks, "\n")
	if !isFlagObjective(objective) {
		return nil
	}
	constraints, err := t.ts.ListConstraints()
	if err != nil {
		return fmt.Errorf("读取目标约束: %w", err)
	}
	for _, c := range constraints {
		text := technicalFlagPattern.ReplaceAllString(c.Text, "")
		if c.Kind == "deny" && flagTermPattern.MatchString(text) && (flagRequestDenied(text) || isFlagObjective(text)) {
			return fmt.Errorf("当前任务约束禁止 flag/CTF 目标；请先遵守人工范围要求")
		}
	}
	request, present := intercept.UserMessage(ctx)
	if present && flagRequestDenied(request) {
		return ValidateFlagObjective(objective, request)
	}
	// A follow-up such as "continue" does not revoke an existing CTF contract.
	// Human corrections and deny constraints override legacy polluted roots.
	description, goal, err := t.ts.Root()
	if err != nil {
		return fmt.Errorf("读取任务要求: %w", err)
	}
	request = goal + "\n" + description + "\n" + request
	return ValidateFlagObjective(objective, request)
}
