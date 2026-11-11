package agent

import (
	"encoding/json"
	"strings"

	actool "github.com/Autumn-27/norma/tool"
)

const intentCallContract = "【当前调用契约】新建检查：summary 与 check_identity 必填；续接：仅传 continue_intent_id 和非空 continuation_note，沿用原身份、资产、来源与验收，不传 asset_ids/parent_ids/check_identity。临时模型断流/限流/服务故障由系统检查原记录并退避自动续接，返回wait_system_recovery时结束本轮等待，不要求用户操作；真实审批、鉴权或无法核实的工具结果保留明确原因。模型不得反复调用或新建等价检查绕过。返回 state=blocked 或 queued=false 表示没有入队，不能称为成功派发。"
const verificationCallContract = "【当前验证契约】note 是判断依据，不是证据正文。status=confirmed 要求原记录 evidence 非空，或通过 additional_evidence 追加已实际取得的证据；已绑定数据包不自动填写文本 evidence。缺少真实证据时保持 unverified/blocked，不编造输出、不只改写 note 重试。字段错误由 Agent 读取当前记录和已保存证据处理，不请求用户填写内部字段；记录已被其他流程更新时使用当前状态，不重复提交。"
const trafficReadContract = "【读取边界】traffic_search 返回 items、page、limit、has_more、next_page；has_more=false 时停止翻页。仅按主机宽查询每轮最多12次，之后停止该类翻页，已知记录读取与明确条件的查询仍可在原预算内进行；全部搜索最多40次。持续重复被拒或总预算耗尽时自动整理已读结果并交回Planner，无需用户逐页核对。不得换主机或新建等价意图重置额度。绿勾只说明查询成功，不证明任务有进展。"

// Called only for authorized built-in rows, after persisted metadata overrides.
// Never change stored custom descriptions, bindings, enablement or constraints.
func RuntimeToolContract(tool actool.CoreTool) actool.CoreTool {
	note := ""
	switch tool.Name() {
	case "add_intent":
		note = intentCallContract
	case "set_finding_verification":
		note = verificationCallContract
	case "traffic_search":
		note = trafficReadContract
	default:
		return tool
	}
	raw, err := json.Marshal(tool.InputSchema())
	if err != nil {
		return tool
	}
	var schema map[string]any
	if json.Unmarshal(raw, &schema) != nil {
		return tool
	}
	props, _ := schema["properties"].(map[string]any)
	if props == nil {
		props = map[string]any{}
		schema["properties"] = props
	}
	var defaults map[string]any
	switch tool.Name() {
	case "add_intent":
		defaults = NewToolSet(nil, "").addIntent().InputSchema()
	case "set_finding_verification":
		defaults = NewToolSet(nil, "").verifyFinding().InputSchema()
	}
	if defaults != nil {
		current, _ := defaults["properties"].(map[string]any)
		for key, value := range current {
			if _, exists := props[key]; !exists {
				props[key] = value
			}
		}
		if tool.Name() == "add_intent" {
			batch, _ := props["intents"].(map[string]any)
			if batch != nil {
				item, _ := batch["items"].(map[string]any)
				if item != nil {
					fields, _ := item["properties"].(map[string]any)
					if fields == nil {
						fields = map[string]any{}
						item["properties"] = fields
					}
					for key, value := range current {
						if key != "intents" {
							if _, exists := fields[key]; !exists {
								fields[key] = value
							}
						}
					}
				}
			}
		}
	}
	appendProperty := func(p map[string]any, key, text string) {
		field, ok := p[key].(map[string]any)
		if !ok {
			return
		}
		desc, _ := field["description"].(string)
		if !strings.Contains(desc, text) {
			field["description"] = text + "\n" + desc
		}
	}
	if tool.Name() == "add_intent" {
		appendProperty(props, "intents", intentCallContract)
		appendProperty(props, "summary", "仅新建时填写；续接不需要重复 summary。")
		appendProperty(props, "asset_ids", "仅新建时填写；续接必须省略。")
		if batch, ok := props["intents"].(map[string]any); ok {
			if item, ok := batch["items"].(map[string]any); ok {
				if fields, ok := item["properties"].(map[string]any); ok {
					appendProperty(fields, "asset_ids", "仅新建时填写；续接必须省略。")
					appendProperty(fields, "summary", "仅新建时填写；续接不需要重复 summary。")
				}
			}
		}
	}
	if tool.Name() == "set_finding_verification" {
		appendProperty(props, "additional_evidence", "原记录没有文本 evidence 且请求 confirmed 时必填；只追加真实观测，note不能替代。")
	}
	return DecorateTool(tool, note+"\n\n"+tool.Description(), schema)
}
