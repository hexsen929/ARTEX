package server

import (
	"context"
	"encoding/json"
	"github.com/Autumn-27/artex/db"
)

func (s *Server) retrySSItems(ctx context.Context, req ssSyncReq, result *ssSyncResult) {
	result.Targets = nil
	result.Stats.Fetched = 0
	for after := int64(0); ; {
		item, err := s.m.pg.NextFailedSyncItem(ctx, req.RetryRunID, after)
		if err != nil {
			ssAppendIssue(&result.Errors, err.Error())
			return
		}
		if item == nil {
			if len(result.Targets) == 0 {
				result.Status = "success"
			}
			return
		}
		after = item.ID
		index := -1
		for i := range result.Targets {
			if result.Targets[i].Target == item.Target && result.Targets[i].AssetType == item.AssetType {
				index = i
				break
			}
		}
		if index < 0 {
			result.Targets = append(result.Targets, ssTargetResult{Target: item.Target, AssetType: item.AssetType, SourceTargetID: item.SourceTargetID, Status: "running"})
			index = len(result.Targets) - 1
		}
		one := &result.Targets[index]
		one.Stats.Fetched++
		result.Stats.Fetched++
		recorded, err := s.m.pg.SyncItemRecorded(ctx, result.RunID, item.Target, item.AssetType, item.ItemIndex)
		if err != nil {
			ssAppendIssue(&result.Errors, err.Error())
			return
		}
		if !recorded {
			if err := s.m.pg.SaveSyncPage(ctx, result.RunID, item.Target, item.AssetType, item.ItemIndex+1, &db.SyncPage{Items: []json.RawMessage{item.Raw}, Count: 1, Last: true}); err != nil {
				ssAppendIssue(&result.Errors, err.Error())
				return
			}
			item.ID, item.RunID = 0, result.RunID
			if len(item.Raw) == 0 {
				item.Status, item.Reason = "failed", "原记录未保留完整 JSON，请重新同步源范围"
				err = s.m.pg.RecordSyncObservation(ctx, item)
			} else {
				err = s.storeSSItem(ctx, item)
			}
			if err != nil {
				ssAppendIssue(&result.Errors, err.Error())
				return
			}
			if err = s.m.pg.FinishSyncPage(ctx, result.RunID, item.Target, item.AssetType, item.ItemIndex+1); err != nil {
				ssAppendIssue(&result.Errors, err.Error())
				return
			}
		}
		// Query-derived written/skipped/failed counters also cover resumed items.
		if err := s.persistSSResult(ctx, result); err != nil {
			ssAppendIssue(&result.Errors, err.Error())
			return
		}
		stored, err := s.m.pg.GetSyncRun(ctx, result.RunID)
		if err != nil {
			ssAppendIssue(&result.Errors, err.Error())
			return
		}
		current := decodeSSStoredResult(stored)
		result.Stats.Written, result.Stats.Failed, result.Stats.Skipped = current.Stats.Written, current.Stats.Failed, current.Stats.Skipped
		result.UniqueAssets, result.Synced = current.UniqueAssets, current.Synced
		for _, target := range current.Targets {
			if target.Target == one.Target && target.AssetType == one.AssetType {
				one.Stats = target.Stats
				break
			}
		}
		one.Status = ssTargetStatus(*one)
	}
}
