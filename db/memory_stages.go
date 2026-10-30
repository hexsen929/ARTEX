package db

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
)

func isMemoryStage(kind string) bool {
	return kind == "intent_stage" || kind == "task_stage"
}

type memoryStageFact struct {
	ID         string   `json:"id"`
	Version    int64    `json:"version"`
	Confidence string   `json:"confidence"`
	Summary    string   `json:"summary"`
	Target     string   `json:"target"`
	Targets    []string `json:"targets,omitempty"`
	Conditions []string `json:"conditions"`
}

type memoryStageBundle struct {
	Stage    json.RawMessage `json:"stage"`
	Coverage struct {
		Included int  `json:"included"`
		Total    int  `json:"total"`
		Partial  bool `json:"partial"`
	} `json:"coverage"`
	Facts []memoryStageFact `json:"facts"`
}

func boundedMemoryStageText(text string) (string, error) {
	var bundle memoryStageBundle
	if err := json.Unmarshal([]byte(text), &bundle); err != nil || len(bundle.Facts) == 0 || len(bundle.Facts) > 8 {
		return "", ErrMemoryUnavailable
	}
	for _, fact := range bundle.Facts {
		if fact.ID == "" || len(fact.Summary) == 0 || len(fact.Summary) > 600 {
			return "", ErrMemoryUnavailable
		}
	}
	for {
		bundle.Coverage.Included = len(bundle.Facts)
		bundle.Coverage.Partial = bundle.Coverage.Included < bundle.Coverage.Total
		raw, err := json.Marshal(bundle)
		if err != nil {
			return "", err
		}
		escaped, _ := json.Marshal(string(raw))
		if len(raw) <= 8000 && len(escaped) <= 11000 {
			return string(raw), nil
		}
		// Never silently truncate an observation. A smaller complete bundle tells
		// the model exactly that coverage is partial.
		if len(bundle.Facts) <= 1 {
			return "", ErrMemoryUnavailable
		}
		bundle.Facts = bundle.Facts[:len(bundle.Facts)-1]
	}
}

// MemoryLearningSnapshot is telemetry only; it does not discover/claim jobs,
// seed defaults, build a provider, or send any source content to a model.
func (d *DB) MemoryLearningSnapshot(ctx context.Context) (MemoryLearningSnapshot, error) {
	var out MemoryLearningSnapshot
	err := d.QueryRowContext(ctx, `SELECT
		count(*) FILTER (WHERE job.state='queued'),count(*) FILTER (WHERE job.state='running'),
		count(*) FILTER (WHERE job.state='failed' AND job.attempts<3),
		count(*) FILTER (WHERE job.state='failed' OR (job.state='skipped' AND job.result='failed')),
		count(*) FILTER (WHERE job.state='done') FROM memory_jobs job
		JOIN memory_sources source ON source.id=job.source_ref
		WHERE source.source_kind IN ('intent_stage','task_stage')`).Scan(&out.Queued, &out.Running, &out.RetryableFailed, &out.Failed, &out.Done)
	if err != nil {
		return out, err
	}
	err = d.QueryRowContext(ctx, `SELECT count(*) FILTER (WHERE action='extract_start'),
		count(*) FILTER (WHERE action='extract_admitted') FROM memory_audit
		WHERE action IN ('extract_start','extract_admitted') AND created_at>now()-interval '1 hour'`).Scan(&out.StartsLastHour, &out.AttemptsLastHour)
	if err != nil {
		return out, err
	}
	var last MemoryLearningLastJob
	err = d.QueryRowContext(ctx, `SELECT id,result,updated_at FROM (
		SELECT job.id,CASE WHEN job.result<>'' THEN job.result WHEN job.state='failed' THEN 'failed'
		  WHEN job.state='done' THEN 'no_experience' ELSE 'skipped' END AS result,job.updated_at
		FROM memory_jobs job JOIN memory_sources source ON source.id=job.source_ref
		WHERE source.source_kind IN ('intent_stage','task_stage') AND job.state IN ('done','failed','skipped')
		UNION ALL SELECT last_job_id,last_result,COALESCE(last_result_at,last_completed_at)
		FROM memory_sources WHERE source_kind IN ('intent_stage','task_stage') AND last_result<>''
		AND COALESCE(last_result_at,last_completed_at) IS NOT NULL) terminal
		ORDER BY updated_at DESC,id DESC LIMIT 1`).Scan(&last.ID, &last.Result, &last.UpdatedAt)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return out, err
	}
	if err == nil {
		out.LastJob = &last
	}
	err = d.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM exploration_nodes intent
		JOIN tasks task ON task.exploration_id=intent.exploration_id
		WHERE intent.kind='intent' AND intent.state<>'deleted' AND task.deleted_at IS NULL AND task.archived_at IS NULL
		AND (intent.state IN ('open','running','paused') OR (intent.state IN ('done','blocked','exhausted','stopped')
		  AND intent.completed_at>now()-interval '30 seconds')))
		OR EXISTS(SELECT 1 FROM tasks task WHERE task.deleted_at IS NULL AND task.archived_at IS NULL
		AND task.status IN ('done','failed','timeout') AND task.completed_at>now()-interval '30 seconds')`).Scan(&out.WaitingStage)
	return out, err
}
