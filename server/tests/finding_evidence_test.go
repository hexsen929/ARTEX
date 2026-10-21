//go:build ignore

package server

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"

	"github.com/Autumn-27/artex/agent"
	"github.com/Autumn-27/artex/db"
)

func TestEvidenceHandoffActualRoleAssemblyAndTaskIsolation(t *testing.T) {
	s, initial, _ := trafficEvidenceServer(t)
	previous := s.m.pg.GetBool(settingAgentTrafficBinding, false)
	if err := s.m.pg.SetBool(settingAgentTrafficBinding, true); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.m.pg.SetBool(settingAgentTrafficBinding, previous) })
	f, _ := s.m.pg.GetFinding(initial.FindingID)
	task := s.m.ResolveTask(fmt.Sprint(*f.TaskID))
	ctx := agent.WithRunInfo(context.Background(), agent.RunInfo{TaskID: *f.TaskID, SessionID: "fixture-worker"})
	for _, role := range []string{"worker", "planner"} {
		ts := agent.NewToolSet(task.Store, role)
		base := ts.WorkerTools()
		if role == "planner" {
			base = ts.PlannerTools()
		}
		tools, _, cleanup := agent.AugmentTools(ctx, role, base)
		for _, name := range []string{"get_finding_record", "get_finding_traffic", "bind_finding_traffic", "update_finding_evidence", "update_finding_report"} {
			workflowTool(t, tools, name)
		}
		cleanup()
	}
	input := map[string]any{"finding_id": initial.FindingID, "evidence_version": f.EvidenceVersion, "evidence": "saved fixture response", "reason": "補全遗漏证据"}
	workflowCall(t, ctx, s.toolUpdateFindingEvidence(), input, false)
	workflowCall(t, ctx, s.toolUpdateFindingEvidence(), input, true)
	child, err := s.m.CreateTaskWithOptions("read-only inheritance", "fixture", db.TaskCreateOptions{SourceTaskIDs: []int64{*f.TaskID}})
	if err != nil {
		t.Fatal(err)
	}
	childID := parseProfileID([]byte(child.ID))
	t.Cleanup(func() { s.m.pg.DeleteTask(childID) })
	childCtx := agent.WithRunInfo(context.Background(), agent.RunInfo{TaskID: childID})
	workflowCall(t, childCtx, s.toolUpdateFindingEvidence(), input, true)
	workflowCall(t, childCtx, s.toolUpdateFindingReport(), map[string]any{"finding_id": initial.NodeID, "report": "foreign overwrite"}, true)
	// A later user unbinding must survive repeated startup migration calls.
	row, _ := s.m.pg.GetTool("update_finding_evidence")
	original, _ := json.Marshal(row.Agents)
	t.Cleanup(func() {
		s.m.pg.Exec(`UPDATE tools SET agents=$1::jsonb WHERE key='update_finding_evidence'`, string(original))
	})
	s.m.pg.Exec(`UPDATE tools SET agents=agents-'worker' WHERE key='update_finding_evidence'`)
	s.seedEvidenceHandoffBindings()
	var rebound bool
	if err = s.m.pg.QueryRow(`SELECT agents ? 'worker' FROM tools WHERE key='update_finding_evidence'`).Scan(&rebound); err != nil || rebound {
		t.Fatal("startup undid explicit unbinding", err)
	}
}
