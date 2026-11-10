package db

import (
	"encoding/json"
	"fmt"
	"slices"
	"strconv"
	"strings"
)

const VerificationHandoffCode = "verification_handoff"

// VerificationFieldFailure contains only validated local identifiers and
// host-owned field names. Never copy model arguments or error prose into it.
type VerificationFieldFailure struct {
	FindingID int64    `json:"finding_id"`
	Fields    []string `json:"missing_fields"`
}

func (f *VerificationFieldFailure) Message() string {
	return fmt.Sprintf("set_finding_verification 未更新记录 #%d：缺少 %s。已停止重复提交，由系统交回 Planner 核对当前记录和已有证据；不需要用户填写内部字段。证据不足时保留待验证状态，不自动确认、不重派原任务。", f.FindingID, strings.Join(f.Fields, ", "))
}

// ParseVerificationFieldFailure accepts only the built-in validation response,
// not substring matches, provider errors, schema failures or unknown fields.
func ParseVerificationFieldFailure(input, result []byte) *VerificationFieldFailure {
	var args struct {
		ID      json.RawMessage `json:"finding_id"`
		Status  string          `json:"status"`
		Version *int64          `json:"version"`
	}
	var failure struct {
		Error  string   `json:"error"`
		Fields []string `json:"missing_fields"`
	}
	if json.Unmarshal(input, &args) != nil || json.Unmarshal(result, &failure) != nil || failure.Error != "verification_missing_fields" || args.Version == nil || *args.Version < 0 || !ValidVerificationStatus(args.Status) || args.Status == VerificationLegacy || len(failure.Fields) == 0 || len(failure.Fields) > 2 {
		return nil
	}
	idText := string(args.ID)
	if len(args.ID) > 0 && args.ID[0] == '"' {
		if json.Unmarshal(args.ID, &idText) != nil {
			return nil
		}
	}
	id, err := strconv.ParseInt(idText, 10, 64)
	if err != nil || id <= 0 {
		return nil
	}
	for _, field := range failure.Fields {
		if field != "note" && field != "additional_evidence" {
			return nil
		}
	}
	slices.Sort(failure.Fields)
	return &VerificationFieldFailure{FindingID: id, Fields: slices.Compact(failure.Fields)}
}

// Norma rejects an unknown deferred name before calling any tool. Match the
// complete local lookup diagnostic, never an arbitrary substring or MCP error.
func IsDeferredLookupRejection(tool string, input, result []byte) bool {
	if tool != "ExecuteExtraTool" {
		return false
	}
	var args struct {
		Name string `json:"tool_name"`
	}
	return json.Unmarshal(input, &args) == nil && args.Name != "" && len(args.Name) <= 512 && !strings.ContainsAny(args.Name, "\r\n") && string(result) == "Error: unknown tool "+args.Name
}
