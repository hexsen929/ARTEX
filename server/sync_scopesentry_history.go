package server

import (
	"context"
	"encoding/json"
	"net/http"
	"time"

	"github.com/Autumn-27/artex/db"
)

func (s *Server) recoverSSSyncRuns() error {
	if s.m == nil || s.m.pg == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_, err := s.m.pg.RecoverSyncRuns(ctx)
	return err
}

func syncHistoryPagination(r *http.Request) (page, size int) {
	page, size = queryInt(r, "page", 1), queryInt(r, "size", 20)
	if size > 100 {
		size = 100
	}
	if page > 1000000 {
		page = 1000000
	}
	return page, size
}

// These endpoints depend only on PostgreSQL, never on source reachability or
// whether the associated MCP still exists.
func (s *Server) syncSSHistory(w http.ResponseWriter, r *http.Request) {
	pg := s.pg(w)
	if pg == nil {
		return
	}
	page, size := syncHistoryPagination(r)
	runs, total, err := pg.ListSyncRuns(r.Context(), size, (page-1)*size)
	if err != nil {
		writeErr(w, 500, "读取同步历史失败: "+err.Error())
		return
	}
	for _, run := range runs {
		normalizeSSHistoryResult(run)
	}
	writeJSON(w, 200, map[string]any{"runs": runs, "total": total, "page": page, "size": size})
}

func (s *Server) syncSSHistoryDetail(w http.ResponseWriter, r *http.Request) {
	pg := s.pg(w)
	if pg == nil {
		return
	}
	id, ok := pathInt(r, "id")
	if !ok || id <= 0 {
		writeErr(w, 400, "无效的同步历史编号")
		return
	}
	run, err := pg.GetSyncRun(r.Context(), id)
	if err != nil {
		writeErr(w, 500, "读取同步历史失败: "+err.Error())
		return
	}
	if run == nil {
		writeErr(w, 404, "同步历史不存在")
		return
	}
	page, size := syncHistoryPagination(r)
	includeRaw := r.URL.Query().Get("include_raw") == "1"
	if includeRaw && size > 1 {
		size = 1
	}
	items, total, err := pg.ListSyncObservations(r.Context(), id, size, (page-1)*size, includeRaw)
	if err != nil {
		writeErr(w, 500, "读取同步明细失败: "+err.Error())
		return
	}
	normalizeSSHistoryResult(run)
	writeJSON(w, 200, map[string]any{"run": run, "items": items, "total": total, "page": page, "size": size})
}

func normalizeSSHistoryResult(run *db.SyncRun) {
	var result ssSyncResult
	if json.Unmarshal(run.Result, &result) != nil {
		return
	}
	result.RunID, result.Status = run.ID, run.Status
	result.StartedAt, result.FinishedAt = run.StartedAt, run.FinishedAt
	if run.Status == "interrupted" {
		for i := range result.Targets {
			if result.Targets[i].Status == "running" {
				result.Targets[i].Status = "interrupted"
			}
		}
	}
	run.Result, _ = json.Marshal(result)
}
