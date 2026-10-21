package server

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
)

type ssToolCaller interface {
	Call(context.Context, string, any) (string, error)
}

func ssAssetQuery(ssType, dimension, target string) map[string]any {
	// The bundled ScopeSentry contract supports task search for all asset types,
	// but app's filter supports only project/tags (skills/scopesentry §4.3–4.4).
	if ssType == "app" && dimension == "task" {
		return map[string]any{"search": "task==" + strconv.Quote(target)}
	}
	return map[string]any{"filter": map[string]any{dimension: []string{target}}}
}

// ssPageAll preserves preceding pages on failure and probes beyond an exactly
// full cap before claiming truncation. Page-number traversal is not a source
// snapshot guarantee: concurrent source changes can still move rows.
func (s *Server) ssPageAll(ctx context.Context, cl ssToolCaller, ssType string, query map[string]any, pageSize int) (items []json.RawMessage, truncated bool, err error) {
	if pageSize <= 0 || pageSize > 500 {
		pageSize = syncDefaultPage
	}
	for page := 1; ; page++ {
		if err := ctx.Err(); err != nil {
			return items, false, err
		}
		args := map[string]any{"asset_type": ssType, "pageIndex": page, "pageSize": pageSize}
		for key, value := range query {
			args[key] = value
		}
		text, err := cl.Call(ctx, "list_assets", args)
		if err != nil {
			return items, false, fmt.Errorf("第 %d 页: %w", page, err)
		}
		var envelope map[string]json.RawMessage
		if err := json.Unmarshal([]byte(text), &envelope); err != nil {
			return items, false, fmt.Errorf("第 %d 页解析失败: %w", page, err)
		}
		raw, exists := envelope["list"]
		if !exists {
			return items, false, fmt.Errorf("第 %d 页响应缺少有效 list 数组", page)
		}
		if string(raw) == "null" {
			// ScopeSentry 1.9.4 emits null for an empty asset/subdomain page.
			// Confirm the boundary independently, so an upstream error cannot
			// silently masquerade as a successful empty source.
			total, countErr := ssCountAssets(ctx, cl, ssType, query)
			if countErr != nil || total > (page-1)*pageSize {
				return items, false, fmt.Errorf("第 %d 页 list 为空但无法核实分页终点 (total=%d): %v", page, total, countErr)
			}
			return items, false, nil
		}
		var batch []json.RawMessage
		if err := json.Unmarshal(raw, &batch); err != nil {
			return items, false, fmt.Errorf("第 %d 页 list 解析失败: %w", page, err)
		}
		remaining := syncMaxPerType - len(items)
		if len(batch) > remaining {
			items = append(items, batch[:remaining]...)
			return items, true, nil
		}
		items = append(items, batch...)
		if len(batch) < pageSize {
			return items, false, nil
		}
	}
}

func ssCountAssets(ctx context.Context, cl ssToolCaller, kind string, query map[string]any) (int, error) {
	args := map[string]any{"asset_type": kind}
	for key, value := range query {
		args[key] = value
	}
	text, err := cl.Call(ctx, "count_assets", args)
	if err != nil {
		return 0, err
	}
	var response struct {
		Total *int `json:"total"`
	}
	if err := json.Unmarshal([]byte(text), &response); err != nil {
		return 0, err
	}
	if response.Total == nil || *response.Total < 0 {
		return 0, fmt.Errorf("count_assets 缺少有效 total")
	}
	return *response.Total, nil
}
