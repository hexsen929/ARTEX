package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"unicode/utf8"
)

func isOperatorExpert(key string) bool {
	var defs []struct {
		Key string `json:"key"`
	}
	if json.Unmarshal(operatorAgentDefinitions, &defs) != nil {
		return false
	}
	for _, d := range defs {
		if d.Key == key {
			return true
		}
	}
	return false
}
func (s *Server) admitOperatorTrigger(ctx context.Context, key string, task int64) (bool, error) {
	if !isOperatorExpert(key) || task <= 0 {
		return true, nil
	}
	if err := ctx.Err(); err != nil {
		return false, err
	}
	cap := 30
	for _, setting := range []string{"trigger_run_cap", "trigger_run_cap_" + key} {
		value, ok, err := s.m.pg.GetSetting(setting)
		if err != nil {
			return false, err
		}
		if ok {
			n, err := strconv.Atoi(value)
			if err == nil && n >= 0 {
				cap = n
			}
		}
	}
	if cap <= 0 {
		return true, nil
	}
	name := fmt.Sprintf("trigger.budget.%d.%s", task, key)
	s.operatorTriggerMu.Lock()
	defer s.operatorTriggerMu.Unlock()
	if s.operatorTriggerCounts == nil {
		s.operatorTriggerCounts = map[string]int{}
	}
	if s.operatorTriggerCounts[name] >= cap {
		return false, nil
	}
	s.operatorTriggerCounts[name]++
	return true, nil
}
func (s *Server) operatorTriggerBudget(w http.ResponseWriter, r *http.Request) {
	if s.pg(w) == nil {
		return
	}
	if r.Method == "PUT" {
		var v struct {
			Cap   int    `json:"cap"`
			Agent string `json:"agent"`
		}
		if !operatorDecode(w, r, &v) {
			return
		}
		if v.Cap < 0 || v.Cap > 1000 || v.Agent != "" && !isOperatorExpert(v.Agent) {
			writeErr(w, 400, "cap must be 0-1000 (0 is unlimited) and agent must be a known expert")
			return
		}
		key := "trigger_run_cap"
		if v.Agent != "" {
			key += "_" + v.Agent
		}
		if err := s.m.pg.SetSetting(key, strconv.Itoa(v.Cap)); err != nil {
			operatorError(w, err)
			return
		}
	}
	s.operatorTriggerMu.Lock()
	items := []map[string]any{}
	for key, count := range s.operatorTriggerCounts {
		items = append(items, map[string]any{"key": key, "value": map[string]int{"count": count}})
	}
	s.operatorTriggerMu.Unlock()

	cap := "30"
	if v, ok, _ := s.m.pg.GetSetting("trigger_run_cap"); ok {
		cap = v
	}
	writeJSON(w, 200, map[string]any{"default_cap": cap, "items": items})
}
func (s *Server) operatorTaskSteer(w http.ResponseWriter, r *http.Request) {
	if s.pg(w) == nil {
		return
	}
	id, err := pathID(r, "id")
	if err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	task, ok := s.m.Task(strconv.FormatInt(id, 10))
	if !ok || task.Store == nil {
		writeErr(w, 404, "task not loaded")
		return
	}
	rows, err := s.m.pg.QueryContext(r.Context(), `SELECT id,payload FROM exploration_nodes WHERE exploration_id=$1 AND kind='intent' AND state='running' ORDER BY id LIMIT 100`, task.Store.ID())
	if err != nil {
		operatorError(w, err)
		return
	}
	items := []map[string]any{}
	allowed := map[int64]bool{}
	for rows.Next() {
		var nid int64
		var raw []byte
		if err = rows.Scan(&nid, &raw); err != nil {
			rows.Close()
			operatorError(w, err)
			return
		}
		if s.engine != nil && !s.engine.HasRunningIntent(nid) {
			continue
		}
		var payload map[string]any
		_ = json.Unmarshal(raw, &payload)
		pending := 0
		if s.engine != nil {
			pending = s.engine.PendingSteers(nid)
		}
		items = append(items, map[string]any{"intent_id": nid, "payload": payload, "pending": pending})
		allowed[nid] = true
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		operatorError(w, err)
		return
	}
	if r.Method == "GET" {
		writeJSON(w, 200, map[string]any{"running": items, "available": len(items) > 0})
		return
	}
	if s.engine == nil {
		writeErr(w, 503, "execution engine unavailable")
		return
	}
	var v struct {
		Message string  `json:"message"`
		IDs     []int64 `json:"intent_ids"`
	}
	if !operatorDecode(w, r, &v) {
		return
	}
	v.Message = strings.TrimSpace(v.Message)
	if v.Message == "" || utf8.RuneCountInString(v.Message) > 2000 || len(v.IDs) > 100 {
		writeErr(w, 400, "message must contain 1-2000 characters")
		return
	}
	if len(v.IDs) == 0 {
		for id := range allowed {
			v.IDs = append(v.IDs, id)
		}
	}
	delivered, skipped := []int64{}, []int64{}
	seen := map[int64]bool{}
	for _, nid := range v.IDs {
		if seen[nid] {
			continue
		}
		seen[nid] = true
		if !allowed[nid] {
			skipped = append(skipped, nid)
			continue
		}
		if err := s.engine.SteerWork(nid, v.Message); err != nil {
			skipped = append(skipped, nid)
		} else {
			delivered = append(delivered, nid)
		}
	}
	if len(delivered) == 0 {
		writeErr(w, 409, "no running worker accepted steering; task state was not changed")
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true, "delivered": delivered, "skipped": skipped})
}

func operatorExecutionRole(key string) bool {
	return key == "pentest" || isOperatorExpert(key)
}
