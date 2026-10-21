package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/Autumn-27/artex/db"
	"github.com/Autumn-27/artex/mcphttp"
)

// 资产同步（ScopeSentry 数据源）。
//
// ScopeSentry 是一个 ASM 资产测绘平台，通过其 MCP 接口按【项目】/【任务】两个维度
// 拉取子域名、Web 应用、服务等资产，映射为 ARTEX 的公司 + 资产模型。数据源本身是
// 一个名为 "ScopeSentry" 的 http 传输 MCP 行（url + X-API-Key 头保存在 mcp_servers）。
//
// 与 agent 工具层不同，这里用 mcphttp.Client.Call 直接调用 MCP 工具、拿原始 JSON，
// 不触发 AskUser 权限弹窗（后台批量同步）。

const (
	scopeSentryMCPName = "ScopeSentry"
	syncMaxPerType     = 5000 // 单目标单类型的入库保护上限
	syncDefaultPage    = 100
)

// findMCPByName returns the MCP server row with the given name, or nil.
func (s *Server) findMCPByName(name string) (*db.MCPServer, error) {
	all, err := s.m.pg.ListMCP()
	if err != nil {
		return nil, err
	}
	for _, m := range all {
		if m.Name == name {
			return m, nil
		}
	}
	return nil, nil
}

// scopeSentryClient dials the configured ScopeSentry MCP (http transport). The env
// map doubles as HTTP headers (X-API-Key). Callers must Close the client.
func (s *Server) scopeSentryClient(ctx context.Context) (*mcphttp.Client, error) {
	m, err := s.findMCPByName(scopeSentryMCPName)
	if err != nil {
		return nil, err
	}
	if m == nil {
		return nil, fmt.Errorf("数据源 %s 不存在，请先创建", scopeSentryMCPName)
	}
	if m.URL == "" {
		return nil, fmt.Errorf("数据源 %s 未配置 URL，请先配置", scopeSentryMCPName)
	}
	return mcphttp.New(ctx, m.Name, m.URL, jsonStrMap(m.Env), m.Insecure)
}

// envHasValue reports whether the env/header map has any non-empty value
// (i.e. an API key / Authorization header was filled in).
func envHasValue(raw json.RawMessage) bool {
	for _, v := range jsonStrMap(raw) {
		if strings.TrimSpace(v) != "" {
			return true
		}
	}
	return false
}

// ---------- GET /api/sync/scopesentry/status ----------

func (s *Server) syncSSStatus(w http.ResponseWriter, r *http.Request) {
	if s.pg(w) == nil {
		return
	}
	m, err := s.findMCPByName(scopeSentryMCPName)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	resp := map[string]any{
		"exists":     m != nil,
		"configured": false,
		"enabled":    false,
		"reachable":  false,
		"tools":      []string{},
	}
	if m == nil {
		writeJSON(w, 200, resp)
		return
	}
	configured := m.URL != "" && envHasValue(m.Env)
	resp["configured"] = configured
	resp["enabled"] = m.Enabled
	resp["url"] = m.URL
	if m.Tools != nil {
		resp["tools"] = m.Tools
	}
	// Light reachability probe only when it can actually connect.
	if configured && m.Enabled {
		ctx, cancel := context.WithTimeout(r.Context(), 12*time.Second)
		defer cancel()
		if cl, cerr := mcphttp.New(ctx, m.Name, m.URL, jsonStrMap(m.Env), m.Insecure); cerr == nil {
			if _, terr := cl.Tools(ctx); terr == nil {
				resp["reachable"] = true
			}
			_ = cl.Close()
		}
	}
	writeJSON(w, 200, resp)
}

// ---------- POST /api/sync/scopesentry/datasource ----------
// Creates the placeholder row if missing, and/or sets URL + API key and enables it.

func (s *Server) syncSSDatasource(w http.ResponseWriter, r *http.Request) {
	pg := s.pg(w)
	if pg == nil {
		return
	}
	var body struct {
		URL    string `json:"url"`
		APIKey string `json:"api_key"`
	}
	_ = decode(r, &body) // empty body = just create the placeholder
	body.URL = strings.TrimSpace(body.URL)
	body.APIKey = strings.TrimSpace(body.APIKey)

	m, err := s.findMCPByName(scopeSentryMCPName)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	if m == nil {
		m = &db.MCPServer{Name: scopeSentryMCPName, Transport: "http", Env: json.RawMessage(`{"X-API-Key":""}`)}
	}
	if body.URL != "" {
		m.URL = body.URL
	}
	if body.APIKey != "" {
		env, _ := json.Marshal(map[string]string{"X-API-Key": body.APIKey})
		m.Env = env
	}
	// Enable only once it can actually be used.
	m.Enabled = m.URL != "" && envHasValue(m.Env)

	id, err := pg.SaveMCP(m)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	m.ID = id
	// Best-effort tool discovery so the status card shows tools right away.
	if m.Enabled {
		ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
		_ = s.discoverAndCacheMCP(ctx, m)
		cancel()
	}
	writeJSON(w, 200, map[string]any{"id": id, "enabled": m.Enabled})
}

// ---------- GET /api/sync/scopesentry/projects ----------

func (s *Server) syncSSProjects(w http.ResponseWriter, r *http.Request) {
	if s.pg(w) == nil {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()
	cl, err := s.scopeSentryClient(ctx)
	if err != nil {
		writeErr(w, 502, err.Error())
		return
	}
	defer cl.Close()

	args := map[string]any{
		"pageIndex": queryInt(r, "page", 1),
		"pageSize":  queryInt(r, "size", 50),
	}
	if q := r.URL.Query().Get("search"); q != "" {
		args["search"] = q
	}
	text, err := cl.Call(ctx, "list_projects_data", args)
	if err != nil {
		writeErr(w, 502, "list_projects_data 失败: "+err.Error())
		return
	}
	// {result:{All:[{id,name,logo,AssetCount,tag}], <tag>:[...]}, tag:{...}}
	var env struct {
		Result map[string]json.RawMessage `json:"result"`
		Tag    map[string]int             `json:"tag"`
	}
	if err := json.Unmarshal([]byte(text), &env); err != nil {
		writeErr(w, 502, "解析项目列表失败: "+err.Error())
		return
	}
	projects := json.RawMessage("[]")
	if raw, ok := env.Result["All"]; ok && len(raw) > 0 {
		projects = raw
	}
	writeJSON(w, 200, map[string]any{"projects": projects, "tag": env.Tag})
}

// ---------- GET /api/sync/scopesentry/tasks ----------

func (s *Server) syncSSTasks(w http.ResponseWriter, r *http.Request) {
	if s.pg(w) == nil {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()
	cl, err := s.scopeSentryClient(ctx)
	if err != nil {
		writeErr(w, 502, err.Error())
		return
	}
	defer cl.Close()

	args := map[string]any{
		"pageIndex": queryInt(r, "page", 1),
		"pageSize":  queryInt(r, "size", 50),
	}
	if q := r.URL.Query().Get("search"); q != "" {
		args["search"] = q
	}
	text, err := cl.Call(ctx, "list_tasks", args)
	if err != nil {
		writeErr(w, 502, "list_tasks 失败: "+err.Error())
		return
	}
	var env struct {
		List json.RawMessage `json:"list"`
	}
	if err := json.Unmarshal([]byte(text), &env); err != nil {
		writeErr(w, 502, "解析任务列表失败: "+err.Error())
		return
	}
	tasks := env.List
	if len(tasks) == 0 {
		tasks = json.RawMessage("[]")
	}
	writeJSON(w, 200, map[string]any{"tasks": tasks})
}

// ssProjectMeta fetches a project's display name and its target root domains
// (get_project.target is a newline-separated list) for company + scope creation.
func (s *Server) ssProjectMeta(ctx context.Context, cl ssToolCaller, projectID string) (name string, roots []string, err error) {
	text, err := cl.Call(ctx, "get_project", map[string]any{"id": projectID})
	if err != nil {
		return "", nil, err
	}
	var p struct {
		Name   string `json:"name"`
		Target string `json:"target"`
	}
	if err := json.Unmarshal([]byte(text), &p); err != nil {
		return "", nil, err
	}
	for _, line := range strings.Split(p.Target, "\n") {
		if t := strings.TrimSpace(line); t != "" {
			roots = append(roots, t)
		}
	}
	return p.Name, roots, nil
}

// queryInt reads an int query param with a default fallback.
func queryInt(r *http.Request, key string, def int) int {
	if v := r.URL.Query().Get(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n
		}
	}
	return def
}
