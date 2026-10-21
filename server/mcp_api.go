package server

import (
	"context"
	"log"
	"net/http"
	"time"

	"github.com/Autumn-27/artex/db"
)

func (s *Server) pgListMCP(w http.ResponseWriter, r *http.Request) {
	pg := s.pg(w)
	if pg == nil {
		return
	}
	ms, err := pg.ListMCP()
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	if ms == nil {
		ms = []*db.MCPServer{}
	}
	writeJSON(w, 200, map[string]any{"servers": ms})
}

func (s *Server) pgSaveMCP(w http.ResponseWriter, r *http.Request) {
	pg := s.pg(w)
	if pg == nil {
		return
	}
	var m db.MCPServer
	if err := decode(r, &m); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	isNew := m.ID == 0
	id, err := pg.SaveMCP(&m)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	// On initial add, auto-discover + cache the tool list so the UI shows it right
	// away (bounded so a slow/broken server can't hang the request). Skipped on
	// plain updates (e.g. enable toggles) to avoid re-spawning the server each time.
	if isNew && m.Enabled {
		m.ID = id
		ctx, cancel := context.WithTimeout(r.Context(), 45*time.Second)
		if derr := s.discoverAndCacheMCP(ctx, &m); derr != nil {
			log.Printf("[mcp] %s 添加后工具发现失败: %v", m.Name, derr)
		}
		cancel()
	}
	writeJSON(w, 200, map[string]any{"id": id})
}

func (s *Server) pgDeleteMCP(w http.ResponseWriter, r *http.Request) {
	pg := s.pg(w)
	if pg == nil {
		return
	}
	id, _ := pathInt(r, "id")
	if err := pg.DeleteMCP(id); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"deleted": id})
}

// pgRefreshMCP re-discovers one MCP's tools on demand and re-caches them, so a
// config change or an earlier discovery failure can be fixed without a restart.
func (s *Server) pgRefreshMCP(w http.ResponseWriter, r *http.Request) {
	pg := s.pg(w)
	if pg == nil {
		return
	}
	id, _ := pathInt(r, "id")
	all, err := pg.ListMCP()
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	var target *db.MCPServer
	for _, m := range all {
		if m.ID == id {
			target = m
			break
		}
	}
	if target == nil {
		writeErr(w, 404, "MCP 不存在")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 45*time.Second)
	defer cancel()
	if err := s.discoverAndCacheMCP(ctx, target); err != nil {
		writeErr(w, 502, "工具发现失败："+err.Error())
		return
	}
	tools, _ := pg.MCPToolsDetailed(id)
	writeJSON(w, 200, map[string]any{"tools": tools})
}

// pgMCPTools returns one MCP's cached tools (name + description) for the detail UI.
func (s *Server) pgMCPTools(w http.ResponseWriter, r *http.Request) {
	pg := s.pg(w)
	if pg == nil {
		return
	}
	id, _ := pathInt(r, "id")
	tools, err := pg.MCPToolsDetailed(id)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	if tools == nil {
		tools = []db.MCPTool{}
	}
	writeJSON(w, 200, map[string]any{"tools": tools})
}

func (s *Server) pgResourceVisibility(w http.ResponseWriter, r *http.Request) {
	pg := s.pg(w)
	if pg == nil {
		return
	}
	id, _ := pathInt(r, "id")
	agents, err := pg.ResourceAgents(r.PathValue("kind"), id)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"agents": idStrings(agents)})
}

func (s *Server) pgToggleVisibility(w http.ResponseWriter, r *http.Request) {
	pg := s.pg(w)
	if pg == nil {
		return
	}
	var body struct {
		AgentID    int64  `json:"agent_id,string"`
		Kind       string `json:"kind"`
		ResourceID int64  `json:"resource_id"`
		Visible    bool   `json:"visible"`
	}
	if err := decode(r, &body); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	if err := pg.ToggleVisibility(body.AgentID, body.Kind, body.ResourceID, body.Visible); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true})
}
