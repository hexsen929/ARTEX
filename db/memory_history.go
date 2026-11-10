package db

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strconv"
	"time"
)

const memoryHistoryBatch = 50
const memoryHistorySQL = memoryDiscoveryFilterSQL + `
	AND ($6::timestamptz IS NULL OR (live.source_updated_at,live.source_kind,live.source_id)>($6,$7,$8))` + memoryDiscoveryOrderSQL

// The cursor contains only ordering metadata. It is not an authorization grant;
// each request keeps the administrator API checks and explicit task predicate.
type memoryHistoryCursor struct {
	Version int       `json:"v"`
	TaskID  int64     `json:"task_id"`
	Epoch   int64     `json:"epoch"`
	Updated time.Time `json:"updated"`
	Kind    string    `json:"kind"`
	ID      string    `json:"id"`
}

type MemoryHistoryProgress struct {
	Queued     int    `json:"queued"`
	Scanned    int    `json:"scanned"`
	HasMore    bool   `json:"has_more"`
	NextCursor string `json:"next_cursor"`
	QueueFull  bool   `json:"queue_full"`
}

func decodeMemoryHistoryCursor(raw string, taskID, epoch int64) (memoryHistoryCursor, error) {
	var cursor memoryHistoryCursor
	if raw == "" {
		return memoryHistoryCursor{Version: 1, TaskID: taskID, Epoch: epoch}, nil
	}
	if len(raw) > 1024 {
		return cursor, ErrMemoryInput
	}
	data, err := base64.RawURLEncoding.DecodeString(raw)
	if err != nil || json.Unmarshal(data, &cursor) != nil || cursor.Version != 1 || cursor.TaskID != taskID {
		return cursor, ErrMemoryInput
	}
	if cursor.Epoch != epoch {
		return cursor, ErrMemoryConflict
	}
	if cursor.Updated.IsZero() && cursor.Kind == "" && cursor.ID == "" {
		return cursor, nil
	}
	id, err := strconv.ParseInt(cursor.ID, 10, 64)
	if err != nil || id <= 0 || strconv.FormatInt(id, 10) != cursor.ID || !isMemoryStage(cursor.Kind) || cursor.Updated.IsZero() || cursor.Updated.Year() < 1 || cursor.Updated.Year() > 9999 {
		return cursor, ErrMemoryInput
	}
	return cursor, nil
}

// QueueMemoryHistoryPage scans at most 50 native identities, including rejected
// and empty stages. A keyset cursor advances by inspected identity, never by the
// number queued, so a completed/empty prefix cannot hide later history.
func (d *DB) QueueMemoryHistoryPage(ctx context.Context, taskID int64, rawCursor string) (MemoryHistoryProgress, error) {
	var progress MemoryHistoryProgress
	if taskID < 0 {
		return progress, ErrMemoryInput
	}
	ctx, cancel := context.WithTimeout(ctx, memoryDiscoveryTimeout)
	defer cancel()
	settings, err := d.GetMemorySettings(ctx)
	if err != nil {
		return progress, err
	}
	if !settings.Enabled || !settings.AutoExtract {
		return progress, ErrMemoryUnavailable
	}
	cursor, err := decodeMemoryHistoryCursor(rawCursor, taskID, settings.Epoch)
	if err != nil {
		return progress, err
	}
	var after any
	if !cursor.Updated.IsZero() {
		after = cursor.Updated
	}
	rows, err := d.QueryContext(ctx, memoryHistorySQL, true, settings.LearnAfter, taskID, settings.Epoch, memoryHistoryBatch+1, after, cursor.Kind, cursor.ID)
	if err != nil {
		return progress, err
	}
	candidates := []memoryCandidate{}
	for rows.Next() {
		var c memoryCandidate
		if err := rows.Scan(&c.kind, &c.id, &c.taskID, &c.version, &c.nativeVersion, &c.state, &c.updated); err != nil {
			rows.Close()
			return progress, err
		}
		candidates = append(candidates, c)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return progress, err
	}
	if len(candidates) > memoryHistoryBatch {
		progress.HasMore = true
		candidates = candidates[:memoryHistoryBatch]
	}
	for _, c := range candidates {
		source, err := d.prepareMemoryCandidate(ctx, c, settings, true)
		if err != nil {
			return progress, err
		}
		if source != nil {
			queued, err := d.queueMemoryExtraction(ctx, *source)
			if errors.Is(err, errMemoryQueueFull) {
				progress.HasMore, progress.QueueFull = true, true
				break // Keep the cursor before this unqueued source.
			}
			if err != nil {
				return progress, err
			}
			if queued {
				progress.Queued++
			}
		}
		progress.Scanned++
		cursor.Updated, cursor.Kind, cursor.ID = c.updated, c.kind, c.id
	}
	if progress.HasMore {
		data, _ := json.Marshal(cursor)
		progress.NextCursor = base64.RawURLEncoding.EncodeToString(data)
	}
	return progress, nil
}
