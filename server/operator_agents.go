package server

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"github.com/Autumn-27/artex/db"
	"log"
	"net/http"
)

// Role definitions are data recovered from the user-supplied runtime. Existing
// agents/prompts/bindings are never overwritten, and automatic triggers are
// initially disabled so installing modules cannot launch work on active tasks.
//
//go:embed operator_agents.json
var operatorAgentDefinitions []byte

func (s *Server) seedOperatorAgents() {
	var done map[string]bool
	if s.m.pg.OperatorSetting(s.ctx, "operator.agents.seeded.v1", &done) == nil && done["done"] {
		return
	}
	var defs []struct {
		Key         string            `json:"key"`
		Name        string            `json:"name"`
		Description string            `json:"description"`
		Prompt      string            `json:"prompt"`
		RunSeconds  int               `json:"run_seconds"`
		WebSearch   bool              `json:"web_search"`
		RunMode     string            `json:"trigger_run_mode"`
		MergeMode   string            `json:"trigger_merge_mode"`
		Parallel    int               `json:"trigger_max_parallel"`
		Skills      []string          `json:"skills"`
		Tools       []string          `json:"tools"`
		Triggers    []db.AgentTrigger `json:"triggers"`
	}
	if err := json.Unmarshal(operatorAgentDefinitions, &defs); err != nil {
		log.Printf("[operator] role definitions invalid")
		return
	}
	for _, v := range defs {
		old, err := s.m.pg.GetAgentByKey(v.Key)
		if err != nil {
			log.Printf("[operator] role lookup failed")
			return
		}
		if old != nil {
			continue
		}
		a, err := s.m.pg.CreateAgent(v.Key, v.Name, v.Description)
		if err != nil {
			log.Printf("[operator] create role %s: %v", v.Key, err)
			return
		}
		if _, err = s.m.pg.SavePrompt(a.ID, v.Prompt, "operator role template", "migration"); err != nil {
			return
		}
		_, err = s.m.pg.Exec(`UPDATE agents SET run_seconds=$2,web_search=$3,trigger_run_mode=$4,trigger_merge_mode=$5,trigger_max_parallel=$6 WHERE key=$1`, v.Key, v.RunSeconds, v.WebSearch, v.RunMode, v.MergeMode, v.Parallel)
		if err != nil {
			return
		}
		if err = s.m.pg.SetAgentSkillVisibility(a.ID, v.Skills); err != nil {
			return
		}
		for _, key := range v.Tools {
			_, err = s.m.pg.Exec(`UPDATE tools SET agents=agents||jsonb_build_array($1::text) WHERE key=$2 AND NOT (agents ? $1)`, v.Key, key)
			if err != nil {
				return
			}
		}
		for _, tr := range v.Triggers {
			tr.ID = 0
			tr.AgentKey = v.Key
			tr.Enabled = false
			tr.LastFire = nil
			if _, err = s.m.pg.CreateTrigger(&tr); err != nil {
				log.Printf("[operator] trigger template %s: %v", v.Key, err)
				return
			}
		}
		log.Printf("[operator] installed role %s (automatic triggers disabled)", v.Key)
	}
	if err := s.m.pg.SaveOperatorSetting(s.ctx, "operator.agents.seeded.v1", map[string]bool{"done": true}); err != nil {
		log.Printf("[operator] seed marker: %v", err)
	}
}
func (s *Server) operatorTaskExperts(w http.ResponseWriter, r *http.Request) {
	if s.pg(w) == nil {
		return
	}
	id, err := pathID(r, "id")
	if err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	var defs []map[string]any
	_ = json.Unmarshal(operatorAgentDefinitions, &defs)
	keys := []string{}
	for _, d := range defs {
		keys = append(keys, fmt.Sprint(d["key"]))
	}
	items, err := s.operatorSQLList(r.Context(), `SELECT to_jsonb(x) FROM (SELECT ca.conversation_id,ca.worker,MAX(ca.seq) AS last_seq FROM conversation_activities ca WHERE ca.task_id=$1 GROUP BY ca.conversation_id,ca.worker) x`, id)
	if err != nil {
		operatorError(w, err)
		return
	}
	writeJSON(w, 200, map[string]any{"experts": keys, "runs": items})
}
