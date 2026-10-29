package db

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
)

func assetMapperDefaultsKey(agentID int64) string {
	return fmt.Sprintf("asset_mapper_defaults_initialized_v1:%d", agentID)
}

// SeedAssetMapperIfMissing creates this new bounded role together with its first
// prompt and two read-only tool grants. The server supplies the public prompt
// and real tool schemas to avoid a package cycle or duplicated defaults here.
// Any pre-existing same-key role is an explicit user choice: even a disabled
// role, empty prompt, empty binding selection or absent marker stays untouched.
func (d *DB) SeedAssetMapperIfMissing(template string, readers []Tool) (bool, error) {
	if strings.TrimSpace(template) == "" || len(readers) != 2 {
		return false, errors.New("资产梳理默认配置不完整")
	}
	defaults := append([]Tool(nil), readers...)
	sort.Slice(defaults, func(i, j int) bool { return defaults[i].Key < defaults[j].Key })
	if defaults[0].Key != "list_assets" || defaults[1].Key != "list_companies" {
		return false, errors.New("资产梳理默认工具必须是两项只读目录查询")
	}
	for _, reader := range defaults {
		var schema map[string]json.RawMessage
		if err := json.Unmarshal(reader.Schema, &schema); err != nil || schema == nil {
			return false, errors.New("资产梳理默认工具 schema 无效")
		}
	}
	tx, err := d.Begin()
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	var agentID int64
	err = tx.QueryRow(`INSERT INTO agents(key,name,description,role,builtin,enabled,interactive_shell,web_search,max_turns,run_seconds)
VALUES('asset_mapper','资产梳理','只读查询已登记资产与企业，不执行命令或外部探测。','assistant',true,true,false,false,8,1200)
ON CONFLICT(key) DO NOTHING RETURNING id`).Scan(&agentID)
	if errors.Is(err, sql.ErrNoRows) {
		return false, tx.Commit()
	}
	if err != nil {
		return false, err
	}
	var promptID int64
	if err := tx.QueryRow(`INSERT INTO agent_prompts(agent_id,version,template_text,note,updated_by)
VALUES($1,1,$2,'内置只读资产梳理默认','system') RETURNING id`, agentID, template).Scan(&promptID); err != nil {
		return false, err
	}
	if _, err := tx.Exec(`UPDATE agents SET current_prompt_id=$2 WHERE id=$1`, agentID, promptID); err != nil {
		return false, err
	}
	for _, reader := range defaults {
		bindings := append([]string(nil), reader.Agents...)
		hasMapper := false
		for _, key := range bindings {
			hasMapper = hasMapper || key == "asset_mapper"
		}
		if !hasMapper {
			bindings = append(bindings, "asset_mapper")
		}
		rawBindings, _ := json.Marshal(bindings)
		var key string
		err := tx.QueryRow(`INSERT INTO tools(key,system,description,schema,agents,enabled,kind)
VALUES($1,true,$2,$3,$4,true,'builtin')
ON CONFLICT(key) DO UPDATE SET agents=CASE
  WHEN tools.agents ? 'asset_mapper' THEN tools.agents
  ELSE tools.agents || '["asset_mapper"]'::jsonb END
WHERE tools.system AND tools.kind='builtin' AND jsonb_typeof(tools.agents)='array'
RETURNING key`, reader.Key, reader.Description, reader.Schema, string(rawBindings)).Scan(&key)
		if errors.Is(err, sql.ErrNoRows) {
			return false, fmt.Errorf("资产梳理只读工具 %s 的已有配置不兼容", reader.Key)
		}
		if err != nil {
			return false, err
		}
	}
	if _, err := tx.Exec(`INSERT INTO settings(key,value) VALUES($1,'true') ON CONFLICT(key) DO NOTHING`, assetMapperDefaultsKey(agentID)); err != nil {
		return false, err
	}
	if err := tx.Commit(); err != nil {
		return false, err
	}
	return true, nil
}
