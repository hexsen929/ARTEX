package server

import (
	"encoding/json"
	"net/http"
	"strconv"
)

// The ID is a standalone findings.id, not an exploration node ID. History is
// read-only and never changes severity, verification or evidence versions.
func (s *Server) findingInlineHistory(w http.ResponseWriter, r *http.Request) {
	pg := s.pg(w)
	if pg == nil {
		return
	}
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		writeErr(w, 400, "invalid finding id")
		return
	}
	finding, err := pg.GetFinding(id)
	if err != nil {
		writeErr(w, 500, "finding lookup failed")
		return
	}
	if finding == nil {
		writeErr(w, 404, "finding not found")
		return
	}
	events, err := pg.ListFindingInlineMergeEvents(id)
	if err != nil {
		writeErr(w, 500, "finding history lookup failed")
		return
	}
	type historyDTO struct {
		ID        string          `json:"id"`
		FindingID string          `json:"finding_id"`
		Kind      string          `json:"kind"`
		Payload   json.RawMessage `json:"payload"`
		CreatedAt string          `json:"created_at"`
	}
	rows := make([]historyDTO, 0, len(events))
	for _, event := range events {
		rows = append(rows, historyDTO{ID: i64s(event.ID), FindingID: i64s(event.FindingID), Kind: event.Kind, Payload: event.Payload, CreatedAt: rfc3339(event.CreatedAt)})
	}
	writeJSON(w, 200, map[string]any{"events": rows})
}
