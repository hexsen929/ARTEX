package server

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/Autumn-27/artex/agent"
	"github.com/Autumn-27/artex/db"
	"github.com/Autumn-27/artex/session"
	actool "github.com/Autumn-27/norma/tool"
	"net/http"
	"strconv"
	"strings"
	"time"
)

func (s *Server) registerWebshellRoutes(mux *http.ServeMux) {
	mux.HandleFunc("POST /api/sessions/{id}/fileop", s.operatorFileOp)
	mux.HandleFunc("POST /api/sessions/{id}/eval", s.operatorWebshellEval)
	mux.HandleFunc("GET /api/sessions/{id}/download", s.operatorSessionDownload)
	mux.HandleFunc("GET /api/webshell", s.operatorWebshellList)
	mux.HandleFunc("POST /api/webshell", s.operatorWebshellSave)
	mux.HandleFunc("PUT /api/webshell/{id}", s.operatorWebshellSave)
	mux.HandleFunc("DELETE /api/webshell/{id}", s.intranetDeleteSession)
	mux.HandleFunc("POST /api/webshell/{id}/test", s.intranetProbeSession)
	mux.HandleFunc("POST /api/webshell/{id}/exec", s.operatorWebshellExec)
	mux.HandleFunc("POST /api/webshell/{id}/eval", s.operatorWebshellEval)
	mux.HandleFunc("GET /api/webshell/{id}/history", s.operatorWebshellHistory)
	mux.HandleFunc("DELETE /api/webshell/{id}/history", s.operatorWebshellClear)
	mux.HandleFunc("POST /api/webshell/{id}/chat", s.operatorWebshellChat)
	mux.HandleFunc("POST /api/webshell/{id}/register-asset", s.operatorWebshellAsset)
}
func (s *Server) operatorWebshellList(w http.ResponseWriter, r *http.Request) {
	if !s.intranetSessionsReady(w) {
		return
	}
	recs, err := s.sessStore.List(r.Context(), "")
	if err != nil {
		operatorError(w, err)
		return
	}
	items := []map[string]any{}
	for _, rec := range recs {
		if !strings.HasPrefix(rec.Kind, "http_") {
			continue
		}
		v := sessionItem(rec)
		meta, e := s.m.pg.WebshellMetadata(r.Context(), rec.ID)
		if e != nil {
			operatorError(w, e)
			return
		}
		for k, x := range meta {
			v[k] = x
		}
		v["has_secret"] = len(rec.Secret) > 0
		items = append(items, v)
	}
	writeJSON(w, 200, map[string]any{"items": items})
}
func (s *Server) operatorWebshellSave(w http.ResponseWriter, r *http.Request) {
	if !s.intranetSessionsReady(w) {
		return
	}
	var req struct {
		URL      string `json:"url"`
		Name     string `json:"name"`
		Remark   string `json:"remark"`
		Lang     string `json:"lang"`
		Password string `json:"password"`
		Secret   string `json:"secret"`
		Method   string `json:"method"`
		Param    string `json:"cmd_param"`
		Encoding string `json:"encoding"`
		TargetOS string `json:"target_os"`
		TaskID   int64  `json:"task_id"`
		Probe    *bool  `json:"probe"`
	}
	if !operatorDecode(w, r, &req) {
		return
	}
	if strings.TrimSpace(req.URL) == "" {
		writeErr(w, 400, "url required")
		return
	}
	opts := session.HTTPOptions{Method: req.Method, CommandParam: req.Param, Encoding: req.Encoding, TargetOS: req.TargetOS}
	if err := opts.Validate(); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	id := int64(0)
	var old *db.SessionRecord
	var err error
	if r.PathValue("id") != "" {
		id, err = pathID(r, "id")
		if err == nil {
			old, err = s.sessStore.Get(r.Context(), id)
		}
		if err != nil {
			operatorError(w, err)
			return
		}
		done, e := s.beginIntranetSessionOperation(r.Context(), id)
		if e != nil {
			writeErr(w, 409, e.Error())
			return
		}
		defer done()
		if req.Password == "" && req.Secret == "" {
			var sec session.Secret
			if json.Unmarshal(old.Secret, &sec) == nil {
				req.Password = sec.Password
				req.Secret = sec.Key
			}
		}
	}
	if req.TaskID > 0 {
		if _, ok := s.m.Task(strconv.FormatInt(req.TaskID, 10)); !ok {
			writeErr(w, 400, "task not found")
			return
		}
	}
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	var sh *session.HTTPShell
	status := db.SessionDead
	probe := req.Probe == nil || *req.Probe
	if req.Lang == "phpenc" || req.Lang == "jspenc" {
		if req.Method != "" && req.Method != "POST" {
			writeErr(w, 400, "encrypted shells require POST")
			return
		}
		sh, err = session.NewEncShell(req.URL, req.Lang, req.Secret)
		if err == nil && probe {
			err = sh.Test(ctx)
		}
	} else if probe {
		sh, _, err = session.Probe(ctx, req.URL, req.Password, req.Lang, opts)
	} else {
		sh, err = session.NewHTTPShell(req.URL, req.Password, req.Lang, opts)
	}
	if err != nil {
		writeErr(w, 502, err.Error())
		return
	}
	if probe {
		status = db.SessionAlive
	}
	secret, _ := json.Marshal(sh.Secret())
	rec := &db.SessionRecord{ID: id, Kind: sh.Kind(), URL: req.URL, Lang: sh.Lang(), Secret: secret, Status: status, CreatedByTask: req.TaskID}
	if id == 0 {
		id, err = s.sessStore.Create(ctx, rec)
	} else {
		err = s.sessStore.UpdateConnection(ctx, rec)
	}
	if err != nil {
		operatorError(w, err)
		return
	}
	method := strings.ToUpper(req.Method)
	if method == "" {
		method = "POST"
	}
	err = s.m.pg.SaveWebshellMetadata(ctx, id, map[string]any{"name": req.Name, "remark": req.Remark, "method": method, "cmd_param": req.Param, "encoding": req.Encoding, "target_os": req.TargetOS})
	if err != nil {
		operatorError(w, err)
		return
	}
	if old != nil && req.TaskID > 0 && old.CreatedByTask != req.TaskID {
		if err := s.sessStore.BindTask(ctx, id, req.TaskID); err != nil {
			operatorError(w, err)
			return
		}
	}
	sh.SetID(id)
	s.sessReg.Add(sh)
	s.auditIntranet(r, "webshell save id=%d probe=%v", id, probe)
	writeJSON(w, 200, map[string]any{"id": id, "status": status, "probed": probe})
}
func (s *Server) operatorWebshellExec(w http.ResponseWriter, r *http.Request) {
	if !s.intranetSessionsReady(w) {
		return
	}
	id, err := pathID(r, "id")
	if err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	var v struct {
		Command string `json:"command"`
		Timeout int    `json:"timeout"`
	}
	if !operatorDecode(w, r, &v) {
		return
	}
	if strings.TrimSpace(v.Command) == "" || len(v.Command) > 32<<10 {
		writeErr(w, 400, "command required; limit 32 KiB")
		return
	}
	done, err := s.beginIntranetSessionOperation(r.Context(), id)
	if err != nil {
		writeErr(w, 409, err.Error())
		return
	}
	defer done()
	sh, err := s.lookupLiveSession(id)
	if err != nil {
		operatorError(w, err)
		return
	}
	if v.Timeout <= 0 || v.Timeout > 120 {
		v.Timeout = 30
	}
	start := time.Now()
	out, se, execErr := sh.Exec(r.Context(), v.Command, time.Duration(v.Timeout)*time.Second)
	ms := time.Since(start).Milliseconds()
	out = truncateOutput(out)
	se = truncateOutput(se)
	_ = s.m.pg.RecordWebshellCommand(r.Context(), id, v.Command, out, se, ms, execErr == nil)
	result := map[string]any{"stdout": out, "stderr": se, "ms": ms, "ok": execErr == nil}
	if execErr != nil {
		result["error"] = execErr.Error()
	}
	s.auditIntranet(r, "webshell exec id=%d ok=%v ms=%d", id, execErr == nil, ms)
	writeJSON(w, 200, result)
}
func (s *Server) operatorWebshellEval(w http.ResponseWriter, r *http.Request) {
	if !s.intranetSessionsReady(w) {
		return
	}
	id, err := pathID(r, "id")
	if err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	var v struct {
		Code string `json:"code"`
	}
	if !operatorDecode(w, r, &v) {
		return
	}
	done, err := s.beginIntranetSessionOperation(r.Context(), id)
	if err != nil {
		writeErr(w, 409, err.Error())
		return
	}
	defer done()
	ss, err := s.lookupLiveSession(id)
	if err != nil {
		operatorError(w, err)
		return
	}
	sh, ok := ss.(*session.HTTPShell)
	if !ok {
		writeErr(w, 400, "HTTP session required")
		return
	}
	out, err := sh.Eval(r.Context(), v.Code, 30*time.Second)
	if err != nil {
		writeErr(w, 502, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"stdout": truncateOutput(out)})
}
func (s *Server) operatorWebshellHistory(w http.ResponseWriter, r *http.Request) {
	if s.pg(w) == nil {
		return
	}
	id, err := pathID(r, "id")
	if err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	rows, err := s.m.pg.QueryContext(r.Context(), `SELECT id,command,stdout,stderr,ms,ok,created_at FROM webshell_commands WHERE session_id=$1 ORDER BY id DESC LIMIT 100`, id)
	if err != nil {
		operatorError(w, err)
		return
	}
	defer rows.Close()
	items := []map[string]any{}
	for rows.Next() {
		var xid, ms int64
		var cmd, out, se string
		var ok bool
		var at time.Time
		if err = rows.Scan(&xid, &cmd, &out, &se, &ms, &ok, &at); err != nil {
			operatorError(w, err)
			return
		}
		items = append(items, map[string]any{"id": xid, "command": cmd, "stdout": out, "stderr": se, "ms": ms, "ok": ok, "created_at": at})
	}
	if err = rows.Err(); err != nil {
		operatorError(w, err)
		return
	}
	writeJSON(w, 200, map[string]any{"items": items})
}
func (s *Server) operatorWebshellClear(w http.ResponseWriter, r *http.Request) {
	if s.pg(w) == nil {
		return
	}
	id, err := pathID(r, "id")
	if err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	_, err = s.m.pg.ExecContext(r.Context(), "DELETE FROM webshell_commands WHERE session_id=$1", id)
	if err != nil {
		operatorError(w, err)
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true})
}
func (s *Server) operatorWebshellChat(w http.ResponseWriter, r *http.Request) {
	if !s.intranetSessionsReady(w) {
		return
	}
	id, err := pathID(r, "id")
	if err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	var req struct {
		TaskID int64  `json:"task_id"`
		Agent  string `json:"agent_key"`
	}
	if !operatorDecode(w, r, &req) {
		return
	}
	rec, err := s.sessStore.Get(r.Context(), id)
	if err != nil {
		operatorError(w, err)
		return
	}
	if req.TaskID == 0 {
		req.TaskID = rec.CreatedByTask
	}
	if req.TaskID <= 0 {
		writeErr(w, 409, "select an explicit task for agent control")
		return
	}
	if _, ok := s.m.Task(strconv.FormatInt(req.TaskID, 10)); !ok {
		writeErr(w, 404, "task not found")
		return
	}
	if req.Agent == "" {
		req.Agent = "pentest"
	}
	if !operatorExecutionRole(req.Agent) {
		writeErr(w, 400, "execution role required")
		return
	}
	if req.TaskID != rec.CreatedByTask {
		if err = s.sessStore.BindTask(r.Context(), id, req.TaskID); err != nil {
			operatorError(w, err)
			return
		}
	}
	c, err := s.m.pg.CreateConversation(req.Agent, fmt.Sprintf("WebShell #%d", id), nil)
	if err != nil {
		operatorError(w, err)
		return
	}
	if err = s.m.pg.SaveOperatorSetting(r.Context(), fmt.Sprintf("conversation.webshell.%d", c.ID), map[string]int64{"session_id": id, "task_id": req.TaskID}); err != nil {
		operatorError(w, err)
		return
	}
	s.auditIntranet(r, "webshell conversation session=%d task=%d agent=%s", id, req.TaskID, req.Agent)
	writeJSON(w, 200, map[string]any{"conversation_id": c.ID, "session_id": id, "task_id": req.TaskID})
}
func (s *Server) operatorWebshellAsset(w http.ResponseWriter, r *http.Request) {
	if !s.intranetSessionsReady(w) {
		return
	}
	id, err := pathID(r, "id")
	if err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	var asset int64
	err = s.m.pg.QueryRowContext(r.Context(), "SELECT COALESCE(host_asset_id,0) FROM sessions WHERE id=$1", id).Scan(&asset)
	if err != nil {
		operatorError(w, err)
		return
	}
	if asset == 0 {
		rec, e := s.sessStore.Get(r.Context(), id)
		if e != nil {
			operatorError(w, e)
			return
		}
		asset, e = s.m.Assets().UpsertEndpoint(db.UpsertEndpointReq{URL: rec.URL, Method: "POST", TaskID: rec.CreatedByTask})
		if e != nil {
			operatorError(w, e)
			return
		}
		if _, e = s.m.pg.ExecContext(r.Context(), "UPDATE sessions SET host_asset_id=$2 WHERE id=$1", id, asset); e != nil {
			operatorError(w, e)
			return
		}
	}

	writeJSON(w, 200, map[string]any{"asset_id": asset})
}
func (s *Server) operatorSessionTools() []actool.CoreTool {
	return []actool.CoreTool{
		actool.Build(actool.Spec{Name: "list_sessions", Description: "列出当前任务可控制的会话；不暴露连接密码。", Schema: operatorSchema("list_sessions", map[string]any{}), ReadOnly: func(json.RawMessage) bool { return true }, Run: func(ctx context.Context, b json.RawMessage, _ *actool.ToolContext) (actool.Result, error) {
			id := agent.RunInfoFrom(ctx).TaskID
			if id <= 0 {
				return actool.Errorf("task context required"), nil
			}
			rs, err := s.sessStore.ListForTask(ctx, id, "")
			if err != nil {
				return actool.Errorf(err.Error()), nil
			}
			items := []map[string]any{}
			for _, r := range rs {
				items = append(items, sessionItem(r))
			}
			out, _ := json.Marshal(items)
			return actool.Text(string(out)), nil
		}}),
		actool.Build(actool.Spec{Name: "session_eval", Description: "通过当前任务绑定的 PHP 会话执行原生 PHP 片段。使用已有任务会话授权，不创建任意远程连接。", Schema: operatorSchema("session_eval", map[string]any{}), Permissions: sessionPerm, ReadOnly: func(json.RawMessage) bool { return false }, Run: func(ctx context.Context, b json.RawMessage, _ *actool.ToolContext) (actool.Result, error) {
			var v struct {
				ID      int64  `json:"session_id"`
				Code    string `json:"code"`
				Timeout int    `json:"timeout"`
			}
			if json.Unmarshal(b, &v) != nil {
				return actool.Errorf("invalid input"), nil
			}
			done, err := s.beginIntranetTaskOperation(agent.RunInfoFrom(ctx).TaskID)
			if err != nil {
				return actool.Errorf(err.Error()), nil
			}
			defer done()
			ss, err := s.lookupTaskSession(ctx, v.ID)
			if err != nil {
				return actool.Errorf(err.Error()), nil
			}
			sh, ok := ss.(*session.HTTPShell)
			if !ok {
				return actool.Errorf("PHP HTTP session required"), nil
			}
			timeout := v.Timeout
			if timeout <= 0 {
				timeout = 15
			}
			if timeout > 300 {
				timeout = 300
			}
			out, err := sh.Eval(ctx, v.Code, time.Duration(timeout)*time.Second)
			if err != nil {
				return actool.Errorf(err.Error()), nil
			}
			return actool.Text(truncateOutput(out)), nil
		}}),
	}
}
