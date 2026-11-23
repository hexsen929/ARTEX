package server

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/Autumn-27/artex/db"
	"github.com/Autumn-27/artex/llmrec"
	"log"
	"net/http"
	"strings"
	"time"
)

const originalSurfacePrompt = "你是漏洞分类器。把每个漏洞类型字符串归到以下 key 之一：\nauth=认证面, authz=授权面(越权), injection=注入面, file=文件面, request=请求面,\ncache=缓存与CDN, logic=业务逻辑, api=接口与文档面, client=前端与客户端面,\ninfra=服务与基础设施面, other=无法归类。\n只输出一个 JSON 对象：键必须是输入里的原字符串，值是 key；不要输出解释，不要代码块。"

func parseSurfaceMapping(raw string, allowed []string) map[string]string {
	raw = strings.TrimSpace(raw)
	if strings.HasPrefix(raw, "```") {
		if i := strings.IndexByte(raw, '\n'); i >= 0 {
			raw = raw[i+1:]
		}
		raw = strings.TrimSpace(strings.TrimSuffix(raw, "```"))
	}
	start, end := strings.IndexByte(raw, '{'), strings.LastIndexByte(raw, '}')
	if start < 0 || end < start {
		return map[string]string{}
	}
	var v map[string]string
	if json.Unmarshal([]byte(raw[start:end+1]), &v) != nil {
		return map[string]string{}
	}
	known := map[string]bool{}
	for _, s := range allowed {
		known[s] = true
	}
	out := map[string]string{}
	for key, surface := range v {
		surface = strings.ToLower(strings.TrimSpace(surface))
		if known[key] && db.ValidFindingSurface(surface) {
			out[key] = surface
		}
	}
	return out
}
func (s *Server) classifyUnknownSurfaces(ctx context.Context) (int, error) {
	s.surfaceClassifyMu.Lock()
	defer s.surfaceClassifyMu.Unlock()
	if s.m == nil || s.m.pg == nil {
		return 0, nil
	}
	classes, err := s.m.pg.FindingVulnClassList(ctx)
	if err != nil {
		return 0, err
	}
	overrides, err := s.m.pg.SurfaceOverrides(ctx)
	if err != nil {
		return 0, err
	}
	unknown := []string{}
	bytes := 0
	for _, c := range classes {
		if _, ok := overrides[c]; ok || db.SurfaceKeyFor(c) != "other" {
			continue
		}
		if len(c) > 1000 {
			continue
		}
		if len(unknown) == 128 || bytes+len(c) > 16<<10 {
			break
		}
		unknown = append(unknown, c)
		bytes += len(c)
	}
	if len(unknown) == 0 {
		return 0, nil
	}
	profile, err := s.m.pg.ActiveProfile()
	if err != nil {
		return 0, err
	}
	if profile == nil {
		return 0, fmt.Errorf("no active model for surface classification")
	}
	provider, _, ok := s.providerForProfile(profile.ID)
	if !ok {
		return 0, fmt.Errorf("surface classification provider unavailable")
	}
	raw, _ := json.Marshal(unknown)
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	ctx = llmrec.WithWorker(ctx, "classifier")
	answer, err := streamCollectText(ctx, provider, originalSurfacePrompt, string(raw))
	if err != nil {
		return 0, err
	}
	mapping := parseSurfaceMapping(answer, unknown)
	if len(mapping) == 0 {
		return 0, fmt.Errorf("model returned no valid surface mappings")
	}
	if err = s.m.pg.UpsertSurfaceOverrides(ctx, mapping); err != nil {
		return 0, err
	}
	return len(mapping), nil
}
func (s *Server) runSurfaceClassifier(ctx context.Context) {
	first := time.NewTimer(5 * time.Second)
	defer first.Stop()
	select {
	case <-ctx.Done():
		return
	case <-first.C:
	}
	tick := time.NewTicker(10 * time.Minute)
	defer tick.Stop()
	for {
		if s.m.pg.GetBool("operator_surface_classifier_enabled", true) {
			if n, err := s.classifyUnknownSurfaces(ctx); err != nil {
				if ctx.Err() != nil {
					return
				}
				log.Printf("[surface] classification unavailable; preserving existing mappings")
			} else if n > 0 {
				log.Printf("[surface] classified %d previously unknown finding classes", n)
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}
func (s *Server) registerSurfaceRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/findings/surfaces", s.operatorSurfaces)
	mux.HandleFunc("PUT /api/findings/surfaces", s.operatorSaveSurfaces)
	mux.HandleFunc("POST /api/findings/surfaces/classify", s.operatorClassifySurfaces)
}
func (s *Server) operatorSurfaces(w http.ResponseWriter, r *http.Request) {
	if s.pg(w) == nil {
		return
	}
	counts, err := s.m.pg.FindingSurfaceCounts(r.Context(), findingFilterFromQuery(r.URL.Query()))
	if err != nil {
		operatorError(w, err)
		return
	}
	overrides, err := s.m.pg.SurfaceOverrides(r.Context())
	if err != nil {
		operatorError(w, err)
		return
	}
	writeJSON(w, 200, map[string]any{"categories": db.FindingSurfaceCategories(), "counts": counts, "overrides": overrides, "automatic_enabled": s.m.pg.GetBool("operator_surface_classifier_enabled", true)})
}
func (s *Server) operatorSaveSurfaces(w http.ResponseWriter, r *http.Request) {
	if s.pg(w) == nil {
		return
	}
	var v struct {
		Mapping map[string]string `json:"mapping"`
		Enabled *bool             `json:"automatic_enabled"`
	}
	if !operatorDecode(w, r, &v) {
		return
	}
	if len(v.Mapping) > 0 {
		if err := s.m.pg.UpsertSurfaceOverrides(r.Context(), v.Mapping); err != nil {
			writeErr(w, 400, err.Error())
			return
		}
	}
	if v.Enabled != nil {
		if err := s.m.pg.SetBool("operator_surface_classifier_enabled", *v.Enabled); err != nil {
			operatorError(w, err)
			return
		}
	}
	writeJSON(w, 200, map[string]any{"ok": true})
}
func (s *Server) operatorClassifySurfaces(w http.ResponseWriter, r *http.Request) {
	if s.pg(w) == nil {
		return
	}
	n, err := s.classifyUnknownSurfaces(r.Context())
	if err != nil {
		writeErr(w, 503, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"classified": n})
}
