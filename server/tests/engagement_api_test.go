//go:build ignore

package server

import (
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/Autumn-27/artex/db"
)

func TestManualGoalRequiresFixedContract(t *testing.T) {
	s, finding, request := trafficEvidenceServer(t)
	storedFinding, err := s.m.pg.GetFinding(finding.FindingID)
	if err != nil || storedFinding == nil || storedFinding.TaskID == nil {
		t.Fatal(err)
	}
	id := strconv.FormatInt(*storedFinding.TaskID, 10)
	path := "/api/tasks/" + id + "/goals"
	for _, body := range []string{
		`{"text":"inspect"}`,
		`{"text":"inspect","goal_type":"unknown"}`,
		`{"text":"inspect","goal_type":"inspection","required_checks":[]}`,
		`{"text":"inspect","goal_type":"inspection","required_checks":[" "]}`,
	} {
		if w := request("POST", path, body); w.Code != 400 {
			t.Fatalf("invalid contract accepted: %d %s", w.Code, w.Body)
		}
	}
	w := request("POST", path, `{"text":"inspect local login","goal_type":"inspection","required_checks":["two authorized candidates"]}`)
	if w.Code != 200 {
		t.Fatalf("valid inspection rejected: %d %s", w.Code, w.Body)
	}
	var goal GoalDTO
	if err := json.Unmarshal(w.Body.Bytes(), &goal); err != nil || goal.GoalType != "inspection" || len(goal.RequiredChecks) != 1 {
		t.Fatalf("contract omitted: %s err=%v", w.Body, err)
	}
	task, _ := s.m.Task(id)
	s.engine.StopTask(task.ID)
}

func TestEngagementHTTPSerializesEditAndResume(t *testing.T) {
	s, _, request := trafficEvidenceServer(t)
	for i := 0; i < 5; i++ {
		t.Run(strconv.Itoa(i), func(t *testing.T) {
			task, err := s.m.CreateTask("old contract", "old goal", nil, 0, 0)
			if err != nil {
				t.Fatal(err)
			}
			tid, _ := strconv.ParseInt(task.ID, 10, 64)
			t.Cleanup(func() { s.engine.StopTask(task.ID); _ = s.m.pg.DeleteTask(tid) })
			old, err := task.Store.AddNode(db.KindIntent, map[string]any{"summary": "old target"}, 5, "open", "planner", nil)
			if err != nil {
				t.Fatal(err)
			}
			if w := request("POST", "/api/tasks/"+task.ID+"/control", `{"action":"pause"}`); w.Code != 200 {
				t.Fatalf("pause: %d %s", w.Code, w.Body)
			}
			body := `{"goal":"new goal","description":"new target","reason":"human changed port","goals":[{"text":"inspect new port","goal_type":"inspection","required_checks":["new port"]}],"constraints":[{"kind":"allow","text":"new port only"}],"scopes":[]}`
			var edit, resume *httptest.ResponseRecorder
			start := make(chan struct{})
			var wg sync.WaitGroup
			wg.Add(2)
			go func() { defer wg.Done(); <-start; edit = request("PUT", "/api/tasks/"+task.ID+"/engagement", body) }()
			go func() {
				defer wg.Done()
				<-start
				resume = request("POST", "/api/tasks/"+task.ID+"/control", `{"action":"resume"}`)
			}()
			close(start)
			wg.Wait()
			if resume.Code != 200 {
				t.Fatalf("resume: %d %s", resume.Code, resume.Body)
			}
			if edit.Code != 200 && edit.Code != 409 {
				t.Fatalf("edit: %d %s", edit.Code, edit.Body)
			}
			stored, err := s.m.pg.GetTask(tid)
			if err != nil || stored == nil {
				t.Fatal(err)
			}
			dto := taskDTO(task, s.resolvedTaskStatus(task))
			if dto.Goal != stored.Goal || dto.Description != stored.Description {
				t.Fatalf("published contract differs: dto=%+v stored=%+v", dto, stored)
			}
			node, err := task.Store.GetNode(old)
			if err != nil || node == nil {
				t.Fatal(err)
			}
			if edit.Code == 200 {
				if dto.Goal != "new goal" || dto.Description != "new target" || node.State != "stopped" || !strings.Contains(string(node.Payload), `"retired_contract": true`) {
					t.Fatalf("accepted edit retained old execution: %+v node=%+v", dto, node)
				}
			} else if stored.Goal != "old goal" || stored.Description != "old contract" || node.State != "open" {
				t.Fatalf("rejected edit mutated contract: %+v node=%+v", stored, node)
			}
		})
	}
}

func TestTaskDTOContractSnapshotIsAtomic(t *testing.T) {
	task := &Task{ID: "1", Goal: "initial", Description: "initial"}
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		for i := 0; i < 2000; i++ {
			value := fmt.Sprintf("contract-%d", i)
			task.setContract(value, value)
		}
	}()
	for i := 0; i < 2000; i++ {
		dto := taskDTO(task, "paused")
		if dto.Goal != dto.Description {
			<-finished
			t.Fatalf("mixed contract snapshot: %+v", dto)
		}
	}
	<-finished
}
