package db

import (
	"database/sql"
	"errors"
	"fmt"
	"sort"
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

// SeedBuiltinSkillVisibility initializes defaults only for existing Agents that
// have never had a Skill selection. Any pre-existing visibility row (including
// a false row) is an explicit configuration and is preserved as a whole. Missing
// auxiliary Agents are left unmarked so the server can seed them immediately
// after creating them on the same first startup.
func (d *DB) SeedBuiltinSkillVisibility() error {
	byAgent := map[string][]string{}
	for name, keys := range builtinSkillVisibility {
		for _, key := range keys {
			byAgent[key] = append(byAgent[key], name)
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
