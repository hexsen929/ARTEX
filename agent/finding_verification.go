package agent

import (
	"context"
	"encoding/json"
	"github.com/Autumn-27/artex/db"
	actool "github.com/Autumn-27/norma/tool"
)

func (t *ToolSet) verifyFinding() actool.CoreTool {
	return t.writeExpTool("set_finding_verification", "更新本任务已有漏洞的验证状态，不重复创建。finding_id 使用独立记录ID；继承记录只读。severity/处理状态保持不变。确认需已有或本次补充证据及依据；受阻/排除需原因；版本冲突后重新读取，不盲目重放。", obj(map[string]any{
		"finding_id": idp("独立 findings 记录ID，不是探索节点ID"), "status": map[string]any{"type": "string", "enum": []string{"confirmed", "unverified", "blocked", "rejected"}},
		"note": str("确认或排除依据，或缺少证据、受阻原因与下一步"), "version": map[string]any{"type": "integer", "description": "从 list_findings 的 evidence_version、get_finding_traffic 或漏洞详情读取的当前证据版本"}, "additional_evidence": str("可选，追加已实际取得的验证输出；不会删除原证据"),
	}, "finding_id", "status", "note", "version"), func(ctx context.Context, in json.RawMessage) (actool.Result, error) {
		var a struct {
			FindingID          json.RawMessage `json:"finding_id"`
			Status             string          `json:"status"`
			Note               string          `json:"note"`
			Version            *int64          `json:"version"`
			AdditionalEvidence string          `json:"additional_evidence"`
		}
		if json.Unmarshal(in, &a) != nil || a.Version == nil || !db.ValidVerificationStatus(a.Status) || a.Status == db.VerificationLegacy {
			return actool.Errorf("请提供 finding_id、合法 status、note 与当前 version"), nil
		}
		f, err := t.ts.VerifyFinding(ctx, pid(a.FindingID), a.Status, a.Note, *a.Version, a.AdditionalEvidence)
		if err != nil {
			return actool.Errorf(err.Error()), nil
		}
		return jsonResult(map[string]any{"finding_id": f.ID, "verification_status": f.VerificationStatus, "verification_note": f.VerificationNote, "evidence_version": f.EvidenceVersion})
	})
}
