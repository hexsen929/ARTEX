package server

import (
	"context"
	"fmt"
	"github.com/Autumn-27/artex/kb"
	"github.com/Autumn-27/artex/memory"
	"net/http"
	"strings"
	"time"
)

type operatorKBConfig struct {
	Preflight bool      `json:"preflight"`
	Backfill  bool      `json:"backfill"`
	After     time.Time `json:"after"`
}

func (s *Server) operatorKBConfig(ctx context.Context) operatorKBConfig {
	v := operatorKBConfig{Preflight: true}
	_ = s.m.pg.OperatorSetting(ctx, "kb.config", &v)
	return v
}
func (s *Server) operatorKBConfigGet(w http.ResponseWriter, r *http.Request) {
	if s.pg(w) == nil {
		return
	}
	writeJSON(w, 200, s.operatorKBConfig(r.Context()))
}
func (s *Server) operatorKBConfigPut(w http.ResponseWriter, r *http.Request) {
	if s.pg(w) == nil {
		return
	}
	var v operatorKBConfig
	if !operatorDecode(w, r, &v) {
		return
	}
	old := s.operatorKBConfig(r.Context())
	v.After = old.After
	if v.Backfill && v.After.IsZero() {
		v.After = time.Now().UTC()
	}
	if err := s.m.pg.SaveOperatorSetting(r.Context(), "kb.config", v); err != nil {
		operatorError(w, err)
		return
	}
	writeJSON(w, 200, v)
}
func (s *Server) runOperatorKBWorker(ctx context.Context) {
	ticker := time.NewTicker(15 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			cfg := s.operatorKBConfig(ctx)
			if !cfg.Backfill || cfg.After.IsZero() {
				continue
			}
			_ = s.backfillOperatorKB(ctx, cfg.After, 8)
		}
	}
}
func (s *Server) backfillOperatorKB(ctx context.Context, after time.Time, limit int) error {
	rows, err := s.m.pg.QueryContext(ctx, `SELECT f.id,COALESCE(f.task_id,0),f.name,f.vulnclass,f.summary,f.evidence,f.report FROM findings f WHERE f.verification_status='verified' AND f.created_at >= $1 AND NOT EXISTS(SELECT 1 FROM kb_entries k WHERE k.source_finding_id=f.id) ORDER BY f.id LIMIT $2`, after, limit)
	if err != nil {
		return err
	}
	type item struct {
		id, task                                  int64
		name, category, summary, evidence, report string
	}
	items := []item{}
	for rows.Next() {
		var v item
		if err = rows.Scan(&v.id, &v.task, &v.name, &v.category, &v.summary, &v.evidence, &v.report); err != nil {
			rows.Close()
			return err
		}
		items = append(items, v)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	for _, v := range items {
		body := strings.TrimSpace(v.report)
		if body == "" {
			body = v.evidence
		}
		if body == "" {
			continue
		}
		if _, err = memory.SanitizeSource(v.name + "\n" + v.summary + "\n" + body); err != nil {
			continue
		}
		cat := v.category
		valid := false
		for _, c := range kb.Categories {
			if cat == c {
				valid = true
			}
		}
		if !valid {
			cat = "methodology"
		}
		title := v.name
		if title == "" {
			title = v.summary
		}
		if len([]rune(title)) > 200 {
			title = string([]rune(title)[:200])
		}
		e := kb.Entry{Code: fmt.Sprintf("finding-%d", v.id), Title: title, Category: cat, Kind: "poc", Description: v.summary, Content: body, Verified: true, VerifiedNote: fmt.Sprintf("ARTEX finding#%d has explicit verified status; original evidence: %s", v.id, v.evidence), Source: "task", CreatedBy: "kb_curator", SourceFindingID: v.id, EngagementID: fmt.Sprint(v.task), Polarity: "positive"}
		if len(e.VerifiedNote) > 8192 {
			e.VerifiedNote = e.VerifiedNote[:8192]
		}
		if err = e.Validate(); err != nil {
			continue
		}
		if _, _, err = s.m.pg.KBPut(ctx, e); err != nil {
			return err
		}
	}
	return nil
}
