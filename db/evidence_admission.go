package db

import (
	"encoding/json"
	"fmt"
	"strings"
)

// ValidateGoalEvidence prevents explicit incomplete/inferred results from being
// promoted even when a model returns a positive verdict. Semantic sufficiency
// still requires independent review of the saved mechanism and observations.
func ValidateGoalEvidence(snapshot GoalProofSnapshot) error {
	if snapshot.Goal == nil || snapshot.Evidence == nil {
		return fmt.Errorf("缺少目标或证据")
	}
	var goal struct {
		Type    string `json:"goal_type"`
		Pending bool   `json:"goal_contract_pending"`
	}
	var evidence struct {
		Confidence string      `json:"confidence"`
		Checkpoint *Checkpoint `json:"checkpoint"`
	}
	if err := json.Unmarshal(snapshot.Goal.Payload, &goal); err != nil {
		return err
	}
	if goal.Pending {
		return fmt.Errorf("目标拆解尚未确定检查或成果契约，请先明确目标类型及必要检查")
	}
	if err := json.Unmarshal(snapshot.Evidence.Payload, &evidence); err != nil {
		return err
	}
	if evidence.Confidence == "inferred" {
		return fmt.Errorf("推断事实不能独自证明目标达成")
	}
	if c := evidence.Checkpoint; c != nil {
		if err := c.Validate(); err != nil {
			return err
		}
		if c.Result == "partial" || c.Result == "blocked" {
			return fmt.Errorf("检查尚有缺口，不能证明目标已完成")
		}
		if goal.Type == "outcome" && c.Result != "positive" {
			return fmt.Errorf("阴性检查不能满足成果目标")
		}
	}
	var findings []struct {
		Evidence string `json:"evidence"`
		Status   string `json:"status"`
	}
	if len(snapshot.Finding) > 0 {
		if err := json.Unmarshal(snapshot.Finding, &findings); err != nil {
			return err
		}
	}
	for _, f := range findings {
		if strings.TrimSpace(f.Evidence) == "" {
			return fmt.Errorf("发现缺少可核查 evidence；保留线索并补证，不能计为已验证成果")
		}
		if f.Status == FindingFalsePositive || f.Status == FindingIgnored || f.Status == FindingDuplicate {
			return fmt.Errorf("发现当前处置状态不支持独立成果")
		}
	}
	return nil
}
