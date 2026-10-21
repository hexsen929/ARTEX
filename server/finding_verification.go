package server

import (
	"database/sql"
	"encoding/json"
	"errors"
	"github.com/Autumn-27/artex/db"
	"net/http"
)

func (s *Server) patchFindingVerification(w http.ResponseWriter, r *http.Request) {
	id := int64(atoiDefault(r.PathValue("id"), 0))
	var body struct {
		AdditionalEvidence string `json:"additional_evidence"`
		Status             string `json:"status"`
		Note               string `json:"note"`
		Version            *int64 `json:"version"`
	}
	if id <= 0 || json.NewDecoder(http.MaxBytesReader(w, r.Body, 256000)).Decode(&body) != nil || body.Version == nil || !db.ValidVerificationStatus(body.Status) || body.Status == db.VerificationLegacy {
		writeErr(w, 400, "provide finding id, status, note and current evidence version")
		return
	}
	err := s.m.pg.SetFindingVerification(r.Context(), id, body.Status, body.Note, *body.Version, body.AdditionalEvidence)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		writeErr(w, 404, "finding not found")
		return
	case errors.Is(err, db.ErrEvidenceConflict):
		writeErr(w, 409, err.Error())
		return
	case errors.Is(err, db.ErrVerificationInput):
		writeErr(w, 400, err.Error())
		return
	case err != nil:
		writeErr(w, 500, "verification update failed")
		return
	}
	f, err := s.m.pg.GetFinding(id)
	if err != nil || f == nil {
		writeErr(w, 500, "finding reload failed")
		return
	}
	writeJSON(w, 200, findingFromDB(f, s.resolveAssetIDs(f.AssetIDs)))
}
