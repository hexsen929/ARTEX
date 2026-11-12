package server

import (
	"encoding/json"
	"fmt"
	"github.com/Autumn-27/artex/session"
	"mime"
	"net/http"
	"path"
	"regexp"
	"strings"
	"time"
)

var operatorFileMode = regexp.MustCompile(`^[0-7]{3,4}$`)

func quoteOperatorPath(s string) string { return "'" + strings.ReplaceAll(s, "'", "'\"'\"'") + "'" }
func (s *Server) operatorFileOp(w http.ResponseWriter, r *http.Request) {
	if !s.intranetSessionsReady(w) {
		return
	}
	id, err := pathID(r, "id")
	if err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	var v struct {
		Op          string `json:"op"`
		Path        string `json:"path"`
		Destination string `json:"destination"`
		Mode        string `json:"mode"`
		Recursive   bool   `json:"recursive"`
	}
	if !operatorDecode(w, r, &v) {
		return
	}
	if v.Path == "" || len(v.Path) > 4000 || strings.ContainsRune(v.Path, 0) {
		writeErr(w, 400, "valid remote path required")
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
	rec, err := s.sessStore.Get(r.Context(), id)
	if err != nil {
		operatorError(w, err)
		return
	}
	var secret session.Secret
	_ = json.Unmarshal(rec.Secret, &secret)
	p := quoteOperatorPath(v.Path)
	dst := quoteOperatorPath(v.Destination)
	cmd := ""
	switch v.Op {
	case "mkdir":
		cmd = "mkdir -- " + p
	case "rename", "copy":
		if v.Destination == "" || len(v.Destination) > 4000 || strings.ContainsRune(v.Destination, 0) {
			writeErr(w, 400, "destination required")
			return
		}
		tool := "mv"
		if v.Op == "copy" {
			tool = "cp"
		}
		cmd = tool + " -- " + p + " " + dst
	case "delete":
		cmd = "rm -f -- " + p
		if v.Recursive {
			cmd = "rm -rf -- " + p
		}
	case "chmod":
		if !operatorFileMode.MatchString(v.Mode) {
			writeErr(w, 400, "octal mode required")
			return
		}
		cmd = "chmod " + v.Mode + " -- " + p
	default:
		writeErr(w, 400, "op must be mkdir,rename,copy,delete,chmod")
		return
	}
	if secret.Platform == "windows" {
		psquote := func(p string) string { return "'" + strings.ReplaceAll(p, "'", "''") + "'" }
		p = psquote(v.Path)
		dst = psquote(v.Destination)
		switch v.Op {
		case "mkdir":
			cmd = "New-Item -ItemType Directory -Path " + p
		case "rename":
			cmd = "Move-Item -LiteralPath " + p + " -Destination " + dst
		case "copy":
			cmd = "Copy-Item -LiteralPath " + p + " -Destination " + dst
		case "delete":
			cmd = "Remove-Item -LiteralPath " + p + " -Force"
			if v.Recursive {
				cmd += " -Recurse"
			}
		case "chmod":
			writeErr(w, 400, "chmod is not a Windows operation")
			return
		}
		cmd = "powershell -NoProfile -NonInteractive -Command " + `"` + strings.ReplaceAll(cmd, `"`, `\"`) + `"`
	}
	out, se, err := sh.Exec(r.Context(), cmd, 30*time.Second)
	s.auditIntranet(r, "fileop id=%d op=%s ok=%v", id, v.Op, err == nil)
	if err != nil {
		writeErr(w, 502, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"stdout": truncateOutput(out), "stderr": truncateOutput(se), "ok": true})
}
func (s *Server) operatorSessionDownload(w http.ResponseWriter, r *http.Request) {
	if !s.intranetSessionsReady(w) {
		return
	}
	id, err := pathID(r, "id")
	if err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	p := r.URL.Query().Get("path")
	if p == "" || len(p) > 4000 {
		writeErr(w, 400, "remote path required")
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
	b, err := sh.ReadFile(r.Context(), p)
	if err != nil {
		writeErr(w, 502, err.Error())
		return
	}
	if len(b) > 8<<20 {
		writeErr(w, 413, "download exceeds 8 MiB")
		return
	}
	name := path.Base(strings.ReplaceAll(p, "\\", "/"))
	w.Header().Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": name}))
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Length", fmt.Sprint(len(b)))
	_, _ = w.Write(b)
}
