package agent

import (
	"context"
	"encoding/json"

	actool "github.com/Autumn-27/norma/tool"
)

func (t *ToolSet) listRemainingWork() actool.CoreTool {
	return t.readExpTool("list_remaining_work",
		"分页列出本任务所有非 done 意图，包括 open/running/paused/blocked/exhausted/stopped/deleted，返回总数及停止/受阻原因。收官前必须逐页核查：has_more=true 时用 next_after 作为下一次 after，直到 has_more=false。旧意图不会因概览截断而遗漏；必要工作仍须完成，只有重复或无关工作才可提交 remaining_work 停止理由。",
		obj(map[string]any{"after": intp("上一页的 next_after；首页省略或填 0"), "limit": intp("每页数量，默认 100，最大 200")}),
		func(ctx context.Context, in json.RawMessage) (actool.Result, error) {
			var args struct {
				After int64 `json:"after"`
				Limit int   `json:"limit"`
			}
			if err := json.Unmarshal(in, &args); err != nil {
				return actool.Errorf("invalid list_remaining_work arguments: " + err.Error()), nil
			}
			page, err := t.ts.ListRemainingWork(ctx, args.After, args.Limit)
			if err != nil {
				return actool.Errorf("list_remaining_work: " + err.Error()), nil
			}
			return jsonResult(page)
		})
}
