package agent

import (
	"context"
	"encoding/json"
	"fmt"
	actool "github.com/Autumn-27/norma/tool"
	"testing"
)

func TestOperatorTaskChatCapabilitiesAreTurnLocal(t *testing.T) {
	calls := 0
	ri := RunInfo{TaskID: 12, ExplorationID: 13, SessionID: "conv-9"}
	allowed := true
	scope := &taskChatScope{owner: ri, role: "pentest", validate: func(context.Context) error {
		if !allowed {
			return fmt.Errorf("task paused")
		}
		return nil
	}}
	tool := &taskChatTool{scope: scope, CoreTool: actool.Build(actool.Spec{Name: "fixture", Run: func(context.Context, json.RawMessage, *actool.ToolContext) (actool.Result, error) {
		calls++
		return actool.Text("fixture"), nil
	}})}
	ctx := context.WithValue(WithRunInfo(context.Background(), ri), taskChatScopeKey{}, scope)
	r, err := tool.Call(ctx, json.RawMessage(`{}`), nil)
	if err != nil || r.IsError || calls != 1 {
		t.Fatal(err, r, calls)
	}
	other := context.WithValue(WithRunInfo(context.Background(), ri), taskChatScopeKey{}, &taskChatScope{owner: ri})
	r, _ = tool.Call(other, json.RawMessage(`{}`), nil)
	if !r.IsError || calls != 1 {
		t.Fatal("old capability crossed turn")
	}
	allowed = false
	r, _ = tool.Call(ctx, json.RawMessage(`{}`), nil)
	if !r.IsError || calls != 1 {
		t.Fatal("paused task still executed")
	}
}
