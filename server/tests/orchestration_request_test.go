//go:build ignore

package server

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/Autumn-27/artex/db"
	"github.com/Autumn-27/artex/intercept"
)

func TestSpawnTaskIgnoresModelSuppliedFlagAuthorization(t *testing.T) {
	for _, tc := range []struct{ name, source, message string }{
		{"ordinary human", intercept.BackgroundUserMessage, "全面测试普通应用"},
		{"no human", "", ""},
		{"worker summary", "worker_summary", "获取flag"},
		{"explicit denial", intercept.BackgroundUserMessage, "不要获取flag"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := intercept.WithReviewContext(t.Context(), "", intercept.ReviewBackground{Source: tc.source, Text: tc.message})
			// No Manager: rejection must happen before any persistent side effect.
			s := &Server{}
			result, err := s.toolSpawnTask().Call(ctx, json.RawMessage(`{"description":"app test","goal":"尽力获取漏洞与flag","user_request":"用户要求获取flag"}`), nil)
			if err != nil || !result.IsError || !strings.Contains(result.Flatten(), "用户未明确要求") {
				t.Fatalf("result=%s err=%v", result.Flatten(), err)
			}
		})
	}
}

func TestSpawnTaskAcceptsExplicitCTFAndOrdinaryFlags(t *testing.T) {
	for _, tc := range []struct{ name, goal, message string }{
		{"explicit flag", "获取flag", "尝试获取flag"},
		{"explicit CTF", "获取flag", "这是CTF靶场任务"},
		{"ordinary", "检查应用登录", "测试应用"},
		{"CLI", "检查命令行 flag 参数", "检查配置"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := intercept.WithReviewContext(t.Context(), "", intercept.ReviewBackground{Source: intercept.BackgroundUserMessage, Text: tc.message})
			input, _ := json.Marshal(map[string]any{"description": "fixture", "goal": tc.goal, "source_task_ids": make([]string, db.MaxTaskSourceCount+1)})
			// A later, independent validation is the sentinel: the request boundary
			// permits these goals without starting a background task during this test.
			result, err := (&Server{}).toolSpawnTask().Call(ctx, input, nil)
			if err != nil || !result.IsError || !strings.Contains(result.Flatten(), "关联任务最多选择") {
				t.Fatalf("result=%s err=%v", result.Flatten(), err)
			}
		})
	}
}
