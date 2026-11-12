package server

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io/fs"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

type cleanupItem struct {
	Key   string   `json:"key"`
	Label string   `json:"label"`
	Hint  string   `json:"hint"`
	Count int      `json:"count"`
	Bytes int64    `json:"bytes"`
	Paths []string `json:"-"`
}

var cleanupID = regexp.MustCompile(`^(?:conv[-_]?|conversation[-_]?)?([0-9]+)(?:\.[A-Za-z0-9]+)?$`)

func (s *Server) registerCleanupRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/maintenance/cleanup", s.operatorCleanupScan)
	mux.HandleFunc("POST /api/maintenance/cleanup", s.operatorCleanupRun)
}
func cleanupPathSafe(root, p string) bool {
	a, err := filepath.Abs(root)
	if err != nil {
		return false
	}
	b, err := filepath.Abs(p)
	if err != nil {
		return false
	}
	rel, err := filepath.Rel(a, b)
	if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return false
	}
	cur := a
	for _, part := range strings.Split(rel, string(filepath.Separator)) {
		cur = filepath.Join(cur, part)
		fi, err := os.Lstat(cur)
		if err != nil || fi.Mode()&os.ModeSymlink != 0 {
			return false
		}
	}
	return true
}
func cleanupSize(p string) (int64, error) {
	var n int64
	err := filepath.WalkDir(p, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.Type()&os.ModeSymlink != 0 {
			return fmt.Errorf("symlink present; skip")
		}
		if !d.IsDir() {
			fi, e := d.Info()
			if e != nil {
				return e
			}
			n += fi.Size()
		}
		return nil
	})
	return n, err
}
func (s *Server) scanOperatorCleanup(ctx context.Context) ([]cleanupItem, string, error) {
	alive := map[string]bool{}
	rows, err := s.m.pg.QueryContext(ctx, `SELECT 'task:'||id::text FROM tasks UNION ALL SELECT 'conv:'||id::text FROM conversations`)
	if err != nil {
		return nil, "", err
	}
	for rows.Next() {
		var id string
		if err = rows.Scan(&id); err != nil {
			rows.Close()
			return nil, "", err
		}
		alive[id] = true
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, "", err
	}
	defs := []struct {
		key, label, sub, owner string
		stale                  bool
	}{{"orphan_sessions", "孤儿会话目录", "sessions", "conv:", false}, {"orphan_transcripts", "孤儿对话记录", "transcripts", "conv:", false}, {"orphan_task_dirs", "孤儿任务目录", "tasks", "task:", false}, {"orphan_archives", "孤儿任务归档", "archives/tasks", "task:", false}, {"stale_staging", "归档暂存残留", "archives/tasks/.staging", "", true}, {"stale_stage", "文件投递暂存", "stage", "", true}, {"stale_tunnels", "隧道临时文件", "tunnels", "", true}}
	out := []cleanupItem{}
	h := sha256.New()
	for _, d := range defs {
		item := cleanupItem{Key: d.key, Label: d.label, Hint: "仅处理可确认无所属记录、无符号链接且无打开文件的内容；暂存须超过24小时"}
		root := filepath.Join(s.dataDir, d.sub)
		es, e := os.ReadDir(root)
		if os.IsNotExist(e) {
			out = append(out, item)
			continue
		}
		if e != nil {
			return nil, "", e
		}
		for _, de := range es {
			p := filepath.Join(root, de.Name())
			if !cleanupPathSafe(s.dataDir, p) {
				continue
			}
			fi, e := de.Info()
			if e != nil {
				return nil, "", e
			}
			if d.stale {
				if time.Since(fi.ModTime()) < 24*time.Hour {
					continue
				}
			} else {
				m := cleanupID.FindStringSubmatch(de.Name())
				if m == nil || alive[d.owner+m[1]] {
					continue
				}
			}
			n, e := cleanupSize(p)
			if e != nil {
				continue
			}
			item.Paths = append(item.Paths, p)
			item.Count++
			item.Bytes += n
			fmt.Fprintf(h, "%s:%s:%d:%d\n", d.key, p, n, fi.ModTime().UnixNano())
		}
		out = append(out, item)
	}
	return out, hex.EncodeToString(h.Sum(nil)), nil
}
func (s *Server) operatorCleanupScan(w http.ResponseWriter, r *http.Request) {
	if s.pg(w) == nil {
		return
	}
	items, token, err := s.scanOperatorCleanup(r.Context())
	if err != nil {
		operatorError(w, err)
		return
	}
	n, b := 0, int64(0)
	for _, i := range items {
		n += i.Count
		b += i.Bytes
	}
	writeJSON(w, 200, map[string]any{"items": items, "scan_token": token, "total_count": n, "total_bytes": b, "data_dir": s.dataDir})
}
func (s *Server) operatorCleanupRun(w http.ResponseWriter, r *http.Request) {
	if s.pg(w) == nil {
		return
	}
	var v struct {
		Keys      []string `json:"keys"`
		ScanToken string   `json:"scan_token"`
	}
	if !operatorDecode(w, r, &v) {
		return
	}
	if len(v.Keys) == 0 {
		writeErr(w, 400, "select cleanup categories")
		return
	}
	items, token, err := s.scanOperatorCleanup(r.Context())
	if err != nil {
		operatorError(w, err)
		return
	}
	if v.ScanToken == "" || v.ScanToken != token {
		writeErr(w, 409, "rescan before cleanup; filesystem changed")
		return
	}
	selected := map[string]bool{}
	for _, k := range v.Keys {
		selected[k] = true
	}
	paths := []string{}
	for _, i := range items {
		if selected[i.Key] {
			paths = append(paths, i.Paths...)
			delete(selected, i.Key)
		}
	}
	if len(selected) > 0 {
		writeErr(w, 400, "unknown cleanup category")
		return
	}
	sort.Strings(paths)
	lsof, err := exec.LookPath("lsof")
	if err != nil {
		writeErr(w, 503, "lsof required to prove files are not in use")
		return
	}
	removed := 0
	// Serialize with task/conversation lifecycle admission; rescan before each removal.
	s.concMu.Lock()
	defer s.concMu.Unlock()
	for _, p := range paths {
		if !cleanupPathSafe(s.dataDir, p) {
			writeErr(w, 409, "cleanup path changed")
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
		cmd := exec.CommandContext(ctx, lsof, "-t", "+D", p)
		if fi, e := os.Stat(p); e == nil && !fi.IsDir() {
			cmd = exec.CommandContext(ctx, lsof, "-t", p)
		}
		b, e := cmd.Output()
		deadline := ctx.Err()
		cancel()
		if deadline != nil {
			writeErr(w, 409, "open-file check timed out")
			return
		}
		if len(b) > 0 {
			writeErr(w, 409, "candidate still in use")
			return
		}
		if e != nil {
			if ex, ok := e.(*exec.ExitError); !ok || ex.ExitCode() != 1 {
				writeErr(w, 409, "open-file check unavailable")
				return
			}
		}
		if err = os.RemoveAll(p); err != nil {
			operatorError(w, err)
			return
		}
		removed++
	}
	writeJSON(w, 200, map[string]any{"removed": removed})
}
