package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"testing"
	"time"

	"github.com/Autumn-27/artex/agent"
	"github.com/Autumn-27/artex/db"
	"github.com/Autumn-27/artex/intercept"
)

func TestReportConversationPanelResolvesWithoutExecutingOrResuming(t *testing.T) {
	s, f, request := trafficEvidenceServer(t)
	finding, err := s.m.pg.GetFinding(f.FindingID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.m.pg.Exec(`UPDATE tasks SET paused=true WHERE id=$1`, *finding.TaskID); err != nil {
		t.Fatal(err)
	}
	path := fmt.Sprintf("/api/exploration/findings/%d/report-conversation", f.FindingID)
	var first struct {
		ConversationID int64 `json:"conversation_id"`
		TaskID         int64 `json:"task_id"`
		FindingID      int64 `json:"finding_id"`
	}
	for i := 0; i < 2; i++ {
		response := request(http.MethodPost, path, `{}`)
		if response.Code != http.StatusOK {
			t.Fatal(response.Code, response.Body)
		}
		var got struct {
			ConversationID int64 `json:"conversation_id"`
			TaskID         int64 `json:"task_id"`
			FindingID      int64 `json:"finding_id"`
		}
		if err = json.Unmarshal(response.Body.Bytes(), &got); err != nil {
			t.Fatal(err)
		}
		if got.ConversationID <= 0 || got.TaskID != *finding.TaskID || got.FindingID != f.FindingID {
			t.Fatal("wrong host binding", got)
		}
		if i == 0 {
			first = got
			t.Cleanup(func() { _ = s.m.pg.DeleteConversation(got.ConversationID) })
		} else if first.ConversationID != got.ConversationID {
			t.Fatal("opening again lost conversation")
		}
	}
	var executions int
	if err = s.m.pg.QueryRow(`SELECT count(*) FROM conversation_activities WHERE conversation_id=$1 AND kind IN ('user','text','tool_use','tool_result','result')`, first.ConversationID).Scan(&executions); err != nil || executions != 0 {
		t.Fatal("opening a panel executed a turn", executions, err)
	}
	parent, err := s.m.pg.GetTask(*finding.TaskID)
	if err != nil || !parent.Paused {
		t.Fatal("opening a panel resumed task", parent, err)
	}
	if len(s.triggerQ) != 0 || len(s.chatBusy) != 0 {
		t.Fatal("panel queued execution")
	}
	if response := request(http.MethodGet, path, ""); response.Code != http.StatusMethodNotAllowed {
		t.Fatal("read-only GET created conversation", response.Code)
	}
}

func reportPauseFixture(t *testing.T) (*findingCheckPauseFixture, *db.Conversation) {
	t.Helper()
	f := newFindingCheckPauseFixture(t)
	pg := f.s.m.pg
	a, err := pg.GetAgentByKey("reporter")
	if err != nil {
		t.Fatal(err)
	}
	created := a == nil
	if created {
		a, err = pg.CreateAgent("reporter", "report fixture", "fixture")
		if err != nil {
			t.Fatal(err)
		}
	}
	oldEnabled, oldProfile := a.Enabled, a.LLMProfileID
	if _, err = pg.Exec(`UPDATE agents SET enabled=true,llm_profile_id=NULL WHERE id=$1`, a.ID); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if created {
			_, _ = pg.Exec(`DELETE FROM agents WHERE id=$1`, a.ID)
		} else {
			_, _ = pg.Exec(`UPDATE agents SET enabled=$2,llm_profile_id=$3 WHERE id=$1`, a.ID, oldEnabled, oldProfile)
		}
	})
	taskID, _ := strconv.ParseInt(f.task.ID, 10, 64)
	if _, err = pg.Exec(`UPDATE tasks SET paused=false WHERE id=$1`, taskID); err != nil {
		t.Fatal(err)
	}
	c, _, err := pg.EnsureFindingReportConversation(t.Context(), f.finding)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = pg.DeleteConversation(c.ID) })
	return f, c
}

func TestReportConversationCarriesTaskScopeAndStopsWithParentPause(t *testing.T) {
	f, c := reportPauseFixture(t)
	key := f.s.convBusyKey(c.ID)
	f.s.chatMu.Lock()
	f.s.chatBusy[key] = true
	f.s.chatMu.Unlock()
	f.s.runConversation(c, "adjust the existing report", key)
	callCtx := f.waitProvider(t)
	taskID, _ := strconv.ParseInt(f.task.ID, 10, 64)
	parent, err := f.s.m.pg.GetTask(taskID)
	if err != nil {
		t.Fatal(err)
	}
	if info := agent.RunInfoFrom(callCtx); info.TaskID != taskID || info.ExplorationID != parent.ExplorationID {
		t.Fatal("reporter lost source task context", info)
	}
	if intercept.ConvIDFromContext(callCtx) != c.ID {
		t.Fatal("reporter lost actual conversation")
	}
	if err = f.s.checkFindingReportConversationScope(callCtx, f.finding); err != nil {
		t.Fatal(err)
	}
	if err = f.s.checkFindingReportConversationScope(callCtx, f.finding+1000000); err == nil {
		t.Fatal("focused report conversation can write a foreign finding")
	}
	f.pauseCommitted(t)
	select {
	case <-callCtx.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("parent pause did not stop reporter")
	}
	waitRetestIdle(t, f.s)
	if _, ok := f.s.findingCheckTasks.Load(c.ID); ok {
		t.Fatal("finished reporter retained live task attribution")
	}
	calls := f.calls.Load()
	f.s.chatMu.Lock()
	f.s.chatBusy[key] = true
	f.s.chatMu.Unlock()
	f.s.runConversation(c, "must not run while parent remains paused", key)
	waitRetestIdle(t, f.s)
	if f.calls.Load() != calls {
		t.Fatal("paused parent admitted a new reporter model call")
	}
	// The panel itself remains available and reuses the same exact association.
	if resolved, _, err := f.s.m.pg.EnsureFindingReportConversation(context.Background(), f.finding); err != nil || resolved.ID != c.ID {
		t.Fatal("paused task cannot view its report panel", resolved, err)
	}
}

func TestReportConversationRejectedEvidenceDoesNotRebindFromTool(t *testing.T) {
	s, f, _ := trafficEvidenceServer(t)
	current, _, err := s.m.pg.EnsureFindingReportConversation(t.Context(), f.FindingID)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.m.pg.DeleteConversation(current.ID) })
	other, err := s.m.pg.CreateConversation("reporter", "unaccepted-report-fixture", nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.m.pg.DeleteConversation(other.ID) })
	finding, err := s.m.pg.GetFinding(f.FindingID)
	if err != nil {
		t.Fatal(err)
	}
	ctx := intercept.WithConvID(t.Context(), other.ID)
	workflowCall(t, ctx, s.toolUpdateFindingReport(), map[string]any{"finding_id": f.NodeID, "report": "stale update", "evidence_version": finding.EvidenceVersion + 1}, true)
	after, err := s.m.pg.GetFinding(f.FindingID)
	if err != nil || after.Report != finding.Report || after.ReportConversationID == nil || *after.ReportConversationID != current.ID {
		t.Fatal("rejected tool changed report or exact link", after, err)
	}
}
