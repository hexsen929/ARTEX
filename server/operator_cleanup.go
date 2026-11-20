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
	Key        string            `json:"key"`
	Label      string            `json:"label"`
	Hint       string            `json:"hint"`
	Count      int               `json:"count"`
	Bytes      int64             `json:"bytes"`
	Paths      []string          `json:"-"`
	Signatures map[string]string `json:"-"`
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
	fi, err := os.Lstat(a)
	if err != nil || !fi.IsDir() || fi.Mode()&os.ModeSymlink != 0 {
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
func cleanupSize(p string) (int64, error) { n, _, err := cleanupTreeSnapshot(p); return n, err }
func cleanupTreeSnapshot(p string) (int64, string, error) {
	var n int64
	h := sha256.New()
	err := filepath.WalkDir(p, func(child string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.Type()&os.ModeSymlink != 0 {
			return fmt.Errorf("symlink present; skip")
		}
		fi, err := d.Info()
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(p, child)
		if err != nil {
			return err
		}
		fmt.Fprintf(h, "%s:%d:%d:%d\n", rel, fi.Mode(), fi.Size(), fi.ModTime().UnixNano())
		if !d.IsDir() {
			n += fi.Size()
		}
		return nil
	})
	return n, hex.EncodeToString(h.Sum(nil)), err
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
	var activeTunnels, archiveJobs int
	if err := s.m.pg.QueryRowContext(ctx, "SELECT count(*) FROM tunnels WHERE state!='stopped'").Scan(&activeTunnels); err != nil {
		return nil, "", err
	}
	if err := s.m.pg.QueryRowContext(ctx, "SELECT count(*) FROM task_archives WHERE state!='ready'").Scan(&archiveJobs); err != nil {
		return nil, "", err
	}
	activeStage := map[string]bool{}
	if s.stage != nil {
		for _, e := range s.stage.List() {
			activeStage[e.Token] = true
		}
	}
	defs := []struct {
		key, label, sub, owner string
		stale                  bool
	}{{"orphan_sessions", "孤儿会话目录", "sessions", "conv:", false}, {"orphan_transcripts", "孤儿对话记录", "transcripts", "conv:", false}, {"orphan_task_dirs", "孤儿任务目录", "tasks", "task:", false}, {"orphan_archives", "孤儿任务归档", "archives/tasks", "task:", false}, {"stale_staging", "归档暂存残留", "archives/tasks/.staging", "", true}, {"stale_stage", "文件投递暂存", "stage", "", true}, {"stale_tunnels", "隧道临时文件", "tunnels", "", true}}
	out := []cleanupItem{}
	h := sha256.New()
	for _, d := range defs {
		item := cleanupItem{Key: d.key, Label: d.label, Hint: "仅处理可确认无所属记录、无符号链接且无打开文件的内容；暂存须超过24小时"}
		item.Signatures = map[string]string{}
		if d.key == "stale_tunnels" && activeTunnels > 0 || d.key == "stale_staging" && archiveJobs > 0 {
			item.Hint = "存在活动或待恢复资源，保留此类别全部文件"
			out = append(out, item)
			continue
		}
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
			if d.key == "stale_stage" && activeStage[de.Name()] {
				continue
			}
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
			n, signature, e := cleanupTreeSnapshot(p)
			if e != nil {
				continue
			}
			item.Paths = append(item.Paths, p)
			item.Signatures[p] = signature
			item.Count++
			item.Bytes += n
			fmt.Fprintf(h, "%s:%s:%s\n", d.key, p, signature)
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
	s.concMu.Lock()
	defer s.concMu.Unlock()
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
	snapshots := map[string]string{}
	for _, i := range items {
		if selected[i.Key] {
			paths = append(paths, i.Paths...)
			for p, signature := range i.Signatures {
				snapshots[p] = signature
			}
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
	// The ownership scan and deletion share lifecycle admission lock. Use an
	// OS-confined root so a replaced intermediate symlink cannot escape dataDir.
	confined, err := os.OpenRoot(s.dataDir)
	if err != nil {
		operatorError(w, err)
		return
	}
	defer confined.Close()
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
		if _, signature, e := cleanupTreeSnapshot(p); e != nil || signature != snapshots[p] {
			writeErr(w, 409, "candidate changed during cleanup checks; rescan")
			return
		}
		rel, e := filepath.Rel(s.dataDir, p)
		if e != nil {
			operatorError(w, e)
			return
		}
		if err = confined.RemoveAll(rel); err != nil {
			operatorError(w, err)
			return
		}
		removed++
	}
	writeJSON(w, 200, map[string]any{"removed": removed})
}
