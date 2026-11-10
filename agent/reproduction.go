package agent

func ReproductionSchema() map[string]any {
	return map[string]any{"type": "object", "description": "可选：执行时记录的真实复现过程，不重新探测或编造请求。最多 32 步。traffic_ids 只引用本次已绑定的真实流量，无抓包可省略并保留命令与输出。",
		"properties": map[string]any{
			"entry_point": str("实际入口"), "provenance": str("依据与来源"),
			"prerequisites": map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "maxItems": 16},
			"steps":         map[string]any{"type": "array", "minItems": 1, "maxItems": 32, "items": obj(map[string]any{"title": str("动作说明"), "request": str("真实请求或命令"), "response": str("真实响应或输出，可保留关键片段并注明截断"), "expected": str("预期现象"), "proves": str("实际证明内容及局限"), "traffic_ids": map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "maxItems": 8}}, "title")},
		}, "required": []string{"steps"}}
}
