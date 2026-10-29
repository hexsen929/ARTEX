package server

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"path/filepath"
	"strings"
	"time"

	"github.com/Autumn-27/artex/agent"
	"github.com/Autumn-27/artex/db"
	"github.com/Autumn-27/artex/traffic"
	"github.com/Autumn-27/norma/skill"
	actool "github.com/Autumn-27/norma/tool"
)

// wireAgentAugment connects the PG agent_visibility table into the agent runtime:
// an agent's visible skills are loaded from the filesystem and packed into one
// Skill meta-tool; cached stdio MCP tools are exposed without spawning servers.
// The first actual call starts the run's private connection. Tools expand to
// mcp__server__tool. skillDir is the root directory of all skill subdirectories.
// hostTools, if set, returns runtime host tools (currently the traffic tools when
// capture is on) to add to EVERY agent's base list — the DB tools table then
// filters them per-agent binding. Empty/nil → no host tools this run (capture off).
func wireAgentAugment(pg *db.DB, skillDir string, hostTools func() ([]actool.CoreTool, map[string][]string), taskDialers ...taskMCPDialer) {
	agent.ToolAugment = func(ctx context.Context, agentKey string) ([]actool.CoreTool, agent.DeferredInfo, func()) {
		if agentKey == "worker.intranet" {
			return []actool.CoreTool{}, agent.DeferredInfo{AssemblyError: fmt.Errorf("内网提示词变体不支持独立执行，请使用 worker 任务运行")}, nil
		}
		skillAgentKey := agent.SkillVisibilityAgent(ctx, agentKey)
		separateSkillRole := skillAgentKey != agentKey
		a, err := pg.GetAgentByKey(agentKey)
		if err != nil || a == nil || !a.Enabled {
			return []actool.CoreTool{}, agent.DeferredInfo{AssemblyError: fmt.Errorf("执行角色 %s 不可用，未装配工具", agentKey)}, nil
		}
		if agentKey == db.CredentialAgentKey || agentKey == "asset_mapper" {
			// Data-only/asset-read-only roles do not initialize legacy Skill,
			// MCP, browser, host-network or interactive execution dependencies.
			return []actool.CoreTool{}, agent.DeferredInfo{}, nil
		}
		var extra []actool.CoreTool
		var taskDial taskMCPDialer
		if len(taskDialers) > 0 {
			taskDial = taskDialers[0]
		}
		taskID := agent.RunInfoFrom(ctx).TaskID

		// --- skills: load visible skills into reg (used for the Skill meta-tool
		// and to know which MCP servers are skill-gated). ---
		skillAgent := a
		if separateSkillRole {
			skillAgent, err = pg.GetAgentByKey(skillAgentKey)
			if err != nil || skillAgent == nil || !skillAgent.Enabled {
				return []actool.CoreTool{}, agent.DeferredInfo{AssemblyError: fmt.Errorf("读取 Skill 角色 %s 失败", skillAgentKey)}, nil
			}
		}
		names, skillErr := pg.AgentSkillNames(skillAgent.ID)
		if skillErr != nil {
			return []actool.CoreTool{}, agent.DeferredInfo{AssemblyError: fmt.Errorf("读取 Skill 角色 %s 的授权失败，未装配工具", skillAgentKey)}, nil
		}
		// A prompt variant may choose a different Skill set, but cannot obtain a
		// new MCP grant merely by referencing a server from that Skill. Preserve
		// the execution role's direct grants and its ordinary Skill-gated grants.
		baseSkillMCPs := map[string]bool{}
		var baseSkillNames []string
		if separateSkillRole {
			baseSkillNames, err = pg.AgentSkillNames(a.ID)
			if err != nil {
				return []actool.CoreTool{}, agent.DeferredInfo{AssemblyError: fmt.Errorf("读取执行角色 %s 的 Skill 授权失败，未装配工具", agentKey)}, nil
			}
		}
		var reg *skill.Registry
		skillDirs := map[string]string{}
		if len(names) > 0 || len(baseSkillNames) > 0 {
			nameSet := make(map[string]bool, len(names))
			for _, n := range names {
				nameSet[n] = true
			}
			baseNameSet := make(map[string]bool, len(baseSkillNames))
			for _, n := range baseSkillNames {
				baseNameSet[n] = true
			}
			root, rootErr := filepath.Abs(skillDir)
			if rootErr != nil {
				return []actool.CoreTool{}, agent.DeferredInfo{AssemblyError: fmt.Errorf("Skill 目录不可用，未装配工具")}, nil
			}
			catalog, catalogErr := inspectSkillCatalog(root)
			if catalogErr != nil {
				return []actool.CoreTool{}, agent.DeferredInfo{AssemblyError: fmt.Errorf("读取 Skill 库失败，未装配工具")}, nil
			}
			expected := map[string]bool{}
			for _, name := range append(append([]string{}, names...), baseSkillNames...) {
				expected[name] = true
				entry, exists := catalog[name]
				if !exists || !entry.Loadable {
					return []actool.CoreTool{}, agent.DeferredInfo{AssemblyError: fmt.Errorf("已授权 Skill %s 不可加载，请检查技能目录", name)}, nil
				}
			}
			allReg, loadErr := skill.LoadDir(root)
			if loadErr != nil || allReg == nil {
				return []actool.CoreTool{}, agent.DeferredInfo{AssemblyError: fmt.Errorf("读取 Skill 库失败，未装配工具")}, nil
			}
			loaded := map[string]bool{}
			for _, entry := range allReg.List() {
				if entry.Dir != "" {
					loaded[filepath.Base(entry.Dir)] = true
				}
			}
			for name := range expected {
				if !loaded[name] {
					return []actool.CoreTool{}, agent.DeferredInfo{AssemblyError: fmt.Errorf("已授权 Skill %s 未进入注册表，请检查技能名称冲突", name)}, nil
				}
			}
			if loadErr == nil && allReg != nil {
				reg = skill.NewRegistry()
				for _, s := range allReg.List() {
					// match by directory name (Base of Dir), not by skill display Name
					if s.Dir != "" && nameSet[filepath.Base(s.Dir)] {
						reg.Add(normalizeLegacyChainReferences(s))
						skillDirs[filepath.Base(s.Dir)] = s.Dir
					}
					if s.Dir != "" && baseNameSet[filepath.Base(s.Dir)] {
						for _, srv := range s.MCPs {
							baseSkillMCPs[srv] = true
						}
					}
				}
				if len(reg.List()) == 0 {
					reg = nil
				}
			}
		}
		if agentKey == "goals" {
			// This runner uses a deliberately limited bypass mode for its own
			// graph writes. Guidance loading must not introduce execution tools,
			// network/MCP connections, or dependency-unlock callbacks.
			if reg != nil {
				extra = append(extra, meterSkillTool(reg.Tool(), pg, reg, a.Key, agent.RunInfoFrom(ctx)))
			}
			reference, closeReferences, err := newGoalsSkillReferenceTool(skillDirs)
			if err != nil {
				return []actool.CoreTool{}, agent.DeferredInfo{AssemblyError: fmt.Errorf("目标拆解 Skill 参考目录不可用，未装配工具")}, closeReferences
			}
			if reference != nil {
				extra = append(extra, reference)
			}
			return extra, agent.DeferredInfo{SkillDirs: skillDirs}, closeReferences
		}

		// A server named by any visible skill's `mcps:` is skill-gated: its tools
		// are deferred + locked (not in the global block) until that skill loads.
		gated := map[string]bool{}
		if reg != nil {
			for _, s := range reg.List() {
				for _, srv := range s.MCPs {
					gated[srv] = true
				}
			}
		}

		// --- mcp: expose enabled servers that are directly visible to this agent
		// OR skill-gated (named in a visible skill's mcps field). Directly-visible
		// and NOT gated → global (unlocked from session start). Skill-gated →
		// deferred until that skill is invoked (regardless of direct visibility).
		var closers []io.Closer
		var warnings []string
		warnUnavailable := func(name, category string) {
			// Category is code-owned, not an error string that might include a
			// private endpoint, authorization header or server-side response.
			warning := fmt.Sprintf("MCP %q 当前不可用（%s），本轮未装配该服务工具。", name, category)
			warnings = append(warnings, warning)
			log.Printf("[assembly] %s", warning)
		}
		cleanup := func() {
			for _, closer := range closers {
				_ = closer.Close()
			}
		}
		serverTools := map[string][]string{} // server name → its tool names
		var allNames, globalNames []string
		globalSet := map[string]bool{}
		{
			mcpIDs, err := pg.AgentVisible(a.ID, "mcp")
			if err != nil {
				return []actool.CoreTool{}, agent.DeferredInfo{AssemblyError: fmt.Errorf("读取 MCP 授权失败，未装配工具")}, cleanup
			}
			want := idSet(mcpIDs)
			all, err := pg.ListMCP()
			if err != nil {
				return []actool.CoreTool{}, agent.DeferredInfo{AssemblyError: fmt.Errorf("读取 MCP 目录失败，未装配工具")}, cleanup
			}
			for _, m := range all {
				if !m.Enabled {
					continue
				}
				directVisible := want[m.ID]
				skillGated := gated[m.Name] && (!separateSkillRole || directVisible || baseSkillMCPs[m.Name])
				if !directVisible && !skillGated {
					continue // neither directly visible nor referenced by a visible skill
				}
				var ts []actool.CoreTool
				scopedBrowser := taskID > 0 && taskDial != nil && m.Name == browserMCPName
				if m.Transport == "stdio" || scopedBrowser {
					// Upgrade name-only legacy caches once, then assemble from schemas.
					metadataCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
					metadata, err := cacheMCPMetadata(metadataCtx, pg, m, false)
					cancel()
					if !mcpSchemasReady(metadata) {
						warnUnavailable(m.Name, "工具元数据未就绪")
						continue
					}
					if err != nil {
						warning := fmt.Sprintf("MCP %q 发现暂不可用，本轮使用已缓存的有效工具元数据。", m.Name)
						warnings = append(warnings, warning)
						log.Printf("[assembly] %s", warning)
					}
					session := newLazyMCPSession(ctx, m)
					attachTaskMCPDialer(session, taskID, taskDial)
					closers = append(closers, session)
					for _, t := range metadata {
						ts = append(ts, session.tool(t))
					}
				} else {
					// Remote SSE/HTTP connections retain their existing lifecycle.
					cl, err := connectMCP(ctx, m)
					if err != nil {
						warnUnavailable(m.Name, "连接未就绪")
						continue
					}
					ts, err = cl.Tools(ctx)
					if err != nil {
						_ = cl.Close()
						warnUnavailable(m.Name, "工具发现未就绪")
						continue
					}
					closers = append(closers, cl)
				}
				for _, t := range ts {
					// Existing explicit MCP grants remain authoritative, including
					// screenshot tools. The reporter screenshot setting controls its
					// optional report workflow, not other roles' saved browser grants.
					t = meterTool(t, pg, t.Name(), a.Key, agent.RunInfoFrom(ctx))
					extra = append(extra, t)
					allNames = append(allNames, t.Name())
					serverTools[m.Name] = append(serverTools[m.Name], t.Name())
					if directVisible && !skillGated {
						// directly visible and not gated → available from session start
						globalNames = append(globalNames, t.Name())
						globalSet[t.Name()] = true
					}
					// skill-gated tools stay out of globalNames; unlocked via unlockSkill()
				}
			}
		}

		// Shared call-gate: global MCP tools are unlocked from the start; skill-gated
		// ones are unlocked when their skill loads (or replayed from history — C2).
		unlock := actool.NewUnlockSet(globalNames...)
		unlockSkill := func(skillName string) {
			if reg == nil {
				return
			}
			if s, ok := reg.Get(skillName); ok {
				for _, srv := range s.MCPs {
					unlock.Add(serverTools[srv]...)
				}
			}
		}

		// Skill meta-tool: on load, unlock the skill's MCPs and reveal their names
		// (the ones not already in the global block).
		if reg != nil {
			reg.OnInvoke = func(s skill.Skill) string {
				unlockSkill(s.Name)
				var reveal []string
				for _, srv := range s.MCPs {
					for _, n := range serverTools[srv] {
						if !globalSet[n] {
							reveal = append(reveal, n)
						}
					}
				}
				return actool.RenderDeferredToolsBlock(reveal)
			}
			// Attribution for the usage ledger: ToolAugment only gets (ctx, agentKey),
			// so the run's task/session ids ride in on the ctx (agent.RunInfo). Read it
			// once here — this closure is rebuilt per run, so the captured value always
			// belongs to this run.
			extra = append(extra, meterSkillTool(reg.Tool(), pg, reg, a.Key, agent.RunInfoFrom(ctx)))
		}

		// host tools (traffic / orchestration / custom) — added to every agent's base;
		// ToolResolve then keeps them only for agents the tool is bound to. Custom
		// tools flagged deferred contribute their names to the deferred wiring so
		// their schema is withheld (SearchExtraTools/ExecuteExtraTool), same as MCP.
		if hostTools != nil {
			ht, deferredBinds := hostTools()
			extra = append(extra, ht...)
			for name, boundAgents := range deferredBinds {
				if !contains(boundAgents, a.Key) {
					continue // only defer names this agent is actually bound to
				}
				allNames = append(allNames, name)       // schema withheld from the prompt
				globalNames = append(globalNames, name) // advertised in the deferred block
				globalSet[name] = true
				if unlock != nil {
					unlock.Add(name) // global deferred → callable from the start
				}
			}
		}

		def := agent.DeferredInfo{
			SkillDirs:        skillDirs,
			AssemblyWarnings: warnings,
			Deferred:         allNames,
			GlobalNames:      globalNames,
			Unlock:           unlock,
			UnlockSkill:      unlockSkill,
		}
		return extra, def, cleanup
	}
}

func idSet(ids []int64) map[int64]bool {
	m := make(map[int64]bool, len(ids))
	for _, id := range ids {
		m[id] = true
	}
	return m
}

// seedPrompts writes each built-in agent's code-default prompt body into
// agent_prompts on startup — first-insert only (SeedPromptIfEmpty is a no-op once
// any version exists), so the DB becomes the authoritative editable source while
// user edits survive restarts. Runs after seedBuiltins has created the agent rows.
func seedPrompts(pg *db.DB) {
	for key, tmpl := range agent.BuiltinPromptSeeds() {
		if key == "asset_mapper" {
			// Its first role/prompt/read-tool defaults are one atomic creation.
			// An existing same-key role, including an empty custom prompt, stays
			// untouched on restart or upgrade.
			continue
		}
		a, err := pg.GetAgentByKey(key)
		if err != nil || a == nil {
			log.Printf("[prompts] seed %s 跳过: agent 不存在 (%v)", key, err)
			continue
		}
		if err := pg.SeedPromptIfEmpty(a.ID, tmpl); err != nil {
			log.Printf("[prompts] seed %s 失败: %v", key, err)
		}
	}
}

// wireTools seeds the built-in tool catalog (idempotent, first-insert only so page
// edits survive restart) and wires the DB tools table into the agent runtime: at
// tool-assembly time each built-in tool is filtered by its agent binding / enabled
// flag and, if kept, wrapped so the model sees the DB-overridden description/schema
// and缺省入参 get injected. MCP/skill/host tools have no row and pass through.
func wireTools(pg *db.DB, domainReg map[string]actool.CoreTool) {
	agent.FindingTrafficBindingEnabled = func() bool { return pg.GetBool(settingAgentTrafficBinding, false) }
	agent.IntelligenceEnabled = func() bool { return pg.GetBool(db.SettingTaskIntelligence, false) }
	agent.IntelligenceRecord = func(ctx context.Context, taskID, nodeID int64, worker string, input db.IntelligenceInput) (int64, error) {
		if !pg.GetBool(db.SettingTaskIntelligence, false) {
			return 0, fmt.Errorf("任务情报自动记录已关闭")
		}
		item, err := pg.RecordIntelligence(ctx, taskID, nodeID, worker, input)
		if err != nil {
			return 0, err
		}
		return item.ID, nil
	}
	// Seed the built-in domain tools (first-insert only; DO NOTHING preserves edits).
	// No startup prune: rows we didn't seed are left alone so future user-defined
	// custom tools (system=false, added via the UI) survive restarts.
	seeds := agent.BuiltinToolSeeds()
	var mapperReaders []db.Tool
	for _, s := range seeds {
		if !agent.IsAssetMapperToolAllowed(s.Key) {
			continue
		}
		schema, err := json.Marshal(s.Schema)
		if err != nil {
			log.Printf("[tools] 资产梳理默认只读工具元数据无效: %v", err)
			continue
		}
		mapperReaders = append(mapperReaders, db.Tool{Key: s.Key, System: true, Description: s.Desc, Schema: schema, Agents: s.Agents, Enabled: true, Kind: "builtin"})
	}
	if _, err := pg.SeedAssetMapperIfMissing(agent.BuiltinPromptSeeds()["asset_mapper"], mapperReaders); err != nil {
		log.Printf("[tools] 资产梳理首次初始化失败，未补写已有角色配置: %v", err)
	}
	for _, s := range seeds {
		schema, _ := json.Marshal(s.Schema)
		// Only the atomic first-creation helper may grant mapper defaults.
		// Otherwise a missing tools row could re-grant an existing role that
		// deliberately has no selected tools, even though its helper is a no-op.
		defaults := make([]string, 0, len(s.Agents))
		for _, key := range s.Agents {
			if key != "asset_mapper" {
				defaults = append(defaults, key)
			}
		}
		agents, _ := json.Marshal(defaults)
		if err := pg.SeedTool(s.Key, s.Desc, schema, agents); err != nil {
			log.Printf("[tools] seed %s 失败: %v", s.Key, err)
		}
	}
	// Seed the traffic host tools so they're bindable per-agent like built-ins.
	// Default binding = worker (preserves prior behavior). Their runtime availability
	// is still gated by the global capture switch (hostTools() returns them only when
	// capture is on), so an off-capture binding simply never surfaces the tool.
	trafficAgents, _ := json.Marshal([]string{"worker"})
	for _, t := range traffic.SeedToolMetas() {
		schema, _ := json.Marshal(t.InputSchema())
		if err := pg.SeedTool(t.Name(), t.Description(), schema, trafficAgents); err != nil {
			log.Printf("[tools] seed %s 失败: %v", t.Name(), err)
		}
	}
	// bashInteractiveShellNote is appended to Bash's description ONLY for agents whose
	// interactive_shell is on, so Bash points at shell_open for interactive programs
	// without ever referencing a tool that isn't injected (§14.1/§14.2).
	const bashInteractiveShellNote = "\n\n需要【交互输入】的程序（msfconsole / ssh 交互登录 / mysql、psql、python 等 REPL / 密码或 yes/no 提示 / nc 反弹 shell）不要用 Bash（它没有 stdin、会卡住），改用 shell_open 开交互会话（用完 shell_close）。一次性、非交互命令仍用 Bash。"
	agent.ToolResolve = func(ctx context.Context, agentKey string, tools []actool.CoreTool) []actool.CoreTool {
		if agentKey == "worker.intranet" {
			return agent.ToolAssemblyFailure(fmt.Errorf("内网提示词变体不支持独立执行"))
		}
		if agentKey == db.CredentialAgentKey {
			return []actool.CoreTool{}
		}
		if agentKey == "asset_mapper" {
			a, err := pg.GetAgentByKey(agentKey)
			if err != nil || a == nil || !a.Enabled {
				return agent.ToolAssemblyFailure(fmt.Errorf("资产梳理角色不可用，未装配工具"))
			}
		}
		rows, err := pg.ListTools()
		if err != nil {
			log.Printf("[tools] 读取工具表失败，本轮工具装配已停止")
			return agent.ToolAssemblyFailure(fmt.Errorf("读取工具授权目录失败，未装配工具"))
		}
		byKey := make(map[string]*db.Tool, len(rows))
		for _, t := range rows {
			byKey[t.Key] = t
		}
		runInfo := agent.RunInfoFrom(ctx)
		resolve := func(t actool.CoreTool, row *db.Tool) actool.CoreTool {
			var schema map[string]any
			if len(row.Schema) > 0 {
				_ = json.Unmarshal(row.Schema, &schema)
			}
			return meterTool(agent.DecorateTool(t, row.Description, schema), pg, row.Key, agentKey, runInfo)
		}
		if agentKey == "asset_mapper" {
			// The mapper is a read-only database view, not an execution role.
			// Saved bindings may narrow the two readers, but cannot turn a
			// shell/custom/host tool into a supported mapper capability.
			out := []actool.CoreTool{}
			for _, row := range rows {
				if !row.Enabled || !contains(row.Agents, agentKey) {
					continue
				}
				if !row.System || row.Kind != "builtin" || !agent.IsAssetMapperToolAllowed(row.Key) {
					return agent.ToolAssemblyFailure(fmt.Errorf("资产梳理存在不支持的工具绑定，未装配工具"))
				}
				reader := domainReg[row.Key]
				if reader == nil {
					return agent.ToolAssemblyFailure(fmt.Errorf("资产梳理只读工具不可用，未装配工具"))
				}
				out = append(out, resolve(reader, row))
			}
			return out
		}
		out := tools[:0:0]
		for _, t := range tools {
			if agentKey == "goals" && !agent.IsGoalsToolAllowed(t.Name()) {
				continue
			}
			row, known := byKey[t.Name()]
			if !known { // MCP/skill/host tool: no row → untouched
				out = append(out, t)
				continue
			}
			if !row.Enabled || !contains(row.Agents, agentKey) {
				continue // disabled globally or not bound to this agent → drop
			}
			out = append(out, resolve(t, row))
		}
		// inject: domain tools bound to this agent in the DB but absent from the
		// incoming list. Covers agents (Auto, custom) whose base only has DefaultTools
		// and therefore never includes ToolSet-backed domain tools. Per-task instances
		// in the base always win: inList is built from the original incoming list so a
		// worker's own upsert_asset is never shadowed by the server-level registry copy.
		if agentKey != "goals" && len(domainReg) > 0 {
			inList := make(map[string]bool, len(tools))
			for _, t := range tools {
				inList[t.Name()] = true
			}
			for _, row := range rows {
				if row.Kind == "shell" || !row.Enabled || !contains(row.Agents, agentKey) || inList[row.Key] {
					continue
				}
				inst, ok := domainReg[row.Key]
				if !ok {
					continue // not a domain tool; custom/host tools are injected via hostTools()
				}
				out = append(out, resolve(inst, row))
			}
		}
		// shell hints: user-defined kind="shell" tools are not callable — they are
		// environment declarations that tell the model which command-line tools are
		// installed. Collect the ones bound to this agent and append to Bash's description.
		var shellHints []string
		for _, row := range rows {
			if row.Kind == "shell" && row.Enabled && contains(row.Agents, agentKey) {
				shellHints = append(shellHints, "- "+row.Key+": "+row.Description)
			}
		}
		if len(shellHints) > 0 {
			note := "\n\n以下工具已安装在此 bash 环境中，可直接通过 Bash 调用：\n" + strings.Join(shellHints, "\n")
			for i, t := range out {
				if t.Name() == "Bash" {
					out[i] = agent.DecorateTool(t, t.Description()+note, t.InputSchema())
					break
				}
			}
		}
		// interactive shell: gated purely by the agent's interactive_shell flag (like
		// web_search), NOT by tools-table binding. When on, inject the 5 shell_* tools
		// and COUPLE the Bash description addendum so it points at shell_open — and never
		// dangles when off. See docs/交互式shell设计.md §14.2.
		if agentKey != "goals" && !actool.InteractiveShellDisabled() {
			a, err := pg.GetAgentByKey(agentKey)
			if err != nil || a == nil || !a.Enabled {
				return agent.ToolAssemblyFailure(fmt.Errorf("读取执行角色配置失败，未装配工具"))
			}
			if a.InteractiveShell {
				out = append(out, actool.ShellSessionTools()...)
				for i, t := range out {
					if t.Name() == "Bash" {
						out[i] = agent.DecorateTool(t, t.Description()+bashInteractiveShellNote, t.InputSchema())
						break
					}
				}
			}
		}
		// 批 5 B4:只有 worker 的 BashEnv 注入了 ARTEX_UA(见 worker.go uaEnv),
		// UA 纪律说明也只绑 worker 的 Bash 描述,其他 agent 无此变量、不 dangling。
		if agentKey == "worker" {
			for i, t := range out {
				if t.Name() == "Bash" {
					out[i] = agent.DecorateTool(t, t.Description()+bashUANote, t.InputSchema())
					break
				}
			}
		}
		return out
	}
}

// bashUANote 是批 5 B4 追加进 worker Bash 描述的 UA 使用纪律(与
// bashInteractiveShellNote 同一套代码固定追加法,DB 描述覆盖不掉)。
const bashUANote = "\n\nHTTP 客户端(curl/httpx 等)统一使用 $ARTEX_UA 作为 User-Agent(本任务稳定分配,任务内不要更换);禁止裸 Chrome UA+库 TLS 之外的组合伪装(UA/TLS 指纹失配正是反 AI 蜜罐的触发器);疑似反 AI 陷阱的目标改用 browser MCP 访问。"

func contains(ss []string, v string) bool {
	for _, s := range ss {
		if s == v {
			return true
		}
	}
	return false
}

// buildDomainReg builds a name→CoreTool registry from a server-level ToolSet
// (real AssetStore, nil ExplorationStore, taskID=0). Used by ToolResolve to inject
// domain tools into agents (Auto, custom) that don't own a per-task ToolSet.
// nil as → returns nil (no injection, graceful degradation).
//
// The nil ExplorationStore is deliberate — these instances are task-less by
// construction — so every tool here must tolerate it. Asset/company tools do
// (they only need the AssetStore); the exploration-graph tools refuse with a
// clear message via ToolSet.needExploration. Binding one of them to a task-less
// agent in the tools table is therefore a useless tool, not a crash.
func buildDomainReg(as *db.AssetStore) map[string]actool.CoreTool {
	if as == nil {
		return nil
	}
	serverTS := agent.NewToolSet(nil, "")
	serverTS.SetAssetStore(as, as.Companies())
	reg := make(map[string]actool.CoreTool)
	for _, t := range serverTS.AllDomainTools() {
		reg[t.Name()] = t
	}
	return reg
}

func jsonStrSlice(raw json.RawMessage) []string {
	var out []string
	if len(raw) > 0 {
		_ = json.Unmarshal(raw, &out)
	}
	return out
}

func jsonStrMap(raw json.RawMessage) map[string]string {
	out := map[string]string{}
	if len(raw) > 0 {
		_ = json.Unmarshal(raw, &out)
	}
	return out
}
