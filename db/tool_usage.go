package db

import (
	"context"
	"database/sql"
)

// ToolUsage is one catalog tool invocation. It stores attribution dimensions only:
// tool arguments and results are deliberately excluded from the ledger.
type ToolUsage struct {
	ToolKey       string `json:"tool_key"`
	AgentKey      string `json:"agent_key"`
	TaskID        int64  `json:"task_id"`
	ExplorationID int64  `json:"exploration_id"`
	IntentID      int64  `json:"intent_id"`
	SessionID     string `json:"session_id"`
}

// InsertToolUsage appends one ledger row. Runtime callers treat metering as
// best-effort so a statistics failure never interrupts the tool itself.
func (d *DB) InsertToolUsage(u *ToolUsage) error {
	_, err := d.Exec(`
INSERT INTO tool_usage(tool_key, agent_key, task_id, exploration_id, intent_id, session_id)
VALUES ($1, NULLIF($2,''), $3, $4, $5, NULLIF($6,''))`,
		u.ToolKey, u.AgentKey, nullIfZero(u.TaskID), nullIfZero(u.ExplorationID),
		nullIfZero(u.IntentID), u.SessionID)
	return err
}

// ToolUsageCounts returns invocation totals keyed by catalog tool key. Entries
// without calls are absent; API callers merge the result into the tools catalog.
func (d *DB) ToolUsageCounts() (map[string]int, error) {
	// An archive/restore atomically moves rows between hot storage and its cold
	// aggregate. Both reads must observe one snapshot or a move between them can
	// count the same invocation twice (or miss it entirely).
	tx, err := d.BeginTx(context.Background(), &sql.TxOptions{Isolation: sql.LevelRepeatableRead, ReadOnly: true})
	if err != nil {
		return nil, err
	}
	defer tx.Rollback() //nolint:errcheck
	rows, err := tx.Query(`
SELECT tool_key, COUNT(*)
FROM tool_usage
GROUP BY tool_key`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]int{}
	for rows.Next() {
		var key string
		var calls int
		if err := rows.Scan(&key, &calls); err != nil {
			return nil, err
		}
		out[key] = calls
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	archived, err := archivedTaskAggregatesFrom(tx)
	if err != nil {
		return nil, err
	}
	for _, aggregate := range archived {
		for key, calls := range aggregate.Tools {
			out[key] += calls
		}
	}
	return out, nil
}
