package server

import (
	"encoding/json"
	"github.com/Autumn-27/artex/agent"
	"net/http"
	"strings"

	"github.com/Autumn-27/artex/db"
)

func (s *Server) saveFindingReproduction(w http.ResponseWriter, r *http.Request) {
	id, ok := pathInt(r, "id")
	if !ok || id <= 0 {
		writeErr(w, 400, "漏洞 ID 无效")
		return
	}
	f, err := s.m.pg.GetFinding(id)
	if err != nil || f == nil {
		writeErr(w, 404, "漏洞不存在")
		return
	}
	if taskID := strings.TrimSpace(r.URL.Query().Get("context_task")); taskID != "" {
		task := s.m.ResolveTask(taskID)
		if task == nil {
			writeErr(w, 404, "任务不存在")
			return
		}
		_, inherited, allowed := findingProvenanceInTask(task, f.TaskID)
		if !allowed || inherited {
			writeErr(w, 403, "继承漏洞只读")
			return
		}
	}
	var input struct {
		EvidenceVersion int64            `json:"evidence_version"`
		Reproduction    *db.Reproduction `json:"reproduction"`
	}
	if !knowledgeDecode(w, r, &input) {
		return
	}
	if err = s.m.pg.SaveFindingReproduction(r.Context(), id, input.EvidenceVersion, input.Reproduction); err != nil {
		if err == db.ErrReproduction {
			writeErr(w, 400, err.Error())
		} else {
			evidenceError(w, err)
		}
		return
	}
	saved, err := s.m.pg.GetFinding(id)
	if err != nil {
		writeErr(w, 503, "保存后读取失败，请刷新核对")
		return
	}
	writeJSON(w, 200, findingFromDB(saved, s.resolveAssetIDs(saved.AssetIDs)))
}

func (s *Server) seedReproductionSchema() {
	row, err := s.m.pg.GetTool("report_finding")
	if err != nil || row == nil || !row.System {
		return
	}
	var schema map[string]any
	if json.Unmarshal(row.Schema, &schema) != nil || schema == nil {
		return
	}
	props := objectProperty(schema, "properties")
	if _, exists := props["reproduction"]; exists {
		return
	}
	props["reproduction"] = agent.ReproductionSchema()
	raw, err := json.Marshal(schema)
	if err != nil {
		return
	}
	_, _ = s.m.pg.Exec(`UPDATE tools SET schema=$2::jsonb WHERE key=$1 AND system AND schema=$3::jsonb`, "report_finding", string(raw), string(row.Schema))
}
