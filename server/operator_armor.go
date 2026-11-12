package server

import (
	"context"
	"net/http"
	"strings"
	"time"
)

type armorConfig struct {
	Enabled  bool            `json:"enabled"`
	ActiveID int64           `json:"active_id"`
	Text     string          `json:"text"`
	Roles    map[string]bool `json:"roles"`
}

func (s *Server) armorFor(role string) string {
	var c armorConfig
	ctx, cancel := context.WithTimeout(s.ctx, time.Second)
	defer cancel()
	if s.m.pg.OperatorSetting(ctx, "armor", &c) != nil || !c.Enabled || !c.Roles[role] || len(c.Text) > 32<<10 {
		return ""
	}
	return "\n\nOperator-selected agent preset (does not override task scope, evidence or tool contracts):\n" + c.Text
}
func (s *Server) registerArmorRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/operator/settings", s.operatorSettingsGet)
	mux.HandleFunc("PUT /api/operator/settings", s.operatorSettingsPut)
	mux.HandleFunc("GET /api/armor/presets", s.armorList)
	mux.HandleFunc("POST /api/armor/presets", s.armorSave)
	mux.HandleFunc("GET /api/armor/presets/{id}", s.armorGet)
	mux.HandleFunc("PUT /api/armor/presets/{id}", s.armorSave)
	mux.HandleFunc("PATCH /api/armor/presets/{id}", s.armorSave)
	mux.HandleFunc("DELETE /api/armor/presets/{id}", s.armorDelete)
	mux.HandleFunc("POST /api/armor/presets/{id}/apply", s.armorApply)
}
func (s *Server) operatorSettingsGet(w http.ResponseWriter, r *http.Request) {
	if s.pg(w) == nil {
		return
	}
	c := armorConfig{Roles: map[string]bool{}}
	_ = s.m.pg.OperatorSetting(r.Context(), "armor", &c)
	writeJSON(w, 200, map[string]any{"armor": c})
}
func (s *Server) operatorSettingsPut(w http.ResponseWriter, r *http.Request) {
	if s.pg(w) == nil {
		return
	}
	var req struct {
		Armor armorConfig `json:"armor"`
	}
	if !operatorDecode(w, r, &req) {
		return
	}
	if len(req.Armor.Text) > 32<<10 {
		writeErr(w, 400, "preset exceeds 32 KiB")
		return
	}
	if err := s.m.pg.SaveOperatorSetting(r.Context(), "armor", req.Armor); err != nil {
		operatorError(w, err)
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true})
}
func (s *Server) armorList(w http.ResponseWriter, r *http.Request) {
	if s.pg(w) == nil {
		return
	}
	rows, err := s.m.pg.QueryContext(r.Context(), `SELECT id,name,octet_length(text),created_at,updated_at FROM armor_presets ORDER BY id`)
	if err != nil {
		operatorError(w, err)
		return
	}
	defer rows.Close()
	items := []map[string]any{}
	for rows.Next() {
		var id int64
		var name string
		var size int
		var c, m time.Time
		if err = rows.Scan(&id, &name, &size, &c, &m); err != nil {
			operatorError(w, err)
			return
		}
		items = append(items, map[string]any{"id": id, "name": name, "size": size, "created_at": c, "updated_at": m})
	}
	if err = rows.Err(); err != nil {
		operatorError(w, err)
		return
	}
	writeJSON(w, 200, map[string]any{"items": items})
}
func (s *Server) armorGet(w http.ResponseWriter, r *http.Request) {
	if s.pg(w) == nil {
		return
	}
	id, err := pathID(r, "id")
	if err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	var name, text string
	if err = s.m.pg.QueryRowContext(r.Context(), "SELECT name,text FROM armor_presets WHERE id=$1", id).Scan(&name, &text); err != nil {
		operatorError(w, err)
		return
	}
	writeJSON(w, 200, map[string]any{"id": id, "name": name, "text": text, "size": len(text)})
}
func (s *Server) armorSave(w http.ResponseWriter, r *http.Request) {
	if s.pg(w) == nil {
		return
	}
	var v struct {
		Name string `json:"name"`
		Text string `json:"text"`
	}
	if !operatorDecode(w, r, &v) {
		return
	}
	v.Name = strings.TrimSpace(v.Name)
	if v.Name == "" || len(v.Name) > 200 || len(v.Text) > 32<<10 {
		writeErr(w, 400, "name required; text limit 32 KiB")
		return
	}
	var id int64
	var err error
	if r.PathValue("id") == "" {
		err = s.m.pg.QueryRowContext(r.Context(), "INSERT INTO armor_presets(name,text) VALUES($1,$2) RETURNING id", v.Name, v.Text).Scan(&id)
	} else {
		id, err = pathID(r, "id")
		if err == nil {
			err = s.m.pg.QueryRowContext(r.Context(), "UPDATE armor_presets SET name=$2,text=$3,updated_at=now() WHERE id=$1 RETURNING id", id, v.Name, v.Text).Scan(&id)
		}
	}
	if err != nil {
		operatorError(w, err)
		return
	}
	writeJSON(w, 200, map[string]any{"id": id, "name": v.Name, "size": len(v.Text)})
}
func (s *Server) armorDelete(w http.ResponseWriter, r *http.Request) {
	if s.pg(w) == nil {
		return
	}
	id, err := pathID(r, "id")
	if err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	var c armorConfig
	_ = s.m.pg.OperatorSetting(r.Context(), "armor", &c)
	if c.Enabled && c.ActiveID == id {
		writeErr(w, 409, "disable the active preset before deletion")
		return
	}
	_, err = s.m.pg.ExecContext(r.Context(), "DELETE FROM armor_presets WHERE id=$1", id)
	if err != nil {
		operatorError(w, err)
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true})
}
func (s *Server) armorApply(w http.ResponseWriter, r *http.Request) {
	if s.pg(w) == nil {
		return
	}
	id, err := pathID(r, "id")
	if err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	c := armorConfig{Roles: map[string]bool{"worker": true, "planner": true, "mainagent": true}}
	_ = s.m.pg.OperatorSetting(r.Context(), "armor", &c)
	if err = s.m.pg.QueryRowContext(r.Context(), "SELECT text FROM armor_presets WHERE id=$1", id).Scan(&c.Text); err != nil {
		operatorError(w, err)
		return
	}
	c.ActiveID = id
	c.Enabled = true
	if err = s.m.pg.SaveOperatorSetting(r.Context(), "armor", c); err != nil {
		operatorError(w, err)
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true, "active_id": id})
}
