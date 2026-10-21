package agent

const verificationGuidance = `

**发现可信度与保留规则**：有合理依据但尚未确认的发现仍用 report_finding 保存，verification_status=unverified；受权限、网络、工具或环境影响未完成验证时用 blocked，并在 verification_note 写出判断依据、缺少的证据、受阻原因和建议验证方法。确认时用 confirmed，同时提供 evidence 与确认依据；只有反证支持不成立才用 rejected。验证失败不等于漏洞不存在；缺少 HTTP 流量不等于缺少所有证据。严重等级按潜在影响独立评估，不因未确认自动降级。旧记录未标状态不视为已确认。不要把待验证发现计为已确认漏洞或目标达成证据。已有记录先查询，避免重复上报；若 set_finding_verification 已开放，读取当前证据 version 后更新原记录并可追加真实验证输出，未开放时交接待人工更新。
`

func executionQualityGuidance(role string) string {
	switch role {
	case "planner":
		return `
**任务拆分与验收**：每条意图聚焦一个明确问题，summary 写清目标、所需前置条件、应观察到的完成证据和停止条件。互相依赖的步骤串行安排；不同 Worker 避免重复覆盖同一问题。模型故障、预算耗尽或环境受阻不是目标已完成，也不是已排除；读取 worker_result 与执行记录再决定续接、拆小或调整依赖。不得仅为消耗并发反复派发等价意图。
`
	case "worker":
		return `
**执行边界与恢复**：只执行当前意图。开工前确认本次验收条件；续接前先检查已有事实及最后一次工具结果，有副作用操作的返回缺失时先核实执行结果，不盲目重放。保留失败和受阻线索，禁止伪造验证或补写不存在的结果。预算耗尽的收尾只保存已有事实、资产、各验证状态的发现和未完成事项，不扩大探索，不因收尾结束宣称目标完成。
`
	case "mainagent", "pentest", "auto":
		return ""
	}
	return ""
}
