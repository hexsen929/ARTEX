package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"

	"github.com/Autumn-27/artex/db"
)

func TestTopologyMappingWiringOutcomeDoesNotInventSuccess(t *testing.T) {
	for _, tc := range []struct {
		running               bool
		user, result, failure int64
		resultFailed          bool
		want                  string
	}{
		{true, 1, 2, 2, true, "running"},
		{false, 1, 2, 2, true, "failed"},
		{false, 1, 2, 3, false, "failed"},
		{false, 1, 2, 0, false, "finished"},
		{false, 3, 2, 2, true, "interrupted"},
		{false, 1, 0, 0, false, "interrupted"},
	} {
		if got := topologyMappingOutcome(tc.running, tc.user, tc.result, tc.failure, tc.resultFailed); got != tc.want {
			t.Errorf("outcome=%q want=%q", got, tc.want)
		}
	}
}

func TestTopologyMappingWiringPersistsNativeTaskAssociationAndFailure(t *testing.T) {
	m, err := NewManager(t.TempDir(), "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = m.Close() })
	task, err := m.CreateTask("topology wiring fixture", "synthetic stored data only", nil, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = m.DeleteTask(task.ID, DeleteTaskOptions{}) })
	s := &Server{m: m, chatBusy: map[string]bool{}}
	conversation, err := m.pg.CreateConversation("asset_mapper", fmt.Sprintf("资产与网络梳理 · 任务 #%s", task.ID), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = m.pg.DeleteConversation(conversation.ID) })
	taskID, err := strconv.ParseInt(task.ID, 10, 64)
	if err != nil {
		t.Fatal(err)
	}
	if err := appendTopologyMappingAssociation(m.pg, conversation.ID, taskID); err != nil {
		t.Fatal(err)
	}
	if err := m.pg.RenameConversation(conversation.ID, "user renamed fixture"); err != nil {
		t.Fatal(err)
	}
	for _, activity := range []db.Activity{
		{Worker: "asset_mapper", Kind: "user", Detail: "fixture question"},
		{Worker: "asset_mapper", Kind: "result", IsError: true, Detail: "fixture provider failure"},
	} {
		if _, err := m.pg.AppendConvActivity(conversation.ID, activity); err != nil {
			t.Fatal(err)
		}
	}
	read := func() topologyMappingState {
		t.Helper()
		request := httptest.NewRequest(http.MethodGet, "/api/tasks/"+task.ID+"/topology-mapping/status", nil)
		request.SetPathValue("id", task.ID)
		response := httptest.NewRecorder()
		s.taskAssetMappingStatus(response, request)
		if response.Code != http.StatusOK {
			t.Fatalf("status=%d: %s", response.Code, response.Body)
		}
		var state topologyMappingState
		if err := json.Unmarshal(response.Body.Bytes(), &state); err != nil {
			t.Fatal(err)
		}
		return state
	}
	state := read()
	if state.Running || state.Status != "failed" || state.ConversationID != conversation.ID || state.FinishedAt == "" || state.Association != "native" {
		t.Fatalf("failed/renamed conversation was lost or called successful: %+v", state)
	}
	s.chatBusy[s.convBusyKey(conversation.ID)] = true
	if state := read(); !state.Running || state.Status != "running" {
		t.Fatalf("live run not reflected: %+v", state)
	}
	delete(s.chatBusy, s.convBusyKey(conversation.ID))
	// An editable matching title must not override a native different-task tag.
	other, err := m.pg.CreateConversation("asset_mapper", fmt.Sprintf("资产与网络梳理 · 任务 #%s", task.ID), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = m.pg.DeleteConversation(other.ID) })
	if err := appendTopologyMappingAssociation(m.pg, other.ID, 9223372036854775800); err != nil {
		t.Fatal(err)
	}
	if state := read(); state.ConversationID != conversation.ID {
		t.Fatalf("editable title replaced the native task association: %+v", state)
	}
	// Old builds did not persist the native marker. Preserve their exact-title
	// lookup, but expose an ended record without calling it verified success.
	legacy, err := m.pg.CreateConversation("asset_mapper", fmt.Sprintf("资产梳理 · 任务 #%s", task.ID), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = m.pg.DeleteConversation(legacy.ID) })
	for _, activity := range []db.Activity{{Kind: "user", Detail: "legacy fixture question"}, {Kind: "result", Detail: "legacy fixture report"}} {
		if _, err := m.pg.AppendConvActivity(legacy.ID, activity); err != nil {
			t.Fatal(err)
		}
	}
	if state := read(); state.Running || state.Status != "finished" || state.ConversationID != legacy.ID || state.Association != "legacy_title" {
		t.Fatalf("legacy report link disappeared or remained polling: %+v", state)
	}
}
