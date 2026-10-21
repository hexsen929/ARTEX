package agent

// taskScopeContext preserves the explicit task boundary even when coverage
// metrics are disabled. Company rules are planning input, not proof of testing.
func (t *ToolSet) taskScopeContext() ([]map[string]any, error) {
	rows, err := t.as.ListTaskScope(t.taskID)
	if err != nil {
		return nil, err
	}
	out := make([]map[string]any, 0, len(rows))
	for _, row := range rows {
		entry := map[string]any{"kind": row.Kind, "source": row.Source, "reason": row.Reason}
		switch {
		case row.Domain != "":
			entry["value"] = row.Domain
		case row.Net != "":
			entry["value"] = row.Net
		case row.Value != "":
			entry["value"] = row.Value
		case row.CompanyID != nil:
			entry["company_id"], entry["company_name"] = *row.CompanyID, row.CompanyName
			rules, err := t.as.Companies().GetScope(*row.CompanyID)
			if err != nil {
				return nil, err
			}
			scope := make([]map[string]any, 0, len(rules))
			keywords := []string{}
			for _, rule := range rules {
				value := rule.Value
				if rule.Domain != "" {
					value = rule.Domain
				} else if rule.Net != "" {
					value = rule.Net
				}
				if rule.Raw != "" {
					value = rule.Raw
				}
				scope = append(scope, map[string]any{"kind": rule.Kind, "value": value})
				if rule.Kind == "keyword" {
					keywords = append(keywords, value)
				}
			}
			entry["company_scope"], entry["company_keywords"] = scope, keywords
		}
		out = append(out, entry)
	}
	return out, nil
}
