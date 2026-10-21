package server

import (
	"context"
	"encoding/json"
	"log"

	actool "github.com/Autumn-27/norma/tool"

	"github.com/Autumn-27/artex/agent"
	"github.com/Autumn-27/artex/db"
)

type toolUsageRecorder interface {
	InsertToolUsage(*db.ToolUsage) error
}

// meteredTool records one row immediately before a catalog tool is invoked. It
// embeds the resolved tool so schema overrides, defaults and permission behavior
// remain unchanged.
type meteredTool struct {
	actool.CoreTool
	recorder toolUsageRecorder
	toolKey  string
	agentKey string
	ri       agent.RunInfo
}

func meterTool(t actool.CoreTool, recorder toolUsageRecorder, toolKey, agentKey string, ri agent.RunInfo) actool.CoreTool {
	if recorder == nil {
		return t
	}
	return &meteredTool{CoreTool: t, recorder: recorder, toolKey: toolKey, agentKey: agentKey, ri: ri}
}

func (m *meteredTool) Call(ctx context.Context, input json.RawMessage, tc *actool.ToolContext) (result actool.Result, callErr error) {
	row := &db.ToolUsage{
		ToolKey:       m.toolKey,
		AgentKey:      m.agentKey,
		TaskID:        m.ri.TaskID,
		ExplorationID: m.ri.ExplorationID,
		IntentID:      m.ri.IntentID,
		SessionID:     m.ri.SessionID,
	}
	if err := m.recorder.InsertToolUsage(row); err != nil {
		log.Printf("[toolusage] insert %s: %v", m.toolKey, err)
	}
	defer func() {
		if recorder, ok := m.recorder.(interface{ FinishToolUsage(int64, string) error }); ok && row.ID > 0 {
			outcome := "success"
			if callErr != nil || result.IsError {
				outcome = "error"
			}
			if recovered := recover(); recovered != nil {
				_ = recorder.FinishToolUsage(row.ID, "error")
				panic(recovered)
			}
			if err := recorder.FinishToolUsage(row.ID, outcome); err != nil {
				log.Printf("[toolusage] finish %s: %v", m.toolKey, err)
			}
		}
	}()
	return m.CoreTool.Call(ctx, input, tc)
}
