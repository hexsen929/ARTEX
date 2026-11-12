package server

import (
	"context"
	"database/sql"
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
	cap := 30
	for _, setting := range []string{"trigger_run_cap", "trigger_run_cap_" + key} {
		v, ok, err := s.m.pg.GetSetting(setting)
		if err != nil {
			return false, err
		}
		if ok {
			n, e := strconv.Atoi(v)
			if e != nil || n < 1 || n > 1000 {
				return false, fmt.Errorf("invalid trigger cap")
			}
			cap = n
		}
	}
	name := fmt.Sprintf("trigger.budget.%d.%s", task, key)
	var raw []byte
	err := s.m.pg.QueryRowContext(ctx, `INSERT INTO operator_module_settings(key,value) VALUES($1,'{"count":1}') ON CONFLICT(key) DO UPDATE SET value=jsonb_set(operator_module_settings.value,'{count}',to_jsonb((operator_module_settings.value->>'count')::int+1)),updated_at=now() WHERE (operator_module_settings.value->>'count')::int<$2 RETURNING value`, name, cap).Scan(&raw)
	if err == sql.ErrNoRows {
		return false, nil
	}
	return err == nil, err
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
		if v.Cap < 1 || v.Cap > 1000 || v.Agent != "" && !isOperatorExpert(v.Agent) {
			writeErr(w, 400, "cap must be 1-1000 and agent must be a known expert")
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
	items, err := s.operatorSQLList(r.Context(), `SELECT jsonb_build_object('key',key,'value',value,'updated_at',updated_at) FROM operator_module_settings WHERE key LIKE 'trigger.budget.%' ORDER BY updated_at DESC LIMIT 100`)
	if err != nil {
		operatorError(w, err)
		return
	}
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
		var payload map[string]any
		_ = json.Unmarshal(raw, &payload)
		items = append(items, map[string]any{"intent_id": nid, "payload": payload})
		allowed[nid] = true
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		operatorError(w, err)
		return
	}
	if r.Method == "GET" {
		writeJSON(w, 200, map[string]any{"running": items})
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
