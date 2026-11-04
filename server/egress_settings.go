package server

import "net/http"

// Navbar refreshes need only two fields. Never serialize the full proxy pool
// (or take its mutex) just to refresh an already opted-in egress banner.
func (s *Server) getEgressSettings(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"show_egress_ip": s.m.pg.GetBool(settingShowEgressIP, false),
		"global_proxy":   maskSecrets(s.m.GlobalProxy()),
	})
}
