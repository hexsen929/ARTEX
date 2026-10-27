package db

import (
	"database/sql"
	"encoding/json"
	"sort"
	"time"
)

// taskArchiveAggregate is the compact, non-sensitive statistics section of a
// cold archive manifest. Fields are additive so older archive formats remain
// readable when new dashboard dimensions are introduced.
type taskArchiveAggregate struct {
	TokenProfiles     []ProfileUsage    `json:"token_profiles"`
	TokenDaily        []ProfileDayUsage `json:"token_daily"`
	Skills            map[string]int    `json:"skills"`
	SkillStats        []SkillStat       `json:"skill_stats"`
	MissingSkillStats []SkillStat       `json:"missing_skill_stats"`
	// New archives retain non-sensitive event dimensions so a notice cleared
	// between two calls to the same skill can be excluded precisely. Old archives
	// remain readable through their aggregate last_used timestamps.
	MissingSkillEvents []archivedMissingSkillEvent `json:"missing_skill_events"`
	Tools              map[string]int              `json:"tools"`
	FindingStats       FindingStats                `json:"finding_stats"`
}

type archivedMissingSkillEvent struct {
	Skill    string    `json:"skill"`
	AgentKey string    `json:"agent_key"`
	TS       time.Time `json:"ts"`
}

func archivedMissingSkillAggregates(q interface {
	Query(string, ...any) (*sql.Rows, error)
}) ([]taskArchiveAggregate, error) {
	rows, err := q.Query(`SELECT archive.aggregate_stats
FROM task_archives archive
JOIN tasks task ON task.id=archive.task_id
WHERE task.archived_at IS NOT NULL`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []taskArchiveAggregate{}
	for rows.Next() {
		var raw json.RawMessage
		if err := rows.Scan(&raw); err != nil {
			return nil, err
		}
		var aggregate taskArchiveAggregate
		if err := json.Unmarshal(raw, &aggregate); err != nil {
			return nil, err
		}
		out = append(out, aggregate)
	}
	return out, rows.Err()
}

func mergeUnacknowledgedArchivedSkillStats(live []SkillStat, archived []taskArchiveAggregate, cutoff *time.Time) []SkillStat {
	if cutoff == nil {
		return mergeArchivedSkillStats(live, archived, true)
	}
	filtered := make([]taskArchiveAggregate, 0, len(archived))
	for _, archive := range archived {
		if archive.MissingSkillEvents != nil {
			byName := map[string]SkillStat{}
			for _, event := range archive.MissingSkillEvents {
				if !event.TS.After(*cutoff) {
					continue
				}
				current := byName[event.Skill]
				current.Skill = event.Skill
				current.Calls++
				current.Agents = mergeAgentKeys(current.Agents, []string{event.AgentKey})
				if current.LastUsed == nil || event.TS.After(*current.LastUsed) {
					timestamp := event.TS
					current.LastUsed = &timestamp
				}
				byName[event.Skill] = current
			}
			archive.MissingSkillStats = nil
			for _, current := range byName {
				archive.MissingSkillStats = append(archive.MissingSkillStats, current)
			}
		} else {
			// Legacy cold summaries predate acknowledgement support. Normal old
			// archives are wholly before the first clear. A newly imported legacy
			// summary with later activity remains visible as a historical aggregate;
			// the old format does not contain per-event times to split it further.
			var retained []SkillStat
			for _, stat := range archive.MissingSkillStats {
				if stat.LastUsed != nil && stat.LastUsed.After(*cutoff) {
					retained = append(retained, stat)
				}
			}
			archive.MissingSkillStats = retained
		}
		filtered = append(filtered, archive)
	}
	return mergeArchivedSkillStats(live, filtered, true)
}

func (d *DB) archivedTaskAggregates() ([]taskArchiveAggregate, error) {
	rawItems, err := d.ArchivedAggregateStats()
	if err != nil {
		return nil, err
	}
	out := make([]taskArchiveAggregate, 0, len(rawItems))
	for _, raw := range rawItems {
		var item taskArchiveAggregate
		if err := json.Unmarshal(raw, &item); err != nil {
			return nil, err
		}
		out = append(out, item)
	}
	return out, nil
}

func mergeAgentKeys(current, incoming []string) []string {
	seen := make(map[string]bool, len(current)+len(incoming))
	out := make([]string, 0, len(current)+len(incoming))
	for _, group := range [][]string{current, incoming} {
		for _, key := range group {
			if key == "" || seen[key] {
				continue
			}
			seen[key] = true
			out = append(out, key)
		}
	}
	return out
}

func mergeArchivedSkillStats(live []SkillStat, archived []taskArchiveAggregate, missing bool) []SkillStat {
	byName := make(map[string]SkillStat, len(live))
	for _, current := range live {
		byName[current.Skill] = current
	}
	for _, aggregate := range archived {
		coldStats := aggregate.SkillStats
		if missing {
			coldStats = aggregate.MissingSkillStats
		} else if len(coldStats) == 0 {
			// Format v1 initially retained only the call-count map. Preserve those
			// summaries even though agent and timestamp dimensions are unavailable.
			for skill, calls := range aggregate.Skills {
				coldStats = append(coldStats, SkillStat{Skill: skill, Calls: calls, Tasks: 1})
			}
		}
		for _, cold := range coldStats {
			current := byName[cold.Skill]
			current.Skill = cold.Skill
			current.Calls += cold.Calls
			current.Tasks += cold.Tasks
			current.Agents = mergeAgentKeys(current.Agents, cold.Agents)
			if cold.LastUsed != nil && (current.LastUsed == nil || cold.LastUsed.After(*current.LastUsed)) {
				current.LastUsed = cold.LastUsed
			}
			byName[cold.Skill] = current
		}
	}
	out := make([]SkillStat, 0, len(byName))
	for _, current := range byName {
		out = append(out, current)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Calls != out[j].Calls {
			return out[i].Calls > out[j].Calls
		}
		return out[i].Skill < out[j].Skill
	})
	return out
}
