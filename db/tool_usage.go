package db

// ToolUsage is one catalog tool invocation. It stores attribution dimensions only:
// tool arguments and results are deliberately excluded from the ledger.
type ToolUsage struct {
	ID            int64  `json:"id,omitempty"`
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
	err := d.QueryRow(`
INSERT INTO tool_usage(tool_key, agent_key, task_id, exploration_id, intent_id, session_id)
VALUES ($1, NULLIF($2,''), $3, $4, $5, NULLIF($6,'')) RETURNING id`,
		u.ToolKey, u.AgentKey, nullIfZero(u.TaskID), nullIfZero(u.ExplorationID),
		nullIfZero(u.IntentID), u.SessionID).Scan(&u.ID)
	return err
}

func (d *DB) FinishToolUsage(id int64, outcome string) error {
	_, err := d.Exec(`UPDATE tool_usage SET outcome=$2 WHERE id=$1`, id, outcome)
	return err
}

type ToolUsageSummary struct {
	Tool    string `json:"tool"`
	Agent   string `json:"agent"`
	Outcome string `json:"outcome"`
	Calls   int    `json:"calls"`
}

// Historical rows and calls interrupted before their return remain unknown.
func (d *DB) ToolUsageByAgent(taskID int64) ([]ToolUsageSummary, error) {
	rows, err := d.Query(`SELECT tool_key,COALESCE(agent_key,''),outcome,count(*) FROM tool_usage WHERE $1::bigint=0 OR task_id=$1 GROUP BY tool_key,agent_key,outcome ORDER BY tool_key,agent_key,outcome`, taskID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []ToolUsageSummary{}
	for rows.Next() {
		var row ToolUsageSummary
		if err = rows.Scan(&row.Tool, &row.Agent, &row.Outcome, &row.Calls); err != nil {
			return nil, err
		}
		out = append(out, row)
	}
	return out, rows.Err()
}

// ToolUsageCounts returns invocation totals keyed by catalog tool key. Entries
// without calls are absent; API callers merge the result into the tools catalog.
func (d *DB) ToolUsageCounts() (map[string]int, error) {
	rows, err := d.Query(`
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
	archived, err := d.archivedTaskAggregates()
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
