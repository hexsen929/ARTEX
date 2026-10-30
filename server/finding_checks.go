package server

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/Autumn-27/artex/agent"
	"github.com/Autumn-27/artex/agent/chainskel"
	"github.com/Autumn-27/artex/db"
	"github.com/Autumn-27/artex/intercept"
	actool "github.com/Autumn-27/norma/tool"
)

// settingFindingVerifyMode 是验证管线的 per-phase 开关:auto(默认,外网阶段
// 自动验证)/ off(内网阶段默认关闭——内网误报多为推理错误、重放价值低,且重放
// =再触发一次利用动作,撞 EDR/审计/锁账号的代价质变,见 FIXPLAN-AND-EVALUATION.md
// 2026-09-13 分阶段验证策略)。本期只做单一开关;按任务阶段自动切换是后续任务。
// off 时自动派发跳过(水位照常前进,不回放),手动单条验证(POST checks)不受影响。
const settingFindingVerifyMode = "finding_verify_mode"

// findingVerifyAutoEnabled accepts only a known explicit automatic mode. The
// database reader supplies "auto" for an absent setting; malformed stored values
// must not silently enable networked automatic work.
func findingVerifyAutoEnabled(mode string) bool {
	return mode == "auto"
}

// Automatic work requires both switches: the global mode and the editable
// verifier's on_finding trigger. Merged evidence uses the same admission rule as
// a newly discovered finding. A missing/deleted trigger is deliberately not
// repaired here; doing so could re-enable work the operator explicitly stopped.
func (s *Server) automaticFindingVerificationEnabled() (bool, error) {
	if s.m == nil || s.m.pg == nil {
		return false, errors.New("db 未初始化")
	}
	ctx := s.ctx
	if ctx == nil {
		ctx = context.Background()
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	var mode string
	var agentEnabled, triggerEnabled bool
	err := s.m.pg.QueryRowContext(ctx, `SELECT
COALESCE((SELECT value FROM settings WHERE key=$1),'auto'),
EXISTS(SELECT 1 FROM agents WHERE key=$2 AND enabled),
EXISTS(SELECT 1 FROM agent_triggers WHERE agent_key=$2 AND enabled AND on_finding)`,
		settingFindingVerifyMode, db.FindingVerifierAgentKey).Scan(&mode, &agentEnabled, &triggerEnabled)
	if err != nil {
		return false, err
	}
	if mode != "auto" && mode != "off" {
		return false, errors.New("自动复核模式设置无效，未派发新自动任务；请显式保存 auto 或 off")
	}
	return findingVerifyAutoEnabled(mode) && agentEnabled && triggerEnabled, nil
}

func (s *Server) listFindingChecks(w http.ResponseWriter, r *http.Request) {
	pg := s.pg(w)
	if pg == nil {
		return
	}
	id, ok := pathInt(r, "id")
	if !ok || id <= 0 {
		writeErr(w, 400, "bad finding id")
		return
	}
	f, err := pg.GetFinding(id)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	if f == nil {
		writeErr(w, 404, "finding not found")
		return
	}
	items, err := pg.ListFindingChecks(id)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"checks": items})
}

// startFindingCheck 手动发起一次验证(内网阶段的手动单条出口;W3 合并后的增量
// 重验也走同一入口 RequestFindingCheck)。不受 finding_verify_mode 限制。
func (s *Server) startFindingCheck(w http.ResponseWriter, r *http.Request) {
	pg := s.pg(w)
	if pg == nil {
		return
	}
	id, ok := pathInt(r, "id")
	if !ok || id <= 0 {
		writeErr(w, 400, "bad finding id")
		return
	}
	var req struct {
		Kind string `json:"kind"`
	}
	if !decodeConversationRequest(w, r, &req) {
		return
	}
	kind := strings.TrimSpace(req.Kind)
	if kind == "" {
		kind = db.FindingCheckKindVerify
	}
	if kind != db.FindingCheckKindVerify && kind != db.FindingCheckKindRetest {
		writeErr(w, 400, "kind 必须为 verify / retest")
		return
	}
	f, err := pg.GetFinding(id)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	if f == nil {
		writeErr(w, 404, "finding not found")
		return
	}
	check, created, err := s.RequestFindingCheck(id, kind)
	if errors.Is(err, sql.ErrNoRows) {
		writeErr(w, 404, "finding not found")
		return
	}
	if err != nil {
		writeErr(w, 409, err.Error())
		return
	}
	code := http.StatusOK
	if created {
		code = http.StatusAccepted
	}
	writeJSON(w, code, map[string]any{"check": check, "created": created})
}

// RequestFindingCheck 是 finding_checks 管线的可复用入口(C2 预留):调度器的
// 自动派发、上面的手动 HTTP 入口、后续合并(W3)的增量重验都调它。先建
// finding_checks 行(pending,含快照+会话)再跑会话;同一 finding 已有活跃
// check 时去重返回(created=false)。
func (s *Server) RequestFindingCheck(findingID int64, kind string) (*db.FindingCheck, bool, error) {
	s.concMu.Lock()
	defer s.concMu.Unlock()
	return s.requestFindingCheck(findingID, kind)
}

// Automatic validation belongs to the parent task's execution lifecycle. Both
// checking eligibility and synchronously registering the new run's cancellation
// happen under the same lock as persistent pause, so no automatic check slips
// into the gap between a pause commit and cancellation. Explicit manual checks
// use RequestFindingCheck, which serializes creation but permits paused tasks.
func (s *Server) requestAutomaticFindingCheck(findingID int64, kind string) (*db.FindingCheck, bool, error) {
	s.concMu.Lock()
	defer s.concMu.Unlock()
	if s.m == nil || s.m.pg == nil {
		return nil, false, errors.New("db 未初始化")
	}
	enabled, err := s.automaticFindingVerificationEnabled()
	if err != nil || !enabled {
		return nil, false, err
	}
	finding, err := s.m.pg.GetFinding(findingID)
	if err != nil {
		return nil, false, err
	}
	if finding == nil || finding.TaskID == nil || s.engine == nil {
		return nil, false, nil
	}
	taskID := strconv.FormatInt(*finding.TaskID, 10)
	task, ok := s.m.Task(taskID)
	if !ok || s.engine.IsDeleting(taskID) || s.engine.IsPaused(taskID) || !s.engine.Started(taskID) {
		return nil, false, nil
	}
	lifecycle := task.lifecycleSnapshot()
	if lifecycle.Paused || lifecycle.Queued || lifecycle.Status != "running" || isTerminalStatus(lifecycle.Status) {
		return nil, false, nil
	}
	return s.requestFindingCheck(findingID, kind)
}

// Caller holds concMu. Keep the shared creation path free of lifecycle locks to
// avoid recursive locking from the manual and automatic admission methods.
func (s *Server) requestFindingCheck(findingID int64, kind string) (*db.FindingCheck, bool, error) {
	if s.m == nil || s.m.pg == nil {
		return nil, false, errors.New("db 未初始化")
	}
	finding, err := s.m.pg.GetFinding(findingID)
	if err != nil {
		return nil, false, err
	}
	if finding == nil {
		return nil, false, sql.ErrNoRows
	}
	if finding.TaskID != nil {
		done, err := s.beginIntranetTaskOperation(*finding.TaskID)
		if err != nil {
			return nil, false, err
		}
		defer done()
	}
	a, err := s.m.pg.GetAgentByKey(db.FindingVerifierAgentKey)
	if err != nil {
		return nil, false, err
	}
	if a == nil || !a.Enabled {
		return nil, false, errors.New("漏洞验证 Agent 不存在或未启用，请在 Agent 管理中配置 verifier")
	}
	if s.resolveChatAgent(&db.Conversation{AgentKey: a.Key}) == nil {
		return nil, false, errors.New(s.chatUnavailableReason(&db.Conversation{AgentKey: a.Key}))
	}
	if s.ctx.Err() != nil {
		return nil, false, errors.New("服务正在停止")
	}
	check, conv, created, err := s.m.pg.CreateFindingCheck(s.ctx, findingID, kind)
	if err != nil {
		return nil, false, err
	}
	if created {
		busyKey := s.convBusyKey(conv.ID)
		if finding.TaskID != nil {
			s.findingCheckTasks.Store(conv.ID, strconv.FormatInt(*finding.TaskID, 10))
		}
		s.chatMu.Lock()
		s.chatBusy[busyKey] = true
		s.chatMu.Unlock()
		s.runConversation(conv, check.InitialMessage(), busyKey)
	}
	return check, created, nil
}

// onFindingMerged 是档 A 合并后的 C2 增量重验钩子:新证据搭了既有"已验证"
// 状态的便车,须对聚合 finding 重新验证。与 dispatchFindingVerify 同样遵守
// finding_verify_mode、Agent 与 on_finding 触发器开关;活跃 check 去重使重复
// 触发安全。回调在 Record 事务提交后执行,失败仅记日志,不影响上报主流程。
func (s *Server) onFindingMerged(findingID int64) {
	if _, _, err := s.requestAutomaticFindingCheck(findingID, db.FindingCheckKindVerify); err != nil {
		log.Printf("[verifier] 合并增量重验 finding %d 派发失败: %v", findingID, err)
	}
}

// dispatchFindingVerify 是 scheduler 的 on_finding 自动派发钩子:事件只带
// node_id,经 FindingIDByNodeID 反查 finding(节点与 finding 同事务写入,此刻
// 已可见)。自动准入统一检查 mode、Agent、触发器及父任务状态。调度器仍会
// 消费跳过事件的水位，不回放历史；需要复核的记录从明确的手动入口发起。
func (s *Server) dispatchFindingVerify(e db.TaskEvent) {
	findingID, err := s.m.pg.FindingIDByNodeID(e.NodeID)
	if err != nil {
		log.Printf("[verifier] node %d 反查 finding 失败: %v", e.NodeID, err)
		return
	}
	if _, _, err := s.requestAutomaticFindingCheck(findingID, db.FindingCheckKindVerify); err != nil {
		log.Printf("[verifier] 自动验证 finding %d 派发失败: %v", findingID, err)
	}
}

// cancelTaskFindingChecks is called after a pause commit while concMu remains
// held. Cancel known runtime contexts first: a database failure must never let
// network/tool work continue. Sealing affects only check history, not findings.
func (s *Server) cancelTaskFindingChecks(taskID string, cause error) error {
	id, err := strconv.ParseInt(strings.TrimSpace(taskID), 10, 64)
	if err != nil || id <= 0 {
		return errors.New("无效任务编号")
	}
	taskID = strconv.FormatInt(id, 10)
	if cause == nil {
		cause = agent.AbortChatPausedWithTask
	}
	var known []context.CancelCauseFunc
	s.findingCheckTasks.Range(func(key, value any) bool {
		conversationID, validID := key.(int64)
		owner, validOwner := value.(string)
		if !validID || !validOwner || owner != taskID {
			return true
		}
		s.chatMu.Lock()
		var cancel context.CancelCauseFunc
		// Cleanup removes attribution before clearing busy/cancel. Revalidate
		// under chatMu so a stale Range entry cannot cancel a new history turn.
		if current, ok := s.findingCheckTasks.Load(conversationID); ok && current == taskID {
			cancel = s.chatCancel[s.convBusyKey(conversationID)]
		}
		s.chatMu.Unlock()
		if cancel != nil {
			known = append(known, cancel)
		}
		return true
	})
	for _, cancel := range known {
		cancel(cause)
	}
	if s.m == nil || s.m.pg == nil {
		return errors.New("验证已请求停止，但数据库未就绪，状态审计未完成")
	}
	ctx, cancelAudit := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancelAudit()
	rows, err := s.m.pg.QueryContext(ctx, `SELECT c.id,c.conversation_id FROM finding_checks c JOIN findings f ON f.id=c.finding_id WHERE f.task_id=$1 AND c.status IN ('pending','running')`, id)
	if err != nil {
		return err
	}
	conversations := map[int64]int64{}
	for rows.Next() {
		var checkID int64
		var conversation sql.NullInt64
		if err = rows.Scan(&checkID, &conversation); err != nil {
			rows.Close()
			return err
		}
		if conversation.Valid {
			conversations[checkID] = conversation.Int64
		}
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	// Snapshot the cancellation function, not just its key. A later explicit
	// explanation turn on a sealed historical conversation must remain usable.
	cancels := map[int64]context.CancelCauseFunc{}
	s.chatMu.Lock()
	for checkID, conversationID := range conversations {
		if cancel := s.chatCancel[s.convBusyKey(conversationID)]; cancel != nil {
			cancels[checkID] = cancel
		}
	}
	s.chatMu.Unlock()
	sealed := map[int64]bool{}
	defer func() {
		for checkID, cancel := range cancels {
			if err != nil || sealed[checkID] {
				cancel(cause)
			}
		}
	}()
	rows, err = s.m.pg.QueryContext(ctx, `UPDATE finding_checks c SET status='stopped',error='任务已暂停，独立漏洞验证已停止',finished_at=now() FROM findings f WHERE c.finding_id=f.id AND f.task_id=$1 AND c.status IN ('pending','running') RETURNING c.id`, id)
	if err != nil {
		return err
	}
	for rows.Next() {
		var checkID int64
		if err = rows.Scan(&checkID); err != nil {
			rows.Close()
			return err
		}
		sealed[checkID] = true
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	return nil
}

// Both tools derive the finding from server-owned conversation context. Tool
// arguments cannot redirect a result into a different finding or conversation.
func (s *Server) findingCheckTools() []actool.CoreTool {
	return []actool.CoreTool{
		roTool("get_finding_check_context", "读取当前验证会话关联的漏洞快照(finding、受影响资产、原任务约束)与验证状态。无参数，只能读取本会话。",
			objSchema(map[string]any{}), func(ctx context.Context, _ json.RawMessage) (actool.Result, error) {
				c, err := s.m.pg.FindingCheckForConversation(ctx, intercept.ConvIDFromContext(ctx))
				if err != nil {
					return actool.Errorf(err.Error()), nil
				}
				if c == nil {
					return actool.Errorf("当前会话未关联验证记录，请从漏洞详情发起验证"), nil
				}
				var constraints []db.Constraint
				f, err := s.m.pg.GetFinding(c.FindingID)
				if err != nil {
					return actool.Errorf(err.Error()), nil
				}
				if f != nil && f.TaskID != nil {
					if task, ok := s.m.Task(strconv.FormatInt(*f.TaskID, 10)); ok {
						constraints, err = task.Store.ListConstraints()
						if err != nil {
							return actool.Errorf(err.Error()), nil
						}
					}
				}
				// chains L4 → verifier 判定层:按 finding 的 vulnclass 注入反证
				// checklist(通用层永远带,类别命中加专项),让结论逐条过判据。
				vulnclass := ""
				if f != nil {
					vulnclass = f.VulnClass
				}
				return jsonResult(map[string]any{"check": c, "current_constraints": constraints,
					"verifier_checklist": chainskel.VerifierChecklist(vulnclass)})
			}),
		wrTool("record_finding_check_result", "为当前验证会话保存唯一结论(verified 确认真实 / false_positive 误报 / inconclusive 无法确认)；原漏洞证据与报告保持不变。会话成功结束且快照仍有效时，verified 更新独立验证状态为 confirmed，false_positive 更新为 rejected；不改变人工处置状态。inconclusive 保留不确定检查历史，不覆盖现有验证结论。必须提供本次实际检查的证据，受阻时写明原因。",
			objSchema(map[string]any{
				"verdict":  map[string]any{"type": "string", "enum": []string{"verified", "false_positive", "inconclusive"}},
				"summary":  strParam("本次验证结论摘要"),
				"evidence": strParam("Markdown：本次实际步骤、观察、对照、结论依据；无法确认则列出已检查内容和阻塞原因"),
			}, "verdict", "summary", "evidence"), func(ctx context.Context, in json.RawMessage) (actool.Result, error) {
				var a struct {
					Verdict  string `json:"verdict"`
					Summary  string `json:"summary"`
					Evidence string `json:"evidence"`
				}
				if err := json.Unmarshal(in, &a); err != nil {
					return actool.Errorf(err.Error()), nil
				}
				if err := s.m.pg.RecordFindingCheckResult(ctx, intercept.ConvIDFromContext(ctx), a.Verdict, a.Summary, a.Evidence); err != nil {
					return actool.Errorf(err.Error()), nil
				}
				return jsonResult(map[string]any{"saved": true, "verdict": a.Verdict})
			}),
	}
}

// Seed the editable agent atomically, plus its on_finding trigger. Once seeded,
// user edits/deletion survive restarts; a pre-existing key is kept.
func (s *Server) seedFindingVerifier() error {
	for _, t := range s.findingCheckTools() {
		schema, _ := json.Marshal(t.InputSchema())
		bindings, _ := json.Marshal([]string{db.FindingVerifierAgentKey})
		if err := s.m.pg.SeedTool(t.Name(), t.Description(), schema, bindings); err != nil {
			return err
		}
	}
	tx, err := s.m.pg.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err = seedFindingVerifierRows(tx); err != nil {
		return err
	}
	return tx.Commit()
}

// All first-install defaults share one transaction. In particular a failed
// trigger insert must not leave a committed seed flag or half-created Agent.
// Existing Agent/trigger rows and a completed migration remain user-owned.
func seedFindingVerifierRows(tx *sql.Tx) error {
	const flag = "finding_verifier_seed_v1"
	// Lock this migration, including concurrent server initialization.
	if _, err := tx.Exec(`SELECT pg_advisory_xact_lock(7337741011)`); err != nil {
		return err
	}
	var done string
	err := tx.QueryRow(`SELECT value FROM settings WHERE key=$1`, flag).Scan(&done)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if done == "true" {
		return nil
	}
	var id int64
	err = tx.QueryRow(`INSERT INTO agents(key,name,description,role,builtin,enabled)
	VALUES ($1,'漏洞验证','新漏洞上报后自动触发，重放 PoC 反证误报并保存独立验证结论。','assistant',false,true)
	ON CONFLICT (key) DO NOTHING RETURNING id`, db.FindingVerifierAgentKey).Scan(&id)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if id > 0 {
		var pid int64
		if err = tx.QueryRow(`INSERT INTO agent_prompts(agent_id,version,template_text,note,updated_by)
		VALUES ($1,1,$2,'内置默认','system') RETURNING id`, id, agent.VerifierDefaultPrompt).Scan(&pid); err != nil {
			return err
		}
		if _, err = tx.Exec(`UPDATE agents SET current_prompt_id=$1 WHERE id=$2`, pid, id); err != nil {
			return err
		}
		// Only a newly created built-in default receives an automatic trigger.
		// Never recreate one for a pre-existing Agent: absence may be deliberate.
		if _, err = tx.Exec(`INSERT INTO agent_triggers(agent_key,enabled,on_finding)
		VALUES ($1,true,true)`, db.FindingVerifierAgentKey); err != nil {
			return err
		}
	}
	if _, err = tx.Exec(`INSERT INTO settings(key,value) VALUES ($1,'true') ON CONFLICT(key) DO UPDATE SET value='true'`, flag); err != nil {
		return err
	}
	return nil
}

func (s *Server) finishCheck(id int64, status, reason string) {
	if err := s.m.pg.FinishFindingCheck(id, status, reason); err != nil {
		// Surface failure to the conversation runner rather than inventing a result.
		log.Printf("[check %d] finish: %v", id, err)
	}
}
