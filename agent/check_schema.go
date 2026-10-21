package agent

import (
	"encoding/json"
	actool "github.com/Autumn-27/norma/tool"
)

// Merge additive contract fields after catalog overrides, without replacing the
// operator's descriptions/defaults or re-enabling disabled tools.
func withCheckContractSchema(t actool.CoreTool) actool.CoreTool {
	var additions map[string]any
	batch := ""
	switch t.Name() {
	case "set_goals":
		additions = map[string]any{"goal_type": map[string]any{"type": "string", "enum": []string{"inspection", "outcome"}}, "required_checks": map[string]any{"type": "array", "items": str("必要对象、机制及完成条件")}}
		batch = "goals"
	case "add_intent":
		additions = map[string]any{"check": obj(map[string]any{"target": str("规范化目标/接口"), "mechanism": str("固定检查机制")}, "target", "mechanism"), "retry_of": idp("恢复的原意图"), "recovery_change": str("实际改变什么")}
		batch = "intents"
	case "record_fact":
		additions = map[string]any{"checkpoint": checkpointSchema()}
		batch = "facts"
	default:
		return t
	}
	raw, err := json.Marshal(t.InputSchema())
	if err != nil {
		return t
	}
	var schema map[string]any
	if json.Unmarshal(raw, &schema) != nil {
		return t
	}
	properties, _ := schema["properties"].(map[string]any)
	if properties == nil {
		properties = map[string]any{}
		schema["properties"] = properties
	}
	for k, v := range additions {
		properties[k] = v
	}
	if field, ok := properties[batch].(map[string]any); ok {
		itemSchema, _ := field["items"].(map[string]any)
		if itemSchema == nil {
			itemSchema = map[string]any{"type": "object"}
			field["items"] = itemSchema
		}
		items, _ := itemSchema["properties"].(map[string]any)
		if items == nil {
			items = map[string]any{}
			itemSchema["properties"] = items
		}
		for k, v := range properties {
			if k != batch && items[k] == nil {
				items[k] = v
			}
		}
		for k, v := range additions {
			items[k] = v
		}
		if t.Name() == "set_goals" {
			raw, _ := json.Marshal(itemSchema["required"])
			var required []string
			_ = json.Unmarshal(raw, &required)
			found := false
			for _, name := range required {
				found = found || name == "goal_type"
			}
			if !found {
				required = append(required, "goal_type")
			}
			itemSchema["required"] = required
		}
	}
	note := "\n按固定目标/机制保存检查身份；恢复引用最新 retry_of 并说明 recovery_change。"
	if t.Name() == "set_goals" {
		note = "\n保存固定 goal_type 和 required_checks；inspection 可由完整阴性检查满足，outcome 必须有成果证据。"
	} else if t.Name() == "record_fact" {
		note = "\ncheckpoint 必须如实记录覆盖、结果和缺口，续接保留原认证候选、预算和已验证身份。"
	}
	return DecorateTool(t, t.Description()+note, schema)
}

func checkpointSchema() map[string]any {
	stringsSchema := map[string]any{"type": "array", "items": map[string]any{"type": "string"}}
	return obj(map[string]any{
		"result": map[string]any{"type": "string", "enum": []string{"positive", "negative", "partial", "blocked"}},
		"scope":  stringsSchema, "evidence": str("实际操作及可核查结果"), "gaps": stringsSchema,
		"barrier": str("具体阻碍"), "next_action": str("恢复要改变什么"),
		"auth": obj(map[string]any{
			"request": str("真实接口、method、Content-Type 和字段名；不含凭据"),
			"budget":  intp("授权的总候选尝试次数"),
			"candidates": map[string]any{"type": "array", "items": obj(map[string]any{
				"id": str("固定候选引用，不含密码"), "state": map[string]any{"type": "string", "enum": []string{"remaining", "failed", "skipped", "verified"}}, "reason": str("跳过/停止原因"), "attempts": intp("实际发送请求次数，包括错误/限流中断；续接不得减少"),
			}, "id", "state")},
			"identity": str("已验证身份和权限"), "identity_evidence": str("认证后低影响读取及关键响应"), "stop_reason": str("限流/锁定等停止原因"),
		}, "request", "budget", "candidates"),
	}, "result", "scope", "evidence", "gaps")
}
