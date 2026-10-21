package db

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

var ErrCompletionChanged = errors.New("task completion input or lifecycle changed")

// CompletionDisposition is an explicit decision to stop work, never evidence
// that the work succeeded. Duplicate work must reference an existing result.
type CompletionDisposition struct {
	IntentID    int64  `json:"intent_id"`
	Kind        string `json:"kind"`
	Reason      string `json:"reason"`
	ReferenceID int64  `json:"reference_id,omitempty"`
}

type CompletionWork struct {
	IntentID int64  `json:"intent_id"`
	State    string `json:"state"`
	Summary  string `json:"summary"`
	CompletionDisposition
}

type CompletionInput struct {
	Work            []CompletionWork     `json:"remaining_work"`
	GoalsMet        bool                 `json:"goals_met"`
	Eligible        bool                 `json:"eligible"`
	Data            json.RawMessage      `json:"data"`
	Review          string               `json:"review,omitempty"`
	TimedOut        bool                 `json:"timed_out,omitempty"`
	RecoveryPending bool                 `json:"recovery_pending,omitempty"`
	RecoveryHistory []CompletionRecovery `json:"recovery_history,omitempty"`
	GoalGaps        []CompletionGoalGap  `json:"goal_gaps,omitempty"`
}

type CompletionGoalGap struct {
	GoalID          int64  `json:"goal_id"`
	ExpectedVersion int    `json:"expected_version"`
	Reason          string `json:"reason"`
}

type CompletionReviewVerdict struct {
	Complete bool
	Reason   string
	GoalGaps []CompletionGoalGap
}

type TaskCompletion struct {
	TaskID           int64           `json:"task_id"`
	AttemptID        int64           `json:"attempt_id"`
	Phase            string          `json:"phase"`
	Reason           string          `json:"reason"`
	Snapshot         json.RawMessage `json:"snapshot"`
	InputDigest      string          `json:"input_digest"`
	StartedAt        time.Time       `json:"started_at"`
	DeadlineAt       time.Time       `json:"deadline_at"`
	UpdatedAt        time.Time       `json:"updated_at"`
	ReviewDeadlineAt *time.Time      `json:"review_deadline_at,omitempty"`
	ReviewProfile    string          `json:"review_profile"`
	FailureKind      string          `json:"failure_kind"`
	FailureCount     int             `json:"failure_count"`
	FailureAttemptID int64           `json:"failure_attempt_id"`
	FailureCounts    json.RawMessage `json:"failure_counts"`
	FailureDigest    string          `json:"failure_digest"`
	FailureProfile   string          `json:"failure_profile"`
	NextRetryAt      *time.Time      `json:"next_retry_at,omitempty"`
	RecoveryAction   string          `json:"recovery_action"`
}

const completionColumns = `task_id,attempt_id,phase,reason,snapshot,input_digest,started_at,deadline_at,updated_at,review_deadline_at,review_profile,failure_kind,failure_count,failure_counts,failure_digest,failure_profile,next_retry_at,recovery_action,failure_attempt_id`

func scanCompletion(row interface{ Scan(...any) error }) (*TaskCompletion, error) {
	c := new(TaskCompletion)
	err := row.Scan(&c.TaskID, &c.AttemptID, &c.Phase, &c.Reason, &c.Snapshot, &c.InputDigest, &c.StartedAt, &c.DeadlineAt, &c.UpdatedAt, &c.ReviewDeadlineAt, &c.ReviewProfile, &c.FailureKind, &c.FailureCount, &c.FailureCounts, &c.FailureDigest, &c.FailureProfile, &c.NextRetryAt, &c.RecoveryAction, &c.FailureAttemptID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return c, err
}

func (d *DB) TaskCompletion(ctx context.Context, taskID int64) (*TaskCompletion, error) {
	return scanCompletion(d.QueryRowContext(ctx, `SELECT `+completionColumns+` FROM task_completions WHERE task_id=$1`, taskID))
}

// completionTransaction briefly excludes input mutations while taking a snapshot
// or committing its decision. No network/model call may run inside fn. The lock
// order matches asset/anchor deletion; bounded waits make contention retryable.
func (d *DB) completionTransaction(ctx context.Context, taskID int64, fn func(*sql.Tx) error) error {
	tx, err := d.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, `SET LOCAL lock_timeout='2s'; SET LOCAL statement_timeout='5s'`); err != nil {
		return err
	}
	// Archive/follow-up transactions take the current task row first as well.
	var lockedID int64
	if err = tx.QueryRowContext(ctx, `SELECT id FROM tasks WHERE id=$1 FOR UPDATE`, taskID).Scan(&lockedID); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `LOCK TABLE assets, exploration_anchors, exploration_nodes,
		exploration_edges, task_scope, task_constraints, task_relations, findings, tasks, task_completions IN SHARE ROW EXCLUSIVE MODE`); err != nil {
		return err
	}
	if err = fn(tx); err != nil {
		return err
	}
	return tx.Commit()
}

// The signature includes dynamic scope membership and direct-source evidence.
// Report text/version is intentionally excluded: Reporter may finish after the
// audit; its independent evidence-version checks remain authoritative.
const completionDataSQL = `WITH ` + contextCoverageCTE + `
SELECT jsonb_build_object(
 'tasks',COALESCE((SELECT jsonb_agg(jsonb_build_object('id',t.id,'goal',t.goal,'description',t.description) ORDER BY t.id)
   FROM tasks t JOIN context_tasks c ON c.task_id=t.id),'[]'::jsonb),
 'nodes',COALESCE((SELECT jsonb_agg(jsonb_build_object('id',n.id,'exploration_id',n.exploration_id,'kind',n.kind,
   'state',n.state,'payload',n.payload,'content_version',n.content_version,
   'proof_review',(SELECT jsonb_build_object('event',a->>'event','reason',a->>'reason','evidence_id',a->'evidence_id','at',a->>'at') FROM jsonb_array_elements(n.goal_audit) WITH ORDINALITY x(a,ord) WHERE a->>'event'='prove' ORDER BY ord DESC LIMIT 1)) ORDER BY n.id)
   FROM exploration_nodes n JOIN context_tasks c ON c.exploration_id=n.exploration_id),'[]'::jsonb),
 'edges',COALESCE((SELECT jsonb_agg(to_jsonb(e) ORDER BY e.exploration_id,e.src_id,e.dst_id,e.rel)
   FROM exploration_edges e JOIN context_tasks c ON c.exploration_id=e.exploration_id),'[]'::jsonb),
 'anchors',COALESCE((SELECT jsonb_agg(to_jsonb(a) ORDER BY a.node_id,a.asset_id) FROM exploration_anchors a
   JOIN exploration_nodes n ON n.id=a.node_id JOIN context_tasks c ON c.exploration_id=n.exploration_id),'[]'::jsonb),
 'scopes',COALESCE((SELECT jsonb_agg(to_jsonb(s) ORDER BY s.id) FROM task_scope s
   JOIN context_tasks c ON c.task_id=s.task_id),'[]'::jsonb),
 'constraints',COALESCE((SELECT jsonb_agg(jsonb_build_object('id',r.id,'exploration_id',r.exploration_id,
   'kind',r.kind,'text',r.text) ORDER BY r.id) FROM task_constraints r
   JOIN context_tasks c ON c.exploration_id=r.exploration_id),'[]'::jsonb),
 'assets',COALESCE((SELECT jsonb_agg(to_jsonb(a)-'updated_at'-'created_at'-'last_seen'-'task_ids' ORDER BY a.id)
   FROM assets a JOIN target t ON t.id=a.id),'[]'::jsonb),
 'findings',COALESCE((SELECT jsonb_agg(jsonb_build_object('id',f.id,'node_id',f.node_id,'name',f.name,
   'severity',f.severity,'status',f.status,'evidence_version',f.evidence_version) ORDER BY f.id)
   FROM findings f JOIN context_tasks c ON c.task_id=f.task_id),'[]'::jsonb)
)::text`

type completionNode struct {
	ID            int64          `json:"id"`
	ExplorationID int64          `json:"exploration_id"`
	Kind          string         `json:"kind"`
	State         string         `json:"state"`
	Payload       map[string]any `json:"payload"`
}

func completionInput(ctx context.Context, tx *sql.Tx, taskID int64, dispositions []CompletionDisposition) (CompletionInput, string, error) {
	var input CompletionInput
	var expID int64
	if err := tx.QueryRowContext(ctx, `SELECT exploration_id FROM tasks WHERE id=$1 AND deleted_at IS NULL`, taskID).Scan(&expID); err != nil {
		return input, "", err
	}
	var raw string
	if err := tx.QueryRowContext(ctx, completionDataSQL, taskID).Scan(&raw); err != nil {
		return input, "", err
	}
	input.Data = json.RawMessage(raw)
	var graph struct {
		Nodes []completionNode `json:"nodes"`
	}
	if err := json.Unmarshal(input.Data, &graph); err != nil {
		return input, "", err
	}
	byID := map[int64]completionNode{}
	for _, n := range graph.Nodes {
		byID[n.ID] = n
	}
	decisions := map[int64]CompletionDisposition{}
	for _, disposition := range dispositions {
		n, exists := byID[disposition.IntentID]
		if !exists || n.Kind != KindIntent || n.ExplorationID != expID {
			return input, "", fmt.Errorf("completion decision refers to a non-local intent %d", disposition.IntentID)
		}
		disposition.Reason = strings.TrimSpace(disposition.Reason)
		if disposition.Reason == "" || (disposition.Kind != "duplicate" && disposition.Kind != "irrelevant") {
			return input, "", fmt.Errorf("intent %d requires duplicate/irrelevant and a stop reason", disposition.IntentID)
		}
		if disposition.Kind == "duplicate" {
			ref, exists := byID[disposition.ReferenceID]
			if !exists || ref.ID == n.ID || !(ref.Kind == KindFact || ref.Kind == KindFinding || ref.Kind == KindIntent && ref.State == "done") {
				return input, "", fmt.Errorf("duplicate intent %d requires a fact, finding or completed work reference", n.ID)
			}
		}
		decisions[n.ID] = disposition
	}
	input.GoalsMet, input.Eligible = true, true
	input.Work = []CompletionWork{}
	for _, n := range graph.Nodes {
		if n.ExplorationID != expID {
			continue
		}
		if n.Kind == KindGoal && n.State != "met" {
			input.GoalsMet = false
		}
		if n.Kind == KindGoal && n.State == "met" {
			current, err := (&ExplorationStore{expID: expID}).currentGoalProof(ctx, tx, n.ID)
			if err != nil {
				return input, "", err
			}
			if !current {
				input.GoalsMet = false
			}
		}
		if n.Kind != KindIntent || n.State == "done" {
			continue
		}
		summary, _ := n.Payload["summary"].(string)
		decision, decided := decisions[n.ID]
		if !decided {
			input.Eligible = false
		}
		input.Work = append(input.Work, CompletionWork{IntentID: n.ID, State: n.State, Summary: summary, CompletionDisposition: decision})
	}
	input.Eligible = input.Eligible && input.GoalsMet
	sum := sha256.Sum256([]byte(raw))
	return input, hex.EncodeToString(sum[:]), nil
}

func completionDispositions(input CompletionInput) []CompletionDisposition {
	out := make([]CompletionDisposition, 0, len(input.Work))
	for _, work := range input.Work {
		if work.Kind != "" {
			d := work.CompletionDisposition
			d.IntentID = work.IntentID
			out = append(out, d)
		}
	}
	return out
}

// BeginTaskCompletion validates all remaining work before installing the durable
// barrier. Timeout attempts retain unfinished work instead of claiming success.
func (d *DB) BeginTaskCompletion(ctx context.Context, taskID int64, reason string, dispositions []CompletionDisposition, deadline time.Time, timedOut bool, profiles ...string) (*TaskCompletion, error) {
	profile := ""
	if len(profiles) > 0 {
		profile = profiles[0]
	}
	var result *TaskCompletion
	err := d.completionTransaction(ctx, taskID, func(tx *sql.Tx) error {
		var status string
		var paused, queued bool
		if err := tx.QueryRowContext(ctx, `SELECT status,paused,queued FROM tasks WHERE id=$1 AND deleted_at IS NULL`, taskID).Scan(&status, &paused, &queued); err != nil {
			return err
		}
		if IsTerminal(status) || !timedOut && (paused || queued) {
			return ErrCompletionChanged
		}
		previous, err := scanCompletion(tx.QueryRowContext(ctx, `SELECT `+completionColumns+` FROM task_completions WHERE task_id=$1`, taskID))
		if err != nil {
			return err
		}
		if previous != nil && (previous.Phase == "draining" || previous.Phase == "reviewing") {
			return ErrCompletionChanged
		}
		if previous != nil && previous.Phase == "completed" {
			var old CompletionInput
			if err := json.Unmarshal(previous.Snapshot, &old); err != nil {
				return err
			}
			// A later human follow-up must not reinterpret historically waived
			// work as successful or require it to be waived a second time.
			for _, decision := range completionDispositions(old) {
				var state string
				if err := tx.QueryRowContext(ctx, `SELECT state FROM exploration_nodes WHERE id=$1 AND kind='intent'`, decision.IntentID).Scan(&state); err == nil && state == "stopped" {
					dispositions = append([]CompletionDisposition{decision}, dispositions...)
				}
			}
		}
		input, digest, err := completionInput(ctx, tx, taskID, dispositions)
		if err != nil {
			return err
		}
		if !timedOut && !input.Eligible {
			return fmt.Errorf("completion rejected: goals_met=%t; %d unfinished intents require explicit stop decisions", input.GoalsMet, len(input.Work))
		}
		input.TimedOut = timedOut
		if previous != nil {
			var old CompletionInput
			if err := json.Unmarshal(previous.Snapshot, &old); err != nil {
				return err
			}
			input.RecoveryHistory = old.RecoveryHistory
			input.GoalGaps = old.GoalGaps
		}
		failureKey, err := completionRetryDigest(input)
		if err != nil {
			return err
		}
		keepFailure := previous != nil && previous.FailureDigest == failureKey && previous.FailureProfile == profile
		if !timedOut && keepFailure && previous.FailureCount > 0 {
			if previous.NextRetryAt == nil || time.Now().Before(*previous.NextRetryAt) {
				return &CompletionRetryError{Kind: previous.FailureKind, Count: previous.FailureCount, Next: previous.NextRetryAt, Action: previous.RecoveryAction}
			}
		}
		failure := TaskCompletion{FailureCounts: json.RawMessage(`{}`)}
		if keepFailure {
			failure = *previous
		}
		raw, err := json.Marshal(input)
		if err != nil {
			return err
		}
		result, err = scanCompletion(tx.QueryRowContext(ctx, `INSERT INTO task_completions(task_id,phase,reason,snapshot,input_digest,deadline_at,review_profile)
		 VALUES($1,'draining',$2,$3,$4,$5,$6) ON CONFLICT(task_id) DO UPDATE SET attempt_id=task_completions.attempt_id+1,
		 phase='draining',reason=EXCLUDED.reason,snapshot=EXCLUDED.snapshot,input_digest=EXCLUDED.input_digest,
		 started_at=now(),deadline_at=EXCLUDED.deadline_at,updated_at=now(),review_deadline_at=NULL,review_profile=EXCLUDED.review_profile,
		 failure_kind=$7,failure_count=$8,failure_counts=$9,failure_digest=$10,failure_profile=$11,next_retry_at=NULL,recovery_action='' RETURNING `+completionColumns,
			taskID, reason, string(raw), digest, deadline, profile, failure.FailureKind, failure.FailureCount, string(failure.FailureCounts), failure.FailureDigest, failure.FailureProfile))
		return err
	})
	return result, err
}

// ReviewTaskCompletion refreshes worker results only after the caller has joined
// all executions. Newly added goals/work must pass the same eligibility check.
type CompletionReviewPolicy struct {
	Budget  time.Duration
	Profile string
}

func (d *DB) ReviewTaskCompletion(ctx context.Context, taskID, attemptID int64, policies ...CompletionReviewPolicy) (*TaskCompletion, error) {
	budget := 180 * time.Second
	var profile *string
	if len(policies) > 0 {
		if policies[0].Budget > 0 {
			budget = policies[0].Budget
		}
		profile = &policies[0].Profile
	}
	deadline := time.Now().Add(budget)
	if parent, ok := ctx.Deadline(); ok && parent.Before(deadline) {
		deadline = parent
	}
	var result *TaskCompletion
	err := d.completionTransaction(ctx, taskID, func(tx *sql.Tx) error {
		current, err := scanCompletion(tx.QueryRowContext(ctx, `SELECT `+completionColumns+` FROM task_completions WHERE task_id=$1 AND attempt_id=$2 AND phase='draining'`, taskID, attemptID))
		if err != nil || current == nil {
			return errors.Join(err, ErrCompletionChanged)
		}
		var prior CompletionInput
		if err := json.Unmarshal(current.Snapshot, &prior); err != nil {
			return err
		}
		input, digest, err := completionInput(ctx, tx, taskID, completionDispositions(prior))
		if err != nil {
			return err
		}
		input.TimedOut = prior.TimedOut
		input.RecoveryHistory = prior.RecoveryHistory
		input.GoalGaps = prior.GoalGaps
		if !input.Eligible && !input.TimedOut {
			return ErrCompletionChanged
		}
		// Keep the original running set for recovery, while the data snapshot
		// contains the actual post-settlement states read by the reviewer.
		actualWork := input.Work
		input.Work = prior.Work
		seen := make(map[int64]bool, len(prior.Work))
		for _, work := range prior.Work {
			seen[work.IntentID] = true
		}
		for _, work := range actualWork {
			if !seen[work.IntentID] {
				input.Work = append(input.Work, work)
			}
		}
		raw, _ := json.Marshal(input)
		result, err = scanCompletion(tx.QueryRowContext(ctx, `UPDATE task_completions SET phase='reviewing',snapshot=$3,input_digest=$4,updated_at=now(),review_deadline_at=$5,review_profile=COALESCE($6,review_profile)
		 WHERE task_id=$1 AND attempt_id=$2 RETURNING `+completionColumns, taskID, attemptID, string(raw), digest, deadline, profile))
		return err
	})
	return result, err
}

// FinishTaskCompletion performs a short, serializable-by-locking compare and
// commit. A stale reviewer can neither overwrite pause/timeout nor omit new work.
func (d *DB) FinishTaskCompletion(ctx context.Context, taskID, attemptID int64, digest, status, review string) error {
	if status != "done" && status != "timeout" {
		return fmt.Errorf("invalid completion terminal %q", status)
	}
	return d.completionTransaction(ctx, taskID, func(tx *sql.Tx) error {
		current, err := scanCompletion(tx.QueryRowContext(ctx, `SELECT `+completionColumns+` FROM task_completions WHERE task_id=$1 AND attempt_id=$2 AND phase='reviewing'`, taskID, attemptID))
		if err != nil || current == nil || current.InputDigest != digest {
			return errors.Join(err, ErrCompletionChanged)
		}
		if current.ReviewDeadlineAt != nil && !time.Now().Before(*current.ReviewDeadlineAt) {
			return ErrCompletionChanged
		}
		var prior CompletionInput
		if err := json.Unmarshal(current.Snapshot, &prior); err != nil {
			return err
		}
		input, actual, err := completionInput(ctx, tx, taskID, completionDispositions(prior))
		if err != nil {
			return err
		}
		if actual != digest || status == "done" && !input.Eligible {
			return ErrCompletionChanged
		}
		res, err := tx.ExecContext(ctx, `UPDATE tasks SET status=$2,completed_at=now()
		 WHERE id=$1 AND deleted_at IS NULL AND status NOT IN ('done','failed','timeout')
		 AND ($2='timeout' OR (NOT paused AND NOT queued AND ($3 OR deadline_at IS NULL OR deadline_at>now())))`, taskID, status, prior.TimedOut)
		if err != nil {
			return err
		}
		if n, err := res.RowsAffected(); err != nil || n != 1 {
			return errors.Join(err, ErrCompletionChanged)
		}
		for _, work := range input.Work {
			reason := work.Reason
			if reason == "" {
				reason = "任务时间预算耗尽；此检查未完成"
			}
			for index := range prior.Work {
				if prior.Work[index].IntentID == work.IntentID {
					prior.Work[index].Reason = reason
					break
				}
			}
			if _, err := tx.ExecContext(ctx, `UPDATE exploration_nodes SET state='stopped',completed_at=COALESCE(completed_at,now()),
			 content_version=content_version+1,payload=payload || jsonb_build_object('completion_stop_reason',$2::text)
			 WHERE id=$1 AND kind='intent' AND state<>'done'`, work.IntentID, reason); err != nil {
				return err
			}
		}
		prior.Review = review
		raw, _ := json.Marshal(prior)
		_, err = tx.ExecContext(ctx, `UPDATE task_completions SET phase='completed',reason=$3,snapshot=$4,updated_at=now()
		 WHERE task_id=$1 AND attempt_id=$2`, taskID, attemptID, review, string(raw))
		return err
	})
}

// InterruptTaskCompletion does not reset the task clock or resurrect a paused or
// terminal task. Only work that this attempt interrupted is eligible for retry.
func (d *DB) InterruptTaskCompletion(ctx context.Context, taskID, attemptID int64, reason string, reopen bool) error {
	return d.completionTransaction(ctx, taskID, func(tx *sql.Tx) error {
		current, err := scanCompletion(tx.QueryRowContext(ctx, `SELECT `+completionColumns+` FROM task_completions
 WHERE task_id=$1 AND attempt_id=$2 AND phase IN ('draining','reviewing','interrupted')`, taskID, attemptID))
		if err != nil || current == nil {
			return err
		}
		var input CompletionInput
		if err := json.Unmarshal(current.Snapshot, &input); err != nil {
			return err
		}
		if reopen && (current.Phase != "interrupted" || input.RecoveryPending) {
			for _, work := range input.Work {
				if work.State != "running" {
					continue
				}
				if _, err := tx.ExecContext(ctx, `UPDATE exploration_nodes SET state='open',completed_at=NULL
				 WHERE id=$1 AND kind='intent' AND state IN ('stopped','exhausted')`, work.IntentID); err != nil {
					return err
				}
			}
		}
		input.RecoveryPending = !reopen
		raw, err := json.Marshal(input)
		if err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `UPDATE task_completions SET phase='interrupted',reason=$3,snapshot=$4,updated_at=now()
		 WHERE task_id=$1 AND attempt_id=$2`, taskID, attemptID, reason, string(raw))
		return err
	})
}

// RecoverTaskCompletion must run before startup resets running intents. A prior
// process cannot still own a valid completion decision after restart.
func (d *DB) RecoverTaskCompletion(ctx context.Context, taskID int64) error {
	current, err := d.TaskCompletion(ctx, taskID)
	if err != nil || current == nil {
		return err
	}
	return d.InterruptTaskCompletion(ctx, taskID, current.AttemptID, "进程重启，旧收官决定失效；保留证据并重新复核", true)
}
