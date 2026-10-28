package db

import (
	"context"
	"strings"
)

// SetAgentBindings applies a partial binding patch atomically. Missing fields
// are preserved; an explicitly empty Skill selection still records its
// initialization boundary so a restart cannot seed it back on.
func (d *DB) SetAgentBindings(ctx context.Context, agentID int64, mcps *[]int64, skills *[]string) error {
	tx, err := d.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err = lockSkillVisibilityAgent(tx, agentID); err != nil {
		return err
	}
	if mcps != nil {
		if *mcps == nil {
			empty := []int64{}
			mcps = &empty
		}
		if _, err = tx.ExecContext(ctx, `UPDATE agent_visibility SET enabled=false WHERE agent_id=$1 AND resource_kind='mcp' AND NOT(resource_id=ANY($2::bigint[]))`, agentID, *mcps); err != nil {
			return err
		}
		for _, id := range *mcps {
			var enabled bool
			if err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM agent_visibility WHERE agent_id=$1 AND resource_kind='mcp' AND resource_id=$2 AND enabled)`, agentID, id).Scan(&enabled); err != nil {
				return err
			}
			if enabled {
				continue
			}
			if _, err = tx.ExecContext(ctx, `INSERT INTO agent_visibility(agent_id,resource_kind,resource_id,enabled) VALUES($1,'mcp',$2,true) ON CONFLICT(agent_id,resource_kind,resource_id,mcp_tool_name) DO UPDATE SET enabled=true`, agentID, id); err != nil {
				return err
			}
		}
	}
	if skills != nil {
		clean := make([]string, 0, len(*skills))
		for _, name := range *skills {
			if name = strings.TrimSpace(name); name != "" {
				clean = append(clean, name)
			}
		}
		skills = &clean
		if _, err = tx.ExecContext(ctx, `UPDATE agent_skill_visibility SET enabled=false WHERE agent_id=$1 AND NOT(skill_name=ANY($2::text[]))`, agentID, *skills); err != nil {
			return err
		}
		for _, name := range *skills {
			name = strings.TrimSpace(name)
			if name == "" {
				continue
			}
			if _, err = tx.ExecContext(ctx, `INSERT INTO agent_skill_visibility(agent_id,skill_name,enabled) VALUES($1,$2,true) ON CONFLICT(agent_id,skill_name) DO UPDATE SET enabled=true`, agentID, name); err != nil {
				return err
			}
		}
		if err = markSkillVisibilityConfigured(tx, agentID); err != nil {
			return err
		}
	}
	return tx.Commit()
}
