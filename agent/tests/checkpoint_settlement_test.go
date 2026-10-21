//go:build ignore

package agent

import (
	"context"
	"encoding/json"
	"testing"
	"testing/synctest"
	"time"

	"github.com/Autumn-27/norma/agentcore"
	"github.com/Autumn-27/norma/harness"
	"github.com/Autumn-27/norma/llm"
	"github.com/Autumn-27/norma/permission"
	"github.com/Autumn-27/norma/tool"
)

func TestWorkerSettlementSavesAssetsAfterOld60SecondWindow(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		calls, saved, probes := 0, 0, 0
		provider := completionReviewProvider{stream: func(ctx context.Context, req llm.CompletionRequest, yield func(llm.StreamEvent, error) bool) {
			calls++
			for _, available := range req.Tools {
				if available.Name == "Bash" {
					t.Fatal("settlement exposed a probe tool")
				}
			}
			if calls == 1 {
				time.Sleep(75 * time.Second)
				if ctx.Err() != nil {
					yield(llm.StreamEvent{}, ctx.Err())
					return
				}
				completionReviewEvents(yield, llm.StreamEvent{Type: llm.SEToolUseStart, ToolID: "save", ToolName: "insert_assets"}, llm.StreamEvent{Type: llm.SEToolInputJSON, Text: `{"assets":[{"ip":"127.0.0.1"}]}`}, llm.StreamEvent{Type: llm.SEMessageDelta, StopReason: "tool_use"})
				return
			}
			completionReviewEvents(yield, llm.StreamEvent{Type: llm.SETextDelta, Text: "checkpoint saved; gaps retained"}, llm.StreamEvent{Type: llm.SEMessageDelta, StopReason: "end_turn"})
		}}
		save := tool.Build(tool.Spec{Name: "insert_assets", Run: func(context.Context, json.RawMessage, *tool.ToolContext) (tool.Result, error) {
			saved++
			return tool.Text("saved"), nil
		}})
		probe := tool.Build(tool.Spec{Name: "Bash", Run: func(context.Context, json.RawMessage, *tool.ToolContext) (tool.Result, error) {
			probes++
			return tool.Text("unexpected"), nil
		}})
		control := harness.NewSettlementController()
		control.Request()
		_, err := agentcore.Run(harness.WithSettlementController(t.Context(), control), agentcore.Options{Provider: provider, Tools: []tool.CoreTool{save, probe}, Settlement: workerCheckpointSettlement(false, false), PermissionMode: permission.ModeBypass}, "stop and save obtained assets")
		if err != nil || saved != 1 || probes != 0 {
			t.Fatalf("late checkpoint lost: saved=%d probes=%d calls=%d err=%v", saved, probes, calls, err)
		}
	})
}
