package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Autumn-27/artex/stage"
)

func TestStageCreateHonorsTaskArchiveBarrier(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	managed, err := stage.New(ctx, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer managed.Close()
	e := NewEngine(nil)
	s := &Server{ctx: ctx, engine: e, m: &Manager{tasks: map[string]*Task{"7": {ID: "7"}}}, stage: managed}
	if !e.BeginDelete("7") {
		t.Fatal("archive barrier not installed")
	}
	defer e.AbortDelete("7", false)
	r := httptest.NewRequest(http.MethodPost, "/api/stage", strings.NewReader(`{"name":"fixture","content_base64":"Zml4dHVyZQ==","task_id":"7"}`))
	w := httptest.NewRecorder()
	s.stageCreate(w, r)
	if w.Code != http.StatusConflict || len(managed.List()) != 0 {
		t.Fatalf("resource created after archive barrier: status=%d", w.Code)
	}
}

func TestIntranetTaskOperationDrainsThroughEngineCounter(t *testing.T) {
	e := NewEngine(nil)
	s := &Server{engine: e, m: &Manager{tasks: map[string]*Task{"7": {ID: "7"}}}}
	done, err := s.beginIntranetTaskOperation(7)
	if err != nil {
		t.Fatal(err)
	}
	if e.inflightCount("7") != 1 {
		t.Fatal("manual resource operation was not registered")
	}
	done()
	done()
	if e.inflightCount("7") != 0 {
		t.Fatal("manual resource operation did not drain")
	}
}
