package server

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/Autumn-27/artex/db"
)

type TaskCompletionDTO struct {
	AttemptID        int64                   `json:"attempt_id"`
	Phase            string                  `json:"phase"`
	Reason           string                  `json:"reason"`
	StartedAt        time.Time               `json:"started_at"`
	DeadlineAt       time.Time               `json:"deadline_at"`
	UpdatedAt        time.Time               `json:"updated_at"`
	Review           string                  `json:"review,omitempty"`
	RemainingWork    []db.CompletionWork     `json:"remaining_work,omitempty"`
	ReviewDeadlineAt *time.Time              `json:"review_deadline_at,omitempty"`
	FailureKind      string                  `json:"failure_kind,omitempty"`
	FailureCount     int                     `json:"failure_count,omitempty"`
	NextRetryAt      *time.Time              `json:"next_retry_at,omitempty"`
	RecoveryAction   string                  `json:"recovery_action,omitempty"`
	RecoveryHistory  []db.CompletionRecovery `json:"recovery_history,omitempty"`
	GoalGaps         []db.CompletionGoalGap  `json:"goal_gaps,omitempty"`
}

func completionDTO(c *db.TaskCompletion) *TaskCompletionDTO {
	if c == nil {
		return nil
	}
	out := &TaskCompletionDTO{AttemptID: c.AttemptID, Phase: c.Phase, Reason: c.Reason,
		StartedAt: c.StartedAt, DeadlineAt: c.DeadlineAt, UpdatedAt: c.UpdatedAt,
		ReviewDeadlineAt: c.ReviewDeadlineAt, FailureKind: c.FailureKind, FailureCount: c.FailureCount, NextRetryAt: c.NextRetryAt, RecoveryAction: c.RecoveryAction}
	var snapshot db.CompletionInput
	if json.Unmarshal(c.Snapshot, &snapshot) == nil {
		out.Review, out.RemainingWork = snapshot.Review, snapshot.Work
		out.RecoveryHistory = snapshot.RecoveryHistory
		out.GoalGaps = snapshot.GoalGaps
	}
	return out
}

func completionReport(c *db.TaskCompletion) string {
	out := completionDTO(c)
	if out == nil {
		return ""
	}
	var text strings.Builder
	fmt.Fprintf(&text, "\n\n## 任务收官记录\n\n- 阶段：%s\n- 原因：%s\n", out.Phase, out.Reason)
	if out.FailureKind != "" {
		fmt.Fprintf(&text, "- 复核失败：%s（%d 次）；恢复动作：%s\n", out.FailureKind, out.FailureCount, out.RecoveryAction)
	}
	for _, recovery := range out.RecoveryHistory {
		fmt.Fprintf(&text, "- 恢复记录：%s，%s，%s（原失败 %s/%d 次）\n", recovery.At.Format(time.RFC3339), recovery.Actor, recovery.Reason, recovery.FailureKind, recovery.FailureCount)
	}
	if out.Review != "" {
		fmt.Fprintf(&text, "\n%s\n", out.Review)
	}
	for _, gap := range out.GoalGaps {
		fmt.Fprintf(&text, "- 目标 #%d 证据缺口（版本 %d）：%s\n", gap.GoalID, gap.ExpectedVersion, gap.Reason)
	}
	if len(out.RemainingWork) > 0 {
		text.WriteString("\n### 收官时剩余检查\n\n以下停止决定不代表检查验证通过。\n\n")
		for _, work := range out.RemainingWork {
			fmt.Fprintf(&text, "- #%d %s（原状态 %s）：%s\n", work.IntentID, work.Summary, work.State, work.Reason)
		}
	}
	return text.String()
}
