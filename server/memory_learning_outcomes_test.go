package server

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/Autumn-27/artex/db"
)

func TestMemoryLearningOutcomeMessagesDoNotClaimInvalidOutputIsEmpty(t *testing.T) {
	settings := db.MemorySettings{Enabled: true, AutoExtract: true, MaxJobsPerHour: 3}
	for _, tc := range []struct{ result, want string }{
		{"saved", "曾保存"}, {"empty", "有效空结果"}, {"duplicate", "重复"},
		{"no_experience", "历史未细分"}, {"legacy_unknown", "历史未细分"},
		{"input_invalid", "来源投影"}, {"input_sensitive", "敏感"},
		{"output_invalid", "格式不合格"}, {"output_truncated", "截断"},
		{"output_rejected", "完整适用条件"}, {"output_sensitive", "敏感"},
		{"provider_error", "请求失败"}, {"timeout", "超时"}, {"storage_error", "存储"},
		{"lease_expired", "租约过期"}, {"budget_deferred", "本轮未发送"},
		{"source_changed", "来源、设置变化"}, {"settings_changed", "来源、设置变化"},
		{"cleared", "清空"}, {"failed", "失败"}, {"skipped", "未采纳"},
	} {
		t.Run(tc.result, func(t *testing.T) {
			snapshot := db.MemoryLearningSnapshot{LastJob: &db.MemoryLearningLastJob{ID: 1, Result: tc.result, UpdatedAt: time.Now()}}
			view := memoryLearningStatus(settings, snapshot, true)
			if !strings.Contains(view.Message, tc.want) {
				t.Fatalf("result %q message %q does not describe %q", tc.result, view.Message, tc.want)
			}
			if tc.result != "empty" && strings.Contains(view.Message, "返回了有效空结果") {
				t.Fatal("non-empty outcome was misrepresented as a valid empty completion")
			}
		})
	}
}

func TestMemoryLearningSnapshotSafeCountersAndRollingBudget(t *testing.T) {
	stamp := time.Date(2026, 11, 3, 14, 18, 49, 0, time.UTC)
	snapshot := db.MemoryLearningSnapshot{
		Queued: 4, Failed: 27, RetryableFailed: 1, StoppedFailed: 26, Done: 181, EntryCount: 0,
		StartsLastHour: 3, AttemptsLastHour: 3, BudgetLimit: 3, NextBudgetAt: &stamp,
		OutcomeCounts: map[string]int64{"no_experience": 180, "saved": 1, "private-upstream-body-fixture": 2},
		LastJob:       &db.MemoryLearningLastJob{ID: 1, Result: "private-upstream-body-fixture", UpdatedAt: stamp},
	}
	view := memoryLearningStatus(db.MemorySettings{Enabled: true, AutoExtract: true, MaxJobsPerHour: 9}, snapshot, true)
	if view.Status != "budget_wait" || view.MaxCallsPerHour != 3 || view.NextBudgetAt == nil || !view.NextBudgetAt.Equal(stamp) {
		t.Fatalf("rolling budget observation changed: %+v", view)
	}
	if view.RetryableFailed != 1 || view.StoppedFailed != 26 || view.EntryCount != 0 || view.Processed != 181 {
		t.Fatal("retained job totals were confused with current undeleted memory entries")
	}
	if view.OutcomeCounts["legacy_unknown"] != 2 || view.LastResult != "legacy_unknown" {
		t.Fatal("unknown persisted diagnostic was not normalized to a safe category")
	}
	wire, err := json.Marshal(view)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(wire), "private-upstream-body-fixture") {
		t.Fatal("status endpoint returned arbitrary diagnostic text")
	}
	if view.OutcomeCounts["no_experience"] != 180 || view.OutcomeCounts["saved"] != 1 {
		t.Fatal("legacy outcomes were rewritten instead of explained")
	}
}
