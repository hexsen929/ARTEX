package db

import (
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"strings"
)

// A marker belongs to an Agent ID rather than just its role. Recreating a
// deleted Agent may initialize its defaults; restarting an existing Agent must
// never restore a skill that the user removed.
func skillVisibilityDefaultsKey(agentID int64) string {
	return fmt.Sprintf("skill_visibility_initialized_v1:%d", agentID)
}

func markSkillVisibilityConfigured(tx *sql.Tx, agentID int64) error {
	_, err := tx.Exec(`INSERT INTO settings(key,value) VALUES ($1,'true')
ON CONFLICT (key) DO NOTHING`, skillVisibilityDefaultsKey(agentID))
	return err
}

// lockSkillVisibilityAgent serializes first seeding and every explicit edit on
// the same Agent row. The marker and bindings are committed together.
func lockSkillVisibilityAgent(tx *sql.Tx, agentID int64) error {
	var id int64
	return tx.QueryRow(`SELECT id FROM agents WHERE id=$1 FOR UPDATE`, agentID).Scan(&id)
}

// BuiltinSkillDefaultNames returns the directory-name policy, not the contents
// or installation status of any Skill. Callers must inspect their actual root.
func BuiltinSkillDefaultNames() []string {
	names := make([]string, 0, len(builtinSkillVisibility))
	for name := range builtinSkillVisibility {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// BuiltinSkillDefaultsConfigured distinguishes an intentionally empty selection
// from a role that has never received an inspected default catalog. Existing
// visibility rows (including false tombstones) remain explicit choices even on
// older databases without a marker. Custom and data-only roles have no default
// Skill policy and do not depend on this marker.
func (d *DB) BuiltinSkillDefaultsConfigured(agentID int64, agentKey string) (bool, error) {
	if agentID <= 0 {
		return false, fmt.Errorf("invalid Agent ID for Skill defaults")
	}
	requiresDefaults := false
	for _, keys := range builtinSkillVisibility {
		for _, key := range keys {
			if key == agentKey {
				requiresDefaults = true
				break
			}
		}
	}
	if !requiresDefaults {
		return true, nil
	}
	if d == nil || d.DB == nil {
		return false, fmt.Errorf("Skill default configuration database unavailable")
	}
	var configured bool
	err := d.QueryRow(`SELECT EXISTS(SELECT 1 FROM settings WHERE key=$1)
OR EXISTS(SELECT 1 FROM agent_skill_visibility WHERE agent_id=$2)`, skillVisibilityDefaultsKey(agentID), agentID).Scan(&configured)
	return configured, err
}

// SeedBuiltinSkillVisibility initializes defaults only for existing Agents that
// have never had a Skill selection. Any pre-existing visibility row (including
// a false row) is an explicit configuration and is preserved as a whole. Missing
// auxiliary Agents are left unmarked so the server can seed them immediately
// after creating them on the same first startup. availableNames must be a
// successfully inspected catalog snapshot: nil means unknown and never marks
// roles as initialized; a non-nil empty slice is an inspected empty directory.
func (d *DB) SeedBuiltinSkillVisibility(availableNames []string) error {
	if availableNames == nil {
		return fmt.Errorf("Skill defaults require an inspected catalog")
	}
	available := make(map[string]bool, len(availableNames))
	for _, name := range availableNames {
		if name == "" || name != strings.TrimSpace(name) || strings.ContainsAny(name, `/\\`) {
			return fmt.Errorf("Skill default catalog contains an invalid directory name")
		}
		available[name] = true
	}
	byAgent := map[string][]string{}
	for name, keys := range builtinSkillVisibility {
		for _, key := range keys {
			// Include every policy role even when none of its defaults are
			// installed, so an inspected empty choice is marked once.
			if _, exists := byAgent[key]; !exists {
				byAgent[key] = []string{}
			}
			if available[name] {
				byAgent[key] = append(byAgent[key], name)
			}
		}
	}
	keys := make([]string, 0, len(byAgent))
	for key := range byAgent {
		keys = append(keys, key)
		sort.Strings(byAgent[key])
	}
	sort.Strings(keys)
	for _, key := range keys {
		if err := d.seedAgentSkillVisibility(key, byAgent[key]); err != nil {
			return fmt.Errorf("agent %s default skills: %w", key, err)
		}
	}
	return nil
}

func (d *DB) seedAgentSkillVisibility(key string, defaults []string) error {
	tx, err := d.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var agentID int64
	if err := tx.QueryRow(`SELECT id FROM agents WHERE key=$1 FOR UPDATE`, key).Scan(&agentID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		return err
	}
	var initialized bool
	if err := tx.QueryRow(`SELECT EXISTS(SELECT 1 FROM settings WHERE key=$1)`, skillVisibilityDefaultsKey(agentID)).Scan(&initialized); err != nil {
		return err
	}
	if initialized {
		return tx.Commit()
	}
	var configured bool
	if err := tx.QueryRow(`SELECT EXISTS(SELECT 1 FROM agent_skill_visibility WHERE agent_id=$1)`, agentID).Scan(&configured); err != nil {
		return err
	}
	if !configured {
		for _, name := range defaults {
			if _, err := tx.Exec(`INSERT INTO agent_skill_visibility(agent_id,skill_name,enabled)
VALUES ($1,$2,true) ON CONFLICT (agent_id,skill_name) DO NOTHING`, agentID, name); err != nil {
				return err
			}
		}
	}
	if err := markSkillVisibilityConfigured(tx, agentID); err != nil {
		return err
	}
	return tx.Commit()
}
