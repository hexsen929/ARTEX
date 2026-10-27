package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/Autumn-27/artex/db"
	"github.com/Autumn-27/artex/session"
)

func TestIntranetUnbindSessionRevokesOnlySelectedTaskGrant(t *testing.T) {
	pg := openIntranetTestDB(t)
	owner := intranetFixtureTask(t, pg)
	child := intranetFixtureTask(t, pg, owner.ID)
	otherChild := intranetFixtureTask(t, pg, owner.ID)
	ctx := context.Background()
	store := db.NewSessionStore(pg, []byte("intranet-binding-encryption-fixture"))
	id, err := store.Create(ctx, &db.SessionRecord{
		Kind: "http_php", URL: "http://fixture.invalid/session", Lang: "php", Status: db.SessionAlive,
		CreatedByTask: owner.ID, Secret: json.RawMessage(`{"fixture":"not-a-real-secret"}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Delete(context.Background(), id) })
	for _, task := range []*db.Task{child, otherChild} {
		if err := store.BindTask(ctx, id, task.ID); err != nil {
			t.Fatal(err)
		}
	}
	m := &Manager{pg: pg, tasks: map[string]*Task{}}
	for _, task := range []*db.Task{owner, child, otherChild} {
		key := strconv.FormatInt(task.ID, 10)
		m.tasks[key] = &Task{ID: key}
	}
	s := &Server{ctx: ctx, m: m, engine: NewEngine(m), sessStore: store, sessReg: session.NewRegistry(),
		jwtKey: []byte("intranet-binding-auth-key-fixture"), authkv: newMemKV()}
	s.sessReg.Add(&taskSessionFixture{id: id})
	mux := http.NewServeMux()
	mux.HandleFunc("DELETE /api/tasks/{task}/sessions/{session}/binding", s.intranetUnbindSession)
	handler := s.requireAuth(mux)
	token, err := signJWT(s.jwtKey, 0)
	if err != nil {
		t.Fatal(err)
	}
	request := func(task, session string, authorized bool) *httptest.ResponseRecorder {
		t.Helper()
		r := httptest.NewRequest(http.MethodDelete, "/api/tasks/"+task+"/sessions/"+session+"/binding", nil)
		if authorized {
			r.Header.Set("Authorization", "Bearer "+token)
		}
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		if strings.Contains(w.Body.String(), "not-a-real-secret") || strings.Contains(w.Body.String(), "fixture.invalid") {
			t.Fatal("binding API returned session connection details")
		}
		return w
	}
	ownerID, childID, sessionID := strconv.FormatInt(owner.ID, 10), strconv.FormatInt(child.ID, 10), strconv.FormatInt(id, 10)
	if w := request(childID, sessionID, false); w.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated unbind status=%d", w.Code)
	}
	if allowed, err := store.TaskCanUse(ctx, id, child.ID); err != nil || !allowed {
		t.Fatal("unauthenticated request changed a binding")
	}
	if w := request(ownerID, sessionID, true); w.Code != http.StatusConflict {
		t.Fatalf("owner unbind status=%d", w.Code)
	}
	if w := request("invalid", sessionID, true); w.Code != http.StatusBadRequest {
		t.Fatalf("invalid task status=%d", w.Code)
	}
	if w := request(childID, "0", true); w.Code != http.StatusBadRequest {
		t.Fatalf("invalid session status=%d", w.Code)
	}
	if w := request("9223372036854775807", sessionID, true); w.Code != http.StatusNotFound {
		t.Fatalf("missing task status=%d", w.Code)
	}
	if w := request(childID, "9223372036854775807", true); w.Code != http.StatusNotFound {
		t.Fatalf("missing session status=%d", w.Code)
	}
	if !s.engine.BeginDelete(childID) {
		t.Fatal("could not install archive barrier")
	}
	if w := request(childID, sessionID, true); w.Code != http.StatusConflict {
		t.Fatalf("unbind crossed archive barrier: status=%d", w.Code)
	}
	if allowed, err := store.TaskCanUse(ctx, id, child.ID); err != nil || !allowed {
		t.Fatal("archive-rejected request changed the binding")
	}
	s.engine.AbortDelete(childID, true)
	if err := s.checkTaskIntranetArchive(ctx, child.ID); err == nil {
		t.Fatal("bound live session did not block child archive")
	}
	for i, wantUnbound := range []bool{true, false} {
		w := request(childID, sessionID, true)
		if w.Code != http.StatusOK {
			t.Fatalf("unbind attempt %d status=%d", i, w.Code)
		}
		var response struct {
			TaskID    string `json:"task_id"`
			SessionID string `json:"session_id"`
			Unbound   bool   `json:"unbound"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil {
			t.Fatal(err)
		}
		if response.TaskID != childID || response.SessionID != sessionID || response.Unbound != wantUnbound {
			t.Fatalf("unexpected unbind result on attempt %d", i)
		}
	}
	if allowed, err := store.TaskCanUse(ctx, id, child.ID); err != nil || allowed {
		t.Fatal("selected task retained revoked control")
	}
	for _, task := range []*db.Task{owner, otherChild} {
		if allowed, err := store.TaskCanUse(ctx, id, task.ID); err != nil || !allowed {
			t.Fatal("unbind revoked the owner or another task's control")
		}
	}
	stored, err := store.Get(ctx, id)
	if err != nil || stored == nil || stored.Status != db.SessionAlive || stored.CreatedByTask != owner.ID {
		t.Fatal("unbind closed, deleted or reassigned the shared parent session")
	}
	if _, live := s.sessReg.Get(id); !live {
		t.Fatal("unbind removed the shared session from the live registry")
	}
	if err := s.checkTaskIntranetArchive(ctx, child.ID); err != nil {
		t.Fatalf("revoked child still blocked by parent session: %v", err)
	}
	if err := s.checkTaskIntranetArchive(ctx, owner.ID); err == nil {
		t.Fatal("owner archive ignored its live session")
	}
	if err := s.checkTaskIntranetArchive(ctx, otherChild.ID); err == nil {
		t.Fatal("other bound task lost its live-session archive protection")
	}
	if s.engine.inflightCount(childID) != 0 || s.engine.inflightCount(ownerID) != 0 {
		t.Fatal("binding endpoint leaked an operation lease")
	}
}
