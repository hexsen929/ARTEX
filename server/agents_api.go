package server

import (
	"encoding/json"
	"log"
	"net/http"
	"regexp"
	"strings"

	"github.com/Autumn-27/artex/agent"
	"github.com/Autumn-27/artex/db"
)

// reAgentKey mirrors the agents.key DB check: lowercase letter start, then
// lowercase letters / digits / underscores.
var reAgentKey = regexp.MustCompile(`^[a-z][a-z0-9_]*$`)

func (s *Server) pgListAgents(w http.ResponseWriter, r *http.Request) {
	pg := s.pg(w)
	if pg == nil {
		return
	}
	ags, err := pg.ListAgents()
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	dtos := agentDTOs(ags)
	// overlay per-agent binding counts (mcp/skill by id, tools by key) — best-effort.
	if mcp, skill, tools, err := pg.AgentBindingCounts(); err == nil {
		for i := range dtos {
			dtos[i].McpCount = mcp[ags[i].ID]
			dtos[i].SkillCount = skill[ags[i].ID]
			dtos[i].ToolCount = tools[ags[i].Key]
		}
	}
	writeJSON(w, 200, map[string]any{"agents": dtos})
}

// pgCreateAgent creates a CUSTOM conversational agent (builtin=false, role
// 'assistant') and seeds it a starter prompt so its editor isn't blank.
func (s *Server) pgCreateAgent(w http.ResponseWriter, r *http.Request) {
	pg := s.pg(w)
	if pg == nil {
		return
	}
	var req struct{ Key, Name, Description string }
	if err := decode(r, &req); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	req.Key, req.Name = strings.TrimSpace(req.Key), strings.TrimSpace(req.Name)
	if !reAgentKey.MatchString(req.Key) {
		writeErr(w, 400, "key 需小写字母开头，仅含小写字母/数字/下划线")
		return
	}
	if req.Name == "" {
		writeErr(w, 400, "名称不能为空")
		return
	}
	if exist, _ := pg.GetAgentByKey(req.Key); exist != nil {
		writeErr(w, 409, "该 key 已存在")
		return
	}
	a, err := pg.CreateAgent(req.Key, req.Name, req.Description)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	// starter prompt so the editor shows something editable from the start.
	if err := pg.SeedPromptIfEmpty(a.ID, agent.DefaultAssistantPrompt); err != nil {
		log.Printf("[agents] seed starter prompt for %s 失败: %v", a.Key, err)
	}
	writeJSON(w, 200, agentDTO(a))
}

// pgUpdateAgent updates a custom agent's name/description (built-in agents rejected).
func (s *Server) pgUpdateAgent(w http.ResponseWriter, r *http.Request) {
	pg, a, ok := s.agentByKey(w, r)
	if !ok {
		return
	}
	if a.Builtin {
		writeErr(w, 400, "内置 agent 不可修改名称/描述")
		return
	}
	var req struct{ Name, Description string }
	if err := decode(r, &req); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	req.Name = strings.TrimSpace(req.Name)
	if req.Name == "" {
		writeErr(w, 400, "名称不能为空")
		return
	}
	if err := pg.UpdateAgentMeta(a.Key, req.Name, req.Description); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true})
}

// pgDeleteAgent removes a custom agent (built-in rejected). Prompts/vars/visibility
// cascade via FK; tool-binding cleanup is best-effort.
func (s *Server) pgDeleteAgent(w http.ResponseWriter, r *http.Request) {
	pg, a, ok := s.agentByKey(w, r)
	if !ok {
		return
	}
	if a.Builtin {
		writeErr(w, 400, "内置 agent 不可删除")
		return
	}
	if err := pg.DeleteAgent(a.Key); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	if err := pg.RemoveAgentFromToolBindings(a.Key); err != nil {
		log.Printf("[agents] 清理 %s 工具绑定失败: %v", a.Key, err)
	}
	if err := pg.DeleteTriggersForAgent(a.Key); err != nil {
		log.Printf("[agents] 清理 %s 触发器失败: %v", a.Key, err)
	}
	writeJSON(w, 200, map[string]any{"deleted": a.Key})
}

func (s *Server) agentByKey(w http.ResponseWriter, r *http.Request) (*db.DB, *db.Agent, bool) {
	pg := s.pg(w)
	if pg == nil {
		return nil, nil, false
	}
	a, err := pg.GetAgentByKey(r.PathValue("key"))
	if err != nil {
		writeErr(w, 500, err.Error())
		return nil, nil, false
	}
	if a == nil {
		writeErr(w, 404, "agent not found")
		return nil, nil, false
	}
	return pg, a, true
}

// pgSaveAgentConfig updates an agent's runtime config (currently max_turns) and
// re-applies the live LLM so the change takes effect immediately.
func (s *Server) pgSaveAgentConfig(w http.ResponseWriter, r *http.Request) {
	pg, a, ok := s.agentByKey(w, r)
	if !ok {
		return
	}
	// All fields optional (pointers) so a partial patch (e.g. the triggers tab sending
	// only trigger_* fields) leaves the untouched settings alone instead of resetting
	// max_turns/run_seconds to 0.
	var req struct {
		MaxTurns         *int  `json:"max_turns"`
		RunSeconds       *int  `json:"run_seconds"`
		WebSearch        *bool `json:"web_search"`
		InteractiveShell *bool `json:"interactive_shell"`
		// llm_profile_id 三态:字段缺省=不动;显式 null=解绑(跟随任务/全局);数字=绑定该 profile。
		LLMProfileID json.RawMessage `json:"llm_profile_id"`
		// P3 触发后处理策略(三者一起可选,提供任一即整体写入;未提供则不动)。
		TriggerRunMode     *string `json:"trigger_run_mode"`
		TriggerMergeMode   *string `json:"trigger_merge_mode"`
		TriggerMaxParallel *int    `json:"trigger_max_parallel"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	profileChanged := false
	if req.LLMProfileID != nil { // key present (数字 或 null)
		var id *int64
		if err := json.Unmarshal(req.LLMProfileID, &id); err != nil {
			writeErr(w, 400, "llm_profile_id 格式错误")
			return
		}
		if id != nil { // 绑定:校验目标 profile 有效
			if _, ok := s.loadProfileConfig(*id); !ok {
				writeErr(w, 400, "指定的 LLM 配置不存在或无效")
				return
			}
		}
		if err := pg.SetAgentLLMProfile(a.Key, id); err != nil {
			writeErr(w, 500, err.Error())
			return
		}
		profileChanged = true
	}
	if req.MaxTurns != nil {
		mt := *req.MaxTurns
		if mt < 0 {
			mt = 0
		}
		if err := pg.SetAgentMaxTurns(a.Key, mt); err != nil {
			writeErr(w, 500, err.Error())
			return
		}
	}
	if req.RunSeconds != nil {
		rs := *req.RunSeconds
		if rs < 0 {
			rs = 0
		}
		if err := pg.SetAgentRunSeconds(a.Key, rs); err != nil {
			writeErr(w, 500, err.Error())
			return
		}
	}
	if req.WebSearch != nil {
		if err := pg.SetAgentWebSearch(a.Key, *req.WebSearch); err != nil {
			writeErr(w, 500, err.Error())
			return
		}
	}
	if req.InteractiveShell != nil {
		if err := pg.SetAgentInteractiveShell(a.Key, *req.InteractiveShell); err != nil {
			writeErr(w, 500, err.Error())
			return
		}
	}
	// P3 触发策略:三者作为一组写入(SetAgentTriggerBehavior 一次写三列),缺的字段用
	// 当前存量值回填,避免只传一个把另两个覆盖成默认。
	if req.TriggerRunMode != nil || req.TriggerMergeMode != nil || req.TriggerMaxParallel != nil {
		runMode, mergeMode, maxPar := a.TriggerRunMode, a.TriggerMergeMode, a.TriggerMaxParallel
		if req.TriggerRunMode != nil {
			runMode = *req.TriggerRunMode
		}
		if req.TriggerMergeMode != nil {
			mergeMode = *req.TriggerMergeMode
		}
		if req.TriggerMaxParallel != nil {
			maxPar = *req.TriggerMaxParallel
		}
		if err := pg.SetAgentTriggerBehavior(a.Key, runMode, mergeMode, maxPar); err != nil {
			writeErr(w, 500, err.Error())
			return
		}
	}
	// an agent's LLM binding change also invalidates pinned-task / per-profile caches
	// so their next round re-resolves each agent's bound model.
	if profileChanged {
		s.invalidateProfileAgents()
	}
	// Task bundles capture operational Agent settings (turn/time budgets, tools,
	// web search). Rebuild them even when no global profile is configured.
	s.invalidateTaskAgents()
	// rebuild the live agents so the new max_turns/run_seconds/binding apply without a restart.
	s.cfgMu.Lock()
	cfg, on := s.llmCfg, s.llmOn
	s.cfgMu.Unlock()
	if on {
		_ = s.applyLLM(cfg)
	}
	resp := map[string]any{"ok": true}
	if req.MaxTurns != nil {
		resp["max_turns"] = *req.MaxTurns
	}
	if req.RunSeconds != nil {
		resp["run_seconds"] = *req.RunSeconds
	}
	writeJSON(w, 200, resp)
}

func (s *Server) pgGetAgent(w http.ResponseWriter, r *http.Request) {
	pg, a, ok := s.agentByKey(w, r)
	if !ok {
		return
	}
	cur, _ := pg.CurrentPrompt(a.ID)
	vars, _ := pg.PromptVars(a.ID)
	vars = withGlobalVars(vars)
	vers, _ := pg.ListPromptVersions(a.ID)
	if vers == nil {
		vers = []db.PromptVersion{}
	}
	mcp, _ := pg.AgentVisible(a.ID, "mcp")
	sk, _ := pg.AgentSkillNames(a.ID)
	if sk == nil {
		sk = []string{}
	}
	// 可选 LLM 配置列表(id/name/model/是否默认),供前端渲染 "默认模型" 下拉;当前绑定见 agent.llm_profile_id。
	profs, _ := pg.ListProfiles()
	llmProfiles := make([]map[string]any, 0, len(profs))
	for _, p := range profs {
		llmProfiles = append(llmProfiles, map[string]any{
			"id": p.ID, "name": p.Name, "model": p.Model, "is_default": p.IsDefault,
		})
	}
	writeJSON(w, 200, map[string]any{
		"agent": agentDTO(a), "prompt": cur, "variables": vars, "versions": vers,
		"visibility":   map[string]any{"mcp": mcp, "skill": sk},
		"llm_profiles": llmProfiles, // 可绑定的 LLM 配置候选

		"wrapup_prompt":            a.WrapupPrompt,                  // 已保存的收尾提示词(空=用内置默认)
		"wrapup_default":           agent.WrapupDefault(a.Key),      // 内置默认(供占位/恢复默认)
		"wrapup_max_turns":         a.WrapupMaxTurns,                // 已保存的收尾轮数(0=用内置默认)
		"wrapup_max_turns_default": agent.WrapupTurnsDefault(a.Key), // 内置默认轮数(供 "0=默认N" 提示)
		// 任务级超时收尾词(仅 worker/planner 有内置默认;task_timeout_supported 供前端决定是否显示该分区)
		"task_timeout_wrapup_supported":         agent.TaskTimeoutWrapupDefault(a.Key) != "",
		"task_timeout_wrapup_prompt":            a.TaskTimeoutWrapupPrompt,
		"task_timeout_wrapup_default":           agent.TaskTimeoutWrapupDefault(a.Key),
		"task_timeout_wrapup_max_turns":         a.TaskTimeoutWrapupMaxTurns,
		"task_timeout_wrapup_max_turns_default": agent.WrapupTurnsDefault(a.Key),
	})
}

func (s *Server) pgGetAgentVisibility(w http.ResponseWriter, r *http.Request) {
	pg, a, ok := s.agentByKey(w, r)
	if !ok {
		return
	}
	mcp, _ := pg.AgentVisible(a.ID, "mcp")
	sk, _ := pg.AgentSkillNames(a.ID)
	if sk == nil {
		sk = []string{}
	}
	writeJSON(w, 200, map[string]any{"mcp": mcp, "skill": sk})
}

func (s *Server) pgSetAgentVisibility(w http.ResponseWriter, r *http.Request) {
	pg, a, ok := s.agentByKey(w, r)
	if !ok {
		return
	}
	var body struct {
		MCP   []int64  `json:"mcp"`
		Skill []string `json:"skill"`
	}
	if err := decode(r, &body); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	if err := pg.SetAgentVisibilityKind(a.ID, "mcp", body.MCP); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	if err := pg.SetAgentSkillVisibility(a.ID, body.Skill); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true})
}
