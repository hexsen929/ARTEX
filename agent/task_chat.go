package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/Autumn-27/artex/db"
	actool "github.com/Autumn-27/norma/tool"
	"time"
)

type taskChatScopeKey struct{}
type taskChatScope struct {
	owner    RunInfo
	role     string
	tools    []actool.CoreTool
	validate func(context.Context) error
}
type taskChatTool struct {
	actool.CoreTool
	scope *taskChatScope
}

func (t *taskChatTool) Call(ctx context.Context, b json.RawMessage, tc *actool.ToolContext) (actool.Result, error) {
	scope, _ := ctx.Value(taskChatScopeKey{}).(*taskChatScope)
	if scope != t.scope || RunInfoFrom(ctx) != scope.owner || ctx.Err() != nil {
		return actool.Errorf("task conversation capability expired"), nil
	}
	if err := scope.validate(ctx); err != nil {
		return actool.Errorf(err.Error()), nil
	}
	return t.CoreTool.Call(ctx, b, tc)
}

// ChatInTask is a distinct, explicitly admitted entry point, not an automatic
// upgrade of Chat's parent attribution. Every tool has a per-turn capability
// token and revalidates the persisted task before accessing its graph.
func (c *ChatAgent) ChatInTask(ctx context.Context, agentKey, sessionID, message string, maxTurns int, maxDuration time.Duration, webSearch bool, emit func(db.Activity), as *db.AssetStore, store *db.ExplorationStore, recorder FindingRecorder, validate func(context.Context) error) (string, error) {
	ri := RunInfoFrom(ctx)
	if ri.TaskID <= 0 || store == nil || store.ID() != ri.ExplorationID || validate == nil || ExplanationOnly(ctx) || agentKey == "asset_mapper" || agentKey == db.CredentialAgentKey || agentKey == "goals" || agentKey == "worker.intranet" {
		return "", fmt.Errorf("explicit, matching task conversation capability required")
	}
	if err := validate(ctx); err != nil {
		return "", err
	}
	ts := NewToolSet(store, agentKey)
	ts.SetTaskID(ri.TaskID)
	ts.SetFindingRecorder(recorder)
	if as != nil {
		ts.SetAssetStore(as, as.Companies())
		ts.SetCoverageEnabled(as.CoverageEnabled(ri.TaskID))
	}
	if id, err := store.OriginFactID(); err == nil {
		ts.SetOwnerNode(id)
	}
	scope := &taskChatScope{role: agentKey, validate: validate}
	for _, tool := range ts.DropCoverageTools(ts.WorkerTools()) {
		scope.tools = append(scope.tools, &taskChatTool{CoreTool: tool, scope: scope})
	}
	ctx = context.WithValue(ctx, taskChatScopeKey{}, scope)
	return c.chat(ctx, agentKey, sessionID, message, maxTurns, maxDuration, webSearch, emit)
}
