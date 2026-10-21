package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/Autumn-27/artex/db"
)

func (s *Server) syncSSStartJob(w http.ResponseWriter, r *http.Request) {
	if s.pg(w) == nil {
		return
	}
	var req ssSyncReq
	if err := decode(r, &req); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	if req.RetryRunID > 0 {
		previous, err := s.m.pg.GetSyncRun(r.Context(), req.RetryRunID)
		if err != nil || previous == nil {
			writeErr(w, 400, "重试来源不存在")
			return
		}
		if previous.Status == "running" {
			writeErr(w, 409, "请等待原同步结束")
			return
		}
		id := req.RetryRunID
		if err := json.Unmarshal(previous.Request, &req); err != nil {
			writeErr(w, 400, "原同步请求无效")
			return
		}
		req.RetryRunID = id
	}
	if err := req.normalize(); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	if !s.ssJobMu.TryLock() {
		writeErr(w, 409, "已有后台同步运行，请在历史中查看进度")
		return
	}
	req.Background = true
	result := newSSSyncResult()
	source, err := s.findMCPByName(scopeSentryMCPName)
	if req.RetryRunID == 0 && (err != nil || source == nil || !source.Enabled) {
		s.ssJobMu.Unlock()
		writeErr(w, 400, "数据源未就绪")
		return
	}
	raw, _ := json.Marshal(req)
	initial, _ := json.Marshal(result)
	var sourceID *int64
	if source != nil {
		sourceID = &source.ID
	}
	run, err := s.m.pg.CreateSyncRun(r.Context(), sourceID, raw, initial)
	if err != nil {
		s.ssJobMu.Unlock()
		writeErr(w, 500, err.Error())
		return
	}
	result.RunID, result.StartedAt = run.ID, run.StartedAt
	writeJSON(w, http.StatusAccepted, result)
	go s.executeSSJob(req, result)
}

func (s *Server) syncSSResumeJob(w http.ResponseWriter, r *http.Request) {
	if s.pg(w) == nil {
		return
	}
	id, ok := pathInt(r, "id")
	if !ok || id <= 0 {
		writeErr(w, 400, "无效记录编号")
		return
	}
	if !s.ssJobMu.TryLock() {
		writeErr(w, 409, "已有后台同步运行")
		return
	}
	dispatched := false
	defer func() {
		if !dispatched {
			s.ssJobMu.Unlock()
		}
	}()
	run, err := s.m.pg.GetSyncRun(r.Context(), id)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	if run == nil {
		writeErr(w, 404, "同步记录不存在")
		return
	}
	var req ssSyncReq
	if err := json.Unmarshal(run.Request, &req); err != nil || !req.Background {
		writeErr(w, 400, "该记录不支持断点恢复，请重新同步")
		return
	}
	if err := req.normalize(); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	if err := s.m.pg.ResumeSyncRun(r.Context(), id); err != nil {
		writeErr(w, 409, "记录正在运行或已完成")
		return
	}
	var result ssSyncResult
	if err := json.Unmarshal(run.Result, &result); err != nil {
		result = newSSSyncResult()
	}
	result.RunID, result.StartedAt, result.Status, result.FinishedAt = run.ID, run.StartedAt, "running", nil
	result.Errors = []string{}
	dispatched = true
	writeJSON(w, http.StatusAccepted, result)
	go s.executeSSJob(req, result)
}

// One page is durable before any record is applied. Recorded rows are skipped
// on resume; a partial page is never refetched from a moving source dataset.
func (s *Server) executeSSJob(req ssSyncReq, result ssSyncResult) {
	defer s.ssJobMu.Unlock()
	parent := s.ctx
	if parent == nil {
		parent = context.Background()
	}
	ctx, cancel := context.WithTimeout(parent, 6*time.Hour)
	defer cancel()
	defer func() {
		if recovered := recover(); recovered != nil {
			ssAppendIssue(&result.Errors, fmt.Sprintf("同步异常退出: %v", recovered))
			result.Status = "interrupted"
		}
		if result.Status == "running" {
			result.Status = ssOverallStatus(result)
		}
		if ctx.Err() != nil {
			result.Status = "interrupted"
			ssAppendIssue(&result.Errors, "同步中断，可从已保存页面继续")
		}
		_ = s.persistSSResult(context.Background(), &result)
	}()
	if req.RetryRunID > 0 {
		s.retrySSItems(ctx, req, &result)
		return
	}
	cl, err := s.scopeSentryClient(ctx)
	if err != nil {
		ssAppendIssue(&result.Errors, err.Error())
		return
	}
	defer cl.Close()
	for _, target := range req.Targets {
		if req.Dimension == "project" && req.CreateCompany {
			s.syncSSCompany(ctx, cl, target, &result)
		}
		for _, kind := range req.AssetTypes {
			if ctx.Err() != nil {
				return
			}
			index := -1
			for i := range result.Targets {
				if result.Targets[i].Target == target && result.Targets[i].AssetType == kind {
					index = i
					break
				}
			}
			if index < 0 {
				result.Targets = append(result.Targets, ssTargetResult{Target: target, AssetType: kind, SourceTargetID: req.sourceTargetID(target)})
				index = len(result.Targets) - 1
			}
			one := &result.Targets[index]
			one.Status, one.Error, one.Truncated = "running", "", false
			one.Stats.Fetched = 0
			if err := s.persistSSResult(ctx, &result); err != nil {
				ssAppendIssue(&result.Errors, err.Error())
				return
			}
			ssType := map[string]string{"service": "asset", "subdomain": "subdomain", "app": "app"}[kind]
			query := ssAssetQuery(ssType, req.Dimension, target)
			for page := 1; ; page++ {
				p, err := s.m.pg.GetSyncPage(ctx, result.RunID, target, kind, page)
				if err == nil && p == nil {
					p, err = ssFetchPage(ctx, cl, ssType, query, page, req.PageSize)
					if err == nil {
						err = s.m.pg.SaveSyncPage(ctx, result.RunID, target, kind, page, p)
					}
				}
				if err != nil {
					one.Error = err.Error()
					break
				}
				one.Stats.Fetched += p.Count
				if !p.Done {
					for offset, raw := range p.Items {
						itemIndex := (page-1)*req.PageSize + offset
						recorded, err := s.m.pg.SyncItemRecorded(ctx, result.RunID, target, kind, itemIndex)
						if err != nil {
							one.Error = err.Error()
							break
						}
						if recorded {
							continue
						}
						item := &db.SyncObservation{RunID: result.RunID, Target: target, SourceTargetID: req.sourceTargetID(target), AssetType: kind, ItemIndex: itemIndex, SourceAssetID: ssSourceAssetID(raw), Raw: raw}
						if err := s.storeSSItem(ctx, item); err != nil {
							one.Error = err.Error()
							break
						}
						if warning := ssRawWarnings(raw); warning != "" {
							ssAppendIssue(&result.Warnings, fmt.Sprintf("%s/%s 详情未同步: %s", target, kind, warning))
						}
						if item.RawOmitted {
							ssAppendIssue(&result.Warnings, "单条原始 JSON 超过 16 MiB，未保存；可通过源资产 ID 回查")
						}
					}
					if one.Error != "" {
						break
					}
					if err := s.m.pg.FinishSyncPage(ctx, result.RunID, target, kind, page); err != nil {
						one.Error = err.Error()
						break
					}
				}
				if err := s.persistSSResult(ctx, &result); err != nil {
					one.Error = err.Error()
					break
				}
				if p.Last {
					break
				}
			}
			// SQL-derived counters remain authoritative after an interrupted page.
			if stored, err := s.m.pg.GetSyncRun(ctx, result.RunID); err == nil && stored != nil {
				var current ssSyncResult
				if json.Unmarshal(stored.Result, &current) == nil {
					result.Stats, result.UniqueAssets, result.Synced = current.Stats, current.UniqueAssets, current.Synced
					for _, t := range current.Targets {
						if t.Target == target && t.AssetType == kind {
							fetched := one.Stats.Fetched
							one.Stats = t.Stats
							one.Stats.Fetched = fetched
							break
						}
					}
				}
			}
			one.Status = ssTargetStatus(*one)
			if one.Error != "" {
				ssAppendIssue(&result.Errors, fmt.Sprintf("%s/%s: %s", target, kind, one.Error))
			}
		}
	}
}

func decodeSSStoredResult(run *db.SyncRun) ssSyncResult {
	var result ssSyncResult
	if run != nil {
		_ = json.Unmarshal(run.Result, &result)
	}
	return result
}

func ssFetchPage(ctx context.Context, cl ssToolCaller, kind string, query map[string]any, page, size int) (*db.SyncPage, error) {
	args := map[string]any{"asset_type": kind, "pageIndex": page, "pageSize": size}
	for k, v := range query {
		args[k] = v
	}
	text, err := cl.Call(ctx, "list_assets", args)
	if err != nil {
		return nil, fmt.Errorf("第 %d 页: %w", page, err)
	}
	var envelope map[string]json.RawMessage
	if err := json.Unmarshal([]byte(text), &envelope); err != nil {
		return nil, err
	}
	raw, ok := envelope["list"]
	if !ok {
		return nil, fmt.Errorf("第 %d 页缺少 list", page)
	}
	if string(raw) == "null" {
		total, err := ssCountAssets(ctx, cl, kind, query)
		if err != nil {
			return nil, err
		}
		if total > (page-1)*size {
			return nil, fmt.Errorf("第 %d 页为空但源端仍有 %d 条", page, total)
		}
	}
	p := &db.SyncPage{}
	if err := json.Unmarshal(raw, &p.Items); err != nil {
		return nil, err
	}
	if len(p.Items) > size {
		return nil, fmt.Errorf("第 %d 页返回条数超过请求页长", page)
	}
	p.Count = len(p.Items)
	p.Last = p.Count < size
	if kind == "asset" {
		for i, raw := range p.Items {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			p.Items[i] = ssWithDetail(ctx, cl, raw)
		}
	}
	return p, nil
}
