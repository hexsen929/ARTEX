package server

import (
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"testing"
	"time"
)

func TestOperatorAgentCardsKeepStatisticsWithNullCustomBinding(t *testing.T) {
	s := operatorFixture(t)
	key := fmt.Sprintf("card-null-%d", time.Now().UnixNano())
	if _, err := s.m.pg.Exec(`INSERT INTO tools(key,system,kind,agents) VALUES($1,false,'script','null'::jsonb)`, key); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = s.m.pg.Exec(`DELETE FROM tools WHERE key=$1`, key) })
	w := httptest.NewRecorder()
	s.pgListAgents(w, httptest.NewRequest("GET", "/api/agents", nil))
	if w.Code != 200 {
		t.Fatalf("Agent list status=%d", w.Code)
	}
	var result struct {
		Agents []AgentDTO `json:"agents"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if len(result.Agents) == 0 {
		t.Fatal("empty Agent list")
	}
	for _, a := range result.Agents {
		if a.BindingCountsAvailable == nil || !*a.BindingCountsAvailable || a.BindingCountsError != "" {
			t.Fatalf("one unbound custom tool made Agent %s statistics unavailable", a.Key)
		}
	}
}
