package server

import (
	"context"
	"encoding/json"
	"log"
	"strings"

	"github.com/Autumn-27/artex/agent"
	actool "github.com/Autumn-27/norma/tool"
)

func (s *Server) toolUpdateFindingEvidence() actool.CoreTool {
	return wrTool("update_finding_evidence", "补充已有发现的完整 evidence 和来源，整段覆盖并保留旧证据审计。finding_id 使用独立漏洞记录 ID，先读 get_finding_record/get_finding_traffic 取得最新 evidence_version。来源 fact 必须已保存；引用或流量存在不能替代机制成立的证明。补证使旧审核和报告失效，不重复登记发现。",
		objSchema(map[string]any{
			"finding_id":       strParam("独立漏洞记录 ID，不是探索节点 ID"),
			"evidence_version": map[string]any{"type": "integer", "minimum": 0},
			"evidence":         strParam("完整实际操作、请求/响应关键内容、机制条件、观察结果和限制；不要只写哈希或已有记录编号"),
			"reason":           strParam("补充或纠正的原因"),
			"source_fact_ids":  map[string]any{"type": "array", "maxItems": 20, "items": map[string]any{"type": "integer"}},
		}, "finding_id", "evidence_version", "evidence", "reason"),
		func(ctx context.Context, in json.RawMessage) (actool.Result, error) {
			var a struct {
				FindingID json.RawMessage `json:"finding_id"`
				Version   *int64          `json:"evidence_version"`
				Evidence  string          `json:"evidence"`
				Reason    string          `json:"reason"`
				Facts     []int64         `json:"source_fact_ids"`
			}
			if err := json.Unmarshal(in, &a); err != nil || a.Version == nil || strings.TrimSpace(a.Reason) == "" {
				return actool.Errorf("finding_id、evidence_version、完整 evidence 和 reason 必填"), nil
			}
			id := parseProfileID(a.FindingID)
			if err := s.agentFindingTrafficAccess(ctx, id, true); err != nil {
				return actool.Errorf(err.Error()), nil
			}
			if err := s.checkCaseReviewScope(ctx, "update_finding_evidence", findingCaseRequest{FindingID: a.FindingID}); err != nil {
				return actool.Errorf(err.Error()), nil
			}
			ri := agent.RunInfoFrom(ctx)
			actor := ri.SessionID
			if actor == "" {
				actor = "agent"
			}
			version, err := s.m.pg.UpdateFindingEvidence(ctx, id, *a.Version, ri.TaskID, a.Evidence, actor, a.Reason, a.Facts)
			if err != nil {
				return actool.Errorf(err.Error()), nil
			}
			return jsonResult(map[string]any{"finding_id": i64s(id), "evidence_version": version, "note": "证据已更新，旧审核与报告需重新复核"})
		})
}

func (s *Server) seedEvidenceHandoffBindings() {
	const flag = "check_evidence_handoff_v1"
	if value, _, _ := s.m.pg.GetSetting(flag); value == "true" {
		return
	}
	for _, role := range []string{"worker", "planner", "reporter"} {
		if err := s.m.pg.AddAgentToToolBinding(role, []string{"get_finding_record", "get_finding_traffic", "bind_finding_traffic", "update_finding_evidence", "update_finding_report"}); err != nil {
			log.Printf("[evidence] handoff binding: %v", err)
			return
		}
	}
	_ = s.m.pg.SetSetting(flag, "true")
}
