//go:build ignore

package subagent

import (
	"context"
	"errors"
	"testing"

	"github.com/Autumn-27/norma/harness"
	"github.com/Autumn-27/norma/permission"
	"github.com/Autumn-27/norma/tool"
)

func TestProgrammaticSubagentHonorsInheritedExecutionGuard(t *testing.T) {
	ctx := tool.WithExecutionGuard(context.Background(), func(name string) error {
		if name == "Agent" {
			return errors.New("no new work during checkpoint")
		}
		return nil
	})
	_, err := Run(ctx, Config{
		Provider: fakeProvider{reply: "must not run"},
		Agents:   map[string]Definition{"child": {PermissionMode: permission.ModeBypass}},
	}, "child", "go", "")
	if err == nil {
		t.Fatal("programmatic subagent bypassed the parent execution guard")
	}
}

func TestSubagentInheritsRequestedSettlementController(t *testing.T) {
	controller := harness.NewSettlementController()
	controller.Request()
	// A nil provider would panic if the child tried to make a model call. It has
	// no checkpoint prompt, so a requested controller must stop it at entry.
	text, err := Run(harness.WithSettlementController(context.Background(), controller), Config{
		Agents: map[string]Definition{"child": {PermissionMode: permission.ModeBypass}},
	}, "child", "go", "")
	if err != nil || text != "" {
		t.Fatalf("text=%q err=%v", text, err)
	}
}
