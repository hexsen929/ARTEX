package server

import "log"

func (s *Server) seedGoalsScopeBinding() {
	const flag = "goals_scope_binding_v1"
	if v, _, _ := s.m.pg.GetSetting(flag); v == "true" {
		return
	}
	if err := s.m.pg.AddAgentToToolBinding("goals", []string{"add_task_scope"}); err != nil {
		log.Printf("[tools] goals scope binding: %v", err)
		return
	}
	_ = s.m.pg.SetSetting(flag, "true")
}
