//go:build ignore

package server

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"testing"

	"github.com/Autumn-27/artex/db"
)

func TestGoalCorrectionHTTPKeepsEvidenceAndRejectsStaleVersion(t *testing.T) {
	s, _, request := trafficEvidenceServer(t)
	task, err := s.m.CreateTask("goal correction", "obtain the flag", nil, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	tid, _ := strconv.ParseInt(task.ID, 10, 64)
	t.Cleanup(func() { s.engine.StopTask(task.ID); _ = s.m.pg.DeleteTask(tid) })
	g, err := task.Store.AddGoal(map[string]any{"text": "obtain the flag"}, "human")
	if err != nil {
		t.Fatal(err)
	}
	e, err := task.Store.AddNode(db.KindFact, map[string]any{"summary": "no flag obtained"}, 0, "confirmed", "worker", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err = task.Store.Link(e, db.RelProves, g); err != nil {
		t.Fatal(err)
	}
	if err = task.Store.SetNodeState(g, "met"); err != nil {
		t.Fatal(err)
	}
	n, err := task.Store.GetNode(g)
	if err != nil {
		t.Fatal(err)
	}
	path := fmt.Sprintf("/api/tasks/%s/goals/%d/reopen", task.ID, g)
	if w := request("POST", path, `{ "reason":"missing flag" }`); w.Code != 400 {
		t.Fatalf("missing version accepted: %d %s", w.Code, w.Body)
	}
	if w := request("POST", path, fmt.Sprintf(`{"expected_version":%d,"reason":"missing flag"}`, n.ContentVersion+1)); w.Code != 409 {
		t.Fatalf("stale correction accepted: %d %s", w.Code, w.Body)
	}
	w := request("POST", path, fmt.Sprintf(`{"expected_version":%d,"reason":"negative evidence cannot establish flag outcome"}`, n.ContentVersion))
	if w.Code != 200 {
		t.Fatalf("correction failed: %d %s", w.Code, w.Body)
	}
	var out GoalDTO
	if json.Unmarshal(w.Body.Bytes(), &out) != nil || out.State != "open" || len(out.Audit) != 1 || out.Audit[0].Event != "reopen" || len(out.Audit[0].PreviousProofs) != 1 {
		t.Fatalf("correction lost audit: %s", w.Body)
	}
	if evidence, err := task.Store.GetNode(e); err != nil || evidence == nil || !strings.Contains(string(evidence.Payload), "no flag obtained") {
		t.Fatal("correction destroyed original evidence")
	}
	if w := request("POST", path, fmt.Sprintf(`{"expected_version":%d,"reason":"duplicate correction"}`, n.ContentVersion)); w.Code != 409 {
		t.Fatalf("old version applied twice: %d %s", w.Code, w.Body)
	}
}
