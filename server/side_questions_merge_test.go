package server

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/Autumn-27/artex/db"
	"github.com/Autumn-27/artex/sidequestion"
	"github.com/Autumn-27/norma/llm"
)

func TestMergeFindingSideSnapshotKeepsDatabaseParentAndNoTools(t *testing.T) {
	f := newSideHTTPFixture(t)
	pg := f.m.pg
	a, err := pg.GetAgentByKey("auto")
	if err != nil || a == nil {
		t.Fatal("auto fixture unavailable", err)
	}
	profile := &db.LLMProfile{Name: fmt.Sprintf("finding-side-merge-%d", time.Now().UnixNano()), Format: "openai", BaseURL: "http://fixture.invalid/v1", Model: "fixture", APIKey: "fixture-key", Streaming: true}
	profileID, err := pg.SaveProfile(profile)
	if err != nil {
		t.Fatal(err)
	}
	oldProfile := a.LLMProfileID
	if _, err := pg.Exec(`UPDATE agents SET llm_profile_id=$1 WHERE id=$2`, profileID, a.ID); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = pg.Exec(`UPDATE agents SET llm_profile_id=$1 WHERE id=$2`, oldProfile, a.ID)
		_, _ = pg.Exec(`DELETE FROM llm_profiles WHERE id=$1`, profileID)
	})
	row, err := pg.CreateTask("finding-side fixture", "read-only fixture", nil, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	findingID, err := pg.AddFinding(row.ID, 0, "fixture", "finding marker", "low", "fixture", "untrusted fixture evidence", "fixture", nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = pg.Exec(`DELETE FROM side_question_sessions WHERE task_id=$1`, row.ID)
		_, _ = pg.DeleteFinding(findingID)
		_, _ = pg.Exec(`DELETE FROM tasks WHERE id=$1`, row.ID)
		_, _ = pg.Exec(`DELETE FROM explorations WHERE id=$1`, row.ExplorationID)
	})
	parent := sidequestion.Parent{TaskID: row.ID, ExplorationID: row.ExplorationID}
	snap, err := f.s.buildFindingAutoSnapshot(t.Context(), parent, findingID)
	if err != nil || snap == nil {
		t.Fatal("finding snapshot missing", err)
	}
	if snap.Parent != parent || snap.Parent.ConversationID != 0 || snap.Model.ProfileID != profileID {
		t.Fatalf("snapshot parent or selected Auto model changed: %+v", snap.Parent)
	}
	if len(snap.Request.Tools) != 0 || len(snap.Request.Messages) != 1 || !strings.Contains(snap.Request.Messages[0].Text(), "finding marker") {
		t.Fatal("finding consultation gained tools or lost its bounded evidence")
	}
	wrongParent := sidequestion.Parent{TaskID: row.ID + 1, ExplorationID: row.ExplorationID}
	if got, err := f.s.buildFindingAutoSnapshot(t.Context(), wrongParent, findingID); !errors.Is(err, errFindingSideContext) || got != nil {
		t.Fatal("finding from another task was accepted", err)
	}
	if snap.RunID != 0 {
		t.Fatal("synthetic finding context can outrank real task capture")
	}
	if got, err := pg.SideSnapshot(t.Context(), parent.Key()); err != nil || got != nil {
		t.Fatal("building a read-only finding view wrote a checkpoint", err)
	}
	canonical := f.checkpoint(t, parent)
	canonical.Version++ // emulate an in-memory capture not persisted yet
	canonical.Request.Messages = []llm.Message{llm.UserText("canonical latest main marker")}
	f.s.side.mu.Lock()
	f.s.side.latest[parent.Key()] = canonical
	f.s.side.mu.Unlock()
	view, err := f.s.sideSnapshotForAgent(t.Context(), parent, "auto", findingID)
	if err != nil || view == nil || len(view.Request.Tools) != 0 || !strings.Contains(view.Request.Messages[0].Text(), "finding marker") {
		t.Fatal("existing task checkpoint replaced the requested finding view", err)
	}
	if got, err := f.s.sideSnapshotForAgent(t.Context(), wrongParent, "auto", findingID); !errors.Is(err, errFindingSideContext) || got != nil {
		t.Fatal("existing checkpoint bypassed finding/task ownership", err)
	}
	if err := f.s.saveQuestionSnapshot(t.Context(), parent, *view, true); err != nil {
		t.Fatal(err)
	}
	stored, err := pg.SideSnapshot(t.Context(), parent.Key())
	if err != nil || stored == nil || stored.Version != canonical.Version || stored.Request.Messages[0].Text() != "canonical latest main marker" {
		t.Fatal("finding admission overwrote the real main checkpoint", err)
	}
}
