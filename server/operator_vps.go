package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/Autumn-27/artex/agent"
	actool "github.com/Autumn-27/norma/tool"
	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"
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

// The original VPS client uses its own TOFU file, never ~/.ssh/known_hosts.
// Serialize check+append so concurrent first connections cannot trust two keys.
var operatorKnownHostsMu sync.Mutex

func operatorVPSHostKeyCallback(path, configuredHost string, pin map[string]string) ssh.HostKeyCallback {
	return func(host string, remote net.Addr, key ssh.PublicKey) error {
		if pin["key"] != "" {
			if pin["host"] != configuredHost || pin["key"] != ssh.FingerprintSHA256(key) {
				return fmt.Errorf("SSH host key differs from explicit pin: %s", ssh.FingerprintSHA256(key))
			}
			return nil
		}
		operatorKnownHostsMu.Lock()
		defer operatorKnownHostsMu.Unlock()
		check, err := knownhosts.New(path)
		if err == nil {
			err = check(host, remote, key)
			if err == nil {
				return nil
			}
			var unknown *knownhosts.KeyError
			if !errors.As(err, &unknown) || len(unknown.Want) != 0 {
				return fmt.Errorf("SSH host key mismatch for %s in %s: %w", host, path, err)
			}
		} else if !errors.Is(err, os.ErrNotExist) {
			return err
		}
		if err = os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			return err
		}
		f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
		if err != nil {
			return err
		}
		_, err = fmt.Fprintln(f, knownhosts.Line([]string{knownhosts.Normalize(host)}, key))
		closeErr := f.Close()
		if err != nil {
			return err
		}
		return closeErr
	}
}

type operatorVPS struct {
	ID                           int64     `json:"id"`
	Kind                         string    `json:"kind"`
	Label                        string    `json:"label"`
	Host                         string    `json:"host"`
	Port                         int       `json:"port"`
	User                         string    `json:"user"`
	Note                         string    `json:"note"`
	Enabled                      bool      `json:"enabled"`
	PreferCallback               bool      `json:"prefer_callback"`
	PreferEgress                 bool      `json:"prefer_egress"`
	SocksUser                    string    `json:"socks_user"`
	HasKey                       bool      `json:"has_key"`
	HasPassword                  bool      `json:"has_password"`
	HasSocksPassword             bool      `json:"has_socks_password"`
	LastCheckOK                  bool      `json:"last_check_ok"`
	LastCheckMessage             string    `json:"last_check_msg"`
	LastCheckAt                  time.Time `json:"last_check_at"`
	key, password, socksPassword string
}

const vpsColumns = `id,kind,label,host,port,user_name,note,enabled,prefer_callback,prefer_egress,socks_user,ssh_key,ssh_password,socks_password,last_check_at,last_check_ok,last_check_msg`

func scanOperatorVPS(row interface{ Scan(...any) error }) (operatorVPS, error) {
	var v operatorVPS
	err := row.Scan(&v.ID, &v.Kind, &v.Label, &v.Host, &v.Port, &v.User, &v.Note, &v.Enabled, &v.PreferCallback, &v.PreferEgress, &v.SocksUser, &v.key, &v.password, &v.socksPassword, &v.LastCheckAt, &v.LastCheckOK, &v.LastCheckMessage)
	v.HasKey = v.key != ""
	v.HasPassword = v.password != ""
	v.HasSocksPassword = v.socksPassword != ""
	return v, err
}
func (s *Server) registerVPSRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/vps", s.operatorVPSList)
	mux.HandleFunc("POST /api/vps", s.operatorVPSSave)
	mux.HandleFunc("PUT /api/vps/{id}", s.operatorVPSSave)
	mux.HandleFunc("DELETE /api/vps/{id}", s.operatorVPSDelete)
	mux.HandleFunc("POST /api/vps/{id}/check", s.operatorVPSCheck)
	mux.HandleFunc("POST /api/vps/{id}/exec", s.operatorVPSExec)
	mux.HandleFunc("POST /api/vps/{id}/apply", s.operatorVPSApply)
}
func (s *Server) operatorVPSList(w http.ResponseWriter, r *http.Request) {
	if s.pg(w) == nil {
		return
	}
	rows, err := s.m.pg.QueryContext(r.Context(), "SELECT "+vpsColumns+" FROM vps_hosts ORDER BY prefer_callback DESC,id")
	if err != nil {
		operatorError(w, err)
		return
	}
	defer rows.Close()
	items := []operatorVPS{}
	callback := ""
	for rows.Next() {
		v, e := scanOperatorVPS(rows)
		if e != nil {
			operatorError(w, e)
			return
		}
		items = append(items, v)
		if callback == "" && v.Enabled && v.PreferCallback {
			callback = v.Host
		}
	}
	if err = rows.Err(); err != nil {
		operatorError(w, err)
		return
	}
	writeJSON(w, 200, map[string]any{"vps": items, "callback_host": callback})
}
func (s *Server) operatorVPSSave(w http.ResponseWriter, r *http.Request) {
	if s.pg(w) == nil {
		return
	}
	var req struct {
		Kind           string  `json:"kind"`
		Label          string  `json:"label"`
		Host           string  `json:"host"`
		Port           int     `json:"port"`
		User           string  `json:"user"`
		Note           string  `json:"note"`
		Enabled        *bool   `json:"enabled"`
		PreferCallback bool    `json:"prefer_callback"`
		PreferEgress   bool    `json:"prefer_egress"`
		SocksUser      string  `json:"socks_user"`
		Key            *string `json:"ssh_key"`
		Password       *string `json:"ssh_password"`
		SocksPassword  *string `json:"socks_password"`
		HostKey        string  `json:"host_key_sha256"`
	}
	if !operatorDecode(w, r, &req) {
		return
	}
	req.Host = strings.TrimSpace(req.Host)
	if req.Host == "" || strings.ContainsAny(req.Host, "/\\@\r\n\t ") || len(req.Host) > 253 {
		writeErr(w, 400, "valid host required")
		return
	}
	if req.Port == 0 {
		req.Port = 22
	}
	if req.Port < 1 || req.Port > 65535 {
		writeErr(w, 400, "invalid port")
		return
	}
	if req.User == "" {
		req.User = "root"
	}
	if req.Label == "" {
		req.Label = req.Host
	}
	if req.Kind == "" {
		req.Kind = "ssh"
	}
	if req.Kind != "ssh" && req.Kind != "socks5" {
		writeErr(w, 400, "kind must be ssh or socks5")
		return
	}
	id := int64(0)
	var old operatorVPS
	var err error
	if r.PathValue("id") != "" {
		id, err = pathID(r, "id")
		if err == nil {
			old, err = scanOperatorVPS(s.m.pg.QueryRowContext(r.Context(), "SELECT "+vpsColumns+" FROM vps_hosts WHERE id=$1", id))
		}
		if err != nil {
			operatorError(w, err)
			return
		}
	}
	enc := func(v *string, previous string) (string, error) {
		if v == nil {
			return previous, nil
		}
		if *v == "" {
			return "", nil
		}
		if len(*v) > 64<<10 {
			return "", fmt.Errorf("secret too large")
		}
		return s.m.pg.OperatorSeal(s.jwtKey, []byte(*v))
	}
	k, err := enc(req.Key, old.key)
	if err != nil {
		operatorError(w, err)
		return
	}
	p, err := enc(req.Password, old.password)
	if err != nil {
		operatorError(w, err)
		return
	}
	sp, err := enc(req.SocksPassword, old.socksPassword)
	if err != nil {
		operatorError(w, err)
		return
	}
	enabled := true
	if req.Enabled != nil {
		enabled = *req.Enabled
	} else if id > 0 {
		enabled = old.Enabled
	}
	if id == 0 {
		err = s.m.pg.QueryRowContext(r.Context(), `INSERT INTO vps_hosts(kind,label,host,port,user_name,note,enabled,prefer_callback,prefer_egress,socks_user,ssh_key,ssh_password,socks_password) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13) RETURNING id`, req.Kind, req.Label, req.Host, req.Port, req.User, req.Note, enabled, req.PreferCallback, req.PreferEgress, req.SocksUser, k, p, sp).Scan(&id)
	} else {
		_, err = s.m.pg.ExecContext(r.Context(), `UPDATE vps_hosts SET kind=$2,label=$3,host=$4,port=$5,user_name=$6,note=$7,enabled=$8,prefer_callback=$9,prefer_egress=$10,socks_user=$11,ssh_key=$12,ssh_password=$13,socks_password=$14,updated_at=now() WHERE id=$1`, id, req.Kind, req.Label, req.Host, req.Port, req.User, req.Note, enabled, req.PreferCallback, req.PreferEgress, req.SocksUser, k, p, sp)
	}
	if err != nil {
		operatorError(w, err)
		return
	}
	if req.HostKey != "" {
		if !strings.HasPrefix(req.HostKey, "SHA256:") {
			writeErr(w, 400, "host_key_sha256 must be an SSH SHA256 fingerprint")
			return
		}
		if err = s.m.pg.SaveOperatorSetting(r.Context(), fmt.Sprintf("vps.hostkey.%d", id), map[string]string{"host": req.Host, "key": req.HostKey}); err != nil {
			operatorError(w, err)
			return
		}
	}
	v, err := scanOperatorVPS(s.m.pg.QueryRowContext(r.Context(), "SELECT "+vpsColumns+" FROM vps_hosts WHERE id=$1", id))
	if err != nil {
		operatorError(w, err)
		return
	}
	writeJSON(w, 200, map[string]any{"vps": v})
}
func (s *Server) operatorVPSDelete(w http.ResponseWriter, r *http.Request) {
	if s.pg(w) == nil {
		return
	}
	id, err := pathID(r, "id")
	if err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	_, err = s.m.pg.ExecContext(r.Context(), "DELETE FROM vps_hosts WHERE id=$1", id)
	if err != nil {
		operatorError(w, err)
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true})
}

type boundedOperatorOutput struct {
	b         bytes.Buffer
	remaining int
	truncated bool
}

func (w *boundedOperatorOutput) Write(p []byte) (int, error) {
	n := len(p)
	if len(p) > w.remaining {
		p = p[:w.remaining]
		w.truncated = true
	}
	w.b.Write(p)
	w.remaining -= len(p)
	return n, nil
}
func (s *Server) runOperatorVPS(ctx context.Context, id int64, command string) (map[string]any, error) {
	return s.runOperatorVPSTimeout(ctx, id, command, 120*time.Second)
}
func (s *Server) runOperatorVPSTimeout(ctx context.Context, id int64, command string, timeout time.Duration) (map[string]any, error) {
	v, err := scanOperatorVPS(s.m.pg.QueryRowContext(ctx, "SELECT "+vpsColumns+" FROM vps_hosts WHERE id=$1", id))
	if err != nil {
		return nil, err
	}
	if !v.Enabled || v.Kind != "ssh" {
		return nil, fmt.Errorf("enabled SSH VPS required")
	}
	var auth []ssh.AuthMethod
	if v.key != "" {
		b, e := s.m.pg.OperatorOpen(s.jwtKey, v.key)
		if e != nil {
			return nil, e
		}
		signer, e := ssh.ParsePrivateKey(b)
		if e != nil {
			return nil, fmt.Errorf("invalid configured SSH key")
		}
		auth = append(auth, ssh.PublicKeys(signer))
	}
	if v.password != "" {
		b, e := s.m.pg.OperatorOpen(s.jwtKey, v.password)
		if e != nil {
			return nil, e
		}
		auth = append(auth, ssh.Password(string(b)))
	}
	if len(auth) == 0 {
		return nil, fmt.Errorf("SSH credentials not configured")
	}
	var trusted map[string]string
	_ = s.m.pg.OperatorSetting(ctx, fmt.Sprintf("vps.hostkey.%d", id), &trusted)
	cfg := &ssh.ClientConfig{User: v.User, Auth: auth, Timeout: 10 * time.Second, HostKeyCallback: operatorVPSHostKeyCallback(filepath.Join(s.dataDir, "vps_known_hosts"), v.Host, trusted)}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	conn, err := (&net.Dialer{Timeout: 10 * time.Second}).DialContext(ctx, "tcp", net.JoinHostPort(v.Host, strconv.Itoa(v.Port)))
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(timeout))
	go func() { <-ctx.Done(); conn.Close() }()
	cc, ch, rq, err := ssh.NewClientConn(conn, net.JoinHostPort(v.Host, strconv.Itoa(v.Port)), cfg)
	if err != nil {
		return nil, err
	}
	client := ssh.NewClient(cc, ch, rq)
	defer client.Close()
	ss, err := client.NewSession()
	if err != nil {
		return nil, err
	}
	defer ss.Close()
	out, stderr := &boundedOperatorOutput{remaining: 1 << 20}, &boundedOperatorOutput{remaining: 1 << 20}
	ss.Stdout = out
	ss.Stderr = stderr
	start := time.Now()
	err = ss.Run(command)
	result := map[string]any{"stdout": out.b.String(), "stderr": stderr.b.String(), "ms": time.Since(start).Milliseconds(), "truncated": out.truncated || stderr.truncated}
	if err != nil {
		result["error"] = err.Error()
	}
	return result, err
}
func (s *Server) operatorVPSCheck(w http.ResponseWriter, r *http.Request) {
	if s.pg(w) == nil {
		return
	}
	id, err := pathID(r, "id")
	if err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	result, err := s.runOperatorVPS(r.Context(), id, "printf 'ARTEX_VPS_CHECK\\n'")
	msg := "connected"
	if err != nil {
		msg = err.Error()
	}
	_, _ = s.m.pg.ExecContext(r.Context(), "UPDATE vps_hosts SET last_check_at=now(),last_check_ok=$2,last_check_msg=$3 WHERE id=$1", id, err == nil, msg)
	writeJSON(w, 200, map[string]any{"ok": err == nil, "message": msg, "result": result})
}
func (s *Server) operatorVPSExec(w http.ResponseWriter, r *http.Request) {
	if s.pg(w) == nil {
		return
	}
	id, err := pathID(r, "id")
	if err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	var v struct {
		Command string `json:"command"`
	}
	if !operatorDecode(w, r, &v) {
		return
	}
	if v.Command == "" || len(v.Command) > 32<<10 {
		writeErr(w, 400, "command required, limit 32 KiB")
		return
	}
	out, err := s.runOperatorVPS(r.Context(), id, v.Command)
	if err != nil && out == nil {
		writeErr(w, 502, err.Error())
		return
	}
	writeJSON(w, 200, out)
}
func (s *Server) operatorVPSTool() actool.CoreTool {
	return actool.Build(actool.Spec{Name: "vps_exec", Description: "在操作员预先登记并启用的 SSH VPS 上执行命令；使用已固定的主机密钥和加密保存的凭据。仅接受已登记 ID，不能临时指定任意主机。", Schema: operatorSchema("vps_exec", map[string]any{}), Permissions: sessionPerm, ReadOnly: func(json.RawMessage) bool { return false }, Run: func(ctx context.Context, b json.RawMessage, _ *actool.ToolContext) (actool.Result, error) {
		if agent.RunInfoFrom(ctx).TaskID <= 0 {
			return actool.Errorf("task context required"), nil
		}
		var v struct {
			ID        int64  `json:"vps_id"`
			Command   string `json:"command"`
			VPS       string `json:"vps"`
			TimeoutMS int    `json:"timeout_ms"`
		}
		if err := json.Unmarshal(b, &v); err != nil {
			return actool.Errorf("invalid input"), nil
		}
		if v.ID == 0 {
			rows, err := s.m.pg.QueryContext(ctx, "SELECT id FROM vps_hosts WHERE enabled AND kind='ssh' AND ($1='' OR id::text=$1 OR label=$1) ORDER BY id LIMIT 2", v.VPS)
			if err != nil {
				return actool.Errorf(err.Error()), nil
			}
			ids := []int64{}
			for rows.Next() {
				var id int64
				if rows.Scan(&id) == nil {
					ids = append(ids, id)
				}
			}
			rows.Close()
			if len(ids) != 1 {
				return actool.Errorf("choose exactly one registered VPS by id or name"), nil
			}
			v.ID = ids[0]
		}
		if v.Command == "" || len(v.Command) > 32<<10 {
			return actool.Errorf("command required, at most 32 KiB"), nil
		}
		timeout := v.TimeoutMS
		if timeout <= 0 {
			timeout = 60000
		}
		if timeout > 600000 {
			timeout = 600000
		}
		ctx, cancel := context.WithTimeout(ctx, time.Duration(timeout)*time.Millisecond)
		defer cancel()
		out, err := s.runOperatorVPSTimeout(ctx, v.ID, v.Command, time.Duration(timeout)*time.Millisecond)
		if err != nil && out == nil {
			return actool.Errorf(err.Error()), nil
		}
		raw, _ := json.Marshal(out)
		return actool.Text(string(raw)), nil
	}})
}

// Route changes are delegated to the current settings implementation, retaining
// its validation, environment locks, proxy health supervisor and agent rebuilds.
func (s *Server) operatorVPSApply(w http.ResponseWriter, r *http.Request) {
	if s.pg(w) == nil {
		return
	}
	id, err := pathID(r, "id")
	if err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	var req struct {
		Callback bool `json:"callback"`
		Egress   bool `json:"egress"`
	}
	if !operatorDecode(w, r, &req) {
		return
	}
	v, err := scanOperatorVPS(s.m.pg.QueryRowContext(r.Context(), "SELECT "+vpsColumns+" FROM vps_hosts WHERE id=$1", id))
	if err != nil {
		operatorError(w, err)
		return
	}
	if !v.Enabled || !req.Callback && !req.Egress {
		writeErr(w, 400, "select an enabled VPS and an explicit action")
		return
	}
	body := map[string]any{}
	if req.Callback {
		body["callback_addr"] = v.Host
	}
	if req.Egress {
		if v.Kind != "socks5" {
			writeErr(w, 400, "egress requires a socks5 endpoint, not an SSH port")
			return
		}
		u := url.URL{Scheme: "socks5", Host: net.JoinHostPort(v.Host, strconv.Itoa(v.Port))}
		if v.SocksUser != "" {
			password := ""
			if v.socksPassword != "" {
				b, e := s.m.pg.OperatorOpen(s.jwtKey, v.socksPassword)
				if e != nil {
					operatorError(w, e)
					return
				}
				password = string(b)
			}
			u.User = url.UserPassword(v.SocksUser, password)
		}
		body["global_proxy"] = u.String()
		body["global_proxy_enabled"] = true
	}
	b, _ := json.Marshal(body)
	copy := r.Clone(r.Context())
	copy.Method = "PUT"
	copy.Body = io.NopCloser(bytes.NewReader(b))
	copy.ContentLength = int64(len(b))
	s.putSettings(w, copy)
}
