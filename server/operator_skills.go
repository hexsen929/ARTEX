package server

import (
	"context"
	"fmt"
	"github.com/Autumn-27/artex/kb"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

type operatorSkillHit struct {
	Skill    string `json:"skill"`
	Path     string `json:"path"`
	Category string `json:"category"`
	Kind     string `json:"kind"`
	Snippet  string `json:"snippet"`
	Score    int    `json:"score"`
}

func (s *Server) registerSkillSearchRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/skills/search", s.operatorSkillSearch)
	mux.HandleFunc("POST /api/skills/reindex", s.operatorSkillReindex)
	mux.HandleFunc("PATCH /api/skills/{name}/category", s.operatorSkillCategory)
	mux.HandleFunc("PUT /api/skills/{name}/category", s.operatorSkillCategory)
	mux.HandleFunc("GET /api/operator/skill-categories", s.operatorSkillCategories)
}
func (s *Server) indexOperatorSkills(ctx context.Context) (int, error) {
	tx, err := s.m.pg.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	stamp := time.Now()
	n, total := 0, 0
	err = filepath.WalkDir(s.skillDir, func(p string, d fs.DirEntry, e error) error {
		if e != nil {
			return e
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if d.Type()&os.ModeSymlink != 0 {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		rel, err := filepath.Rel(s.skillDir, p)
		if err != nil {
			return err
		}
		parts := strings.Split(filepath.ToSlash(rel), "/")
		if d.IsDir() {
			if len(parts) > 8 || d.Name() == "node_modules" || strings.HasPrefix(d.Name(), ".") && rel != "." {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.EqualFold(filepath.Ext(p), ".md") {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		if info.Size() > 256<<10 {
			return nil
		}
		if n >= 4096 || int64(total)+info.Size() > 32<<20 {
			return fmt.Errorf("skill index exceeds bounded size")
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		name := parts[0]
		if len(parts) == 1 {
			name = strings.TrimSuffix(name, filepath.Ext(name))
		}
		if !validSkillName(name) {
			return nil
		}
		kind := "reference"
		if d.Name() == "SKILL.md" || len(parts) == 1 {
			kind = "skill"
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO skill_docs(skill,path,kind,content,mtime,size,updated_at) VALUES($1,$2,$3,$4,$5,$6,$7) ON CONFLICT(skill,path) DO UPDATE SET kind=EXCLUDED.kind,content=EXCLUDED.content,mtime=EXCLUDED.mtime,size=EXCLUDED.size,updated_at=EXCLUDED.updated_at`, name, filepath.ToSlash(rel), kind, string(b), info.ModTime().UnixNano(), len(b), stamp)
		if err != nil {
			return err
		}
		category := "other"
		for _, v := range []struct{ key, cat string }{{"web", "web"}, {"ad", "ad-internal"}, {"rev", "reverse"}, {"app", "reverse"}, {"pwn", "pwn"}, {"priv", "cloud-container"}, {"pivot", "webshell-c2"}, {"recon", "recon"}} {
			if strings.Contains(strings.ToLower(name), v.key) {
				category = v.cat
				break
			}
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO skill_categories(skill,category,source) VALUES($1,$2,'inferred') ON CONFLICT(skill) DO NOTHING`, name, category)
		if err != nil {
			return err
		}
		n++
		total += len(b)
		return nil
	})
	if err != nil {
		return 0, err
	}
	if _, err = tx.ExecContext(ctx, "DELETE FROM skill_docs WHERE updated_at<$1", stamp); err != nil {
		return 0, err
	}
	return n, tx.Commit()
}
func (s *Server) searchOperatorSkills(ctx context.Context, q string, limit int) ([]operatorSkillHit, error) {
	if limit <= 0 {
		limit = 10
	}
	if limit > 50 {
		limit = 50
	}
	terms := kb.Terms(q)
	hits := []operatorSkillHit{}
	if len(terms) == 0 {
		return hits, nil
	}
	rows, err := s.m.pg.QueryContext(ctx, `SELECT d.skill,d.path,d.kind,d.content,COALESCE(c.category,'other') FROM skill_docs d LEFT JOIN skill_categories c ON c.skill=d.skill ORDER BY d.skill,d.path LIMIT 4096`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var h operatorSkillHit
		var content string
		if err = rows.Scan(&h.Skill, &h.Path, &h.Kind, &content, &h.Category); err != nil {
			return nil, err
		}
		low := strings.ToLower(content)
		pos := -1
		for _, t := range terms {
			if strings.Contains(strings.ToLower(h.Skill+" "+h.Path), t) {
				h.Score += 8
			}
			if i := strings.Index(low, t); i >= 0 {
				h.Score++
				if pos < 0 {
					pos = i
				}
			}
		}
		if h.Score == 0 {
			continue
		}
		rr := []rune(content)
		start := 0
		if pos > 0 {
			start = len([]rune(content[:pos])) - 100
			if start < 0 {
				start = 0
			}
		}
		end := start + 500
		if end > len(rr) {
			end = len(rr)
		}
		h.Snippet = string(rr[start:end])
		hits = append(hits, h)
	}
	if err = rows.Err(); err != nil {
		return nil, err
	}
	sort.SliceStable(hits, func(i, j int) bool { return hits[i].Score > hits[j].Score })
	if len(hits) > limit {
		hits = hits[:limit]
	}
	return hits, nil
}
func (s *Server) operatorSkillReindex(w http.ResponseWriter, r *http.Request) {
	if s.pg(w) == nil {
		return
	}
	n, err := s.indexOperatorSkills(r.Context())
	if err != nil {
		operatorError(w, err)
		return
	}
	writeJSON(w, 200, map[string]any{"indexed": n})
}
func (s *Server) operatorSkillSearch(w http.ResponseWriter, r *http.Request) {
	if s.pg(w) == nil {
		return
	}
	q := r.URL.Query().Get("q")
	if len(q) > 2000 {
		writeErr(w, 400, "query too long")
		return
	}
	n, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	hits, err := s.searchOperatorSkills(r.Context(), q, n)
	if err != nil {
		operatorError(w, err)
		return
	}
	writeJSON(w, 200, map[string]any{"q": q, "hits": hits})
}
func (s *Server) operatorSkillCategory(w http.ResponseWriter, r *http.Request) {
	if s.pg(w) == nil {
		return
	}
	name := r.PathValue("name")
	var v struct {
		Category string `json:"category"`
	}
	if !operatorDecode(w, r, &v) {
		return
	}
	if !validSkillName(name) || strings.TrimSpace(v.Category) == "" || len(v.Category) > 100 {
		writeErr(w, 400, "invalid skill/category")
		return
	}
	_, err := s.m.pg.ExecContext(r.Context(), `INSERT INTO skill_categories(skill,category,source) VALUES($1,$2,'manual') ON CONFLICT(skill) DO UPDATE SET category=EXCLUDED.category,source='manual',updated_at=now()`, name, v.Category)
	if err != nil {
		operatorError(w, err)
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true})
}
func (s *Server) operatorSkillCategories(w http.ResponseWriter, r *http.Request) {
	if s.pg(w) == nil {
		return
	}
	rows, err := s.m.pg.QueryContext(r.Context(), "SELECT skill,category,source FROM skill_categories ORDER BY skill")
	if err != nil {
		operatorError(w, err)
		return
	}
	defer rows.Close()
	out := []map[string]string{}
	for rows.Next() {
		var a, b, c string
		if err = rows.Scan(&a, &b, &c); err != nil {
			operatorError(w, err)
			return
		}
		out = append(out, map[string]string{"skill": a, "category": b, "source": c})
	}
	if err = rows.Err(); err != nil {
		operatorError(w, err)
		return
	}
	writeJSON(w, 200, map[string]any{"items": out})
}
