package db

import "encoding/json"

// Add missing fields and batch item properties without resetting descriptions,
// defaults, existing constraints, role bindings or the enabled flag.
func (d *DB) MergePlanningToolSchema(defaults json.RawMessage) error {
	tx, err := d.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`UPDATE tools SET schema=jsonb_set(schema,'{properties}',($1::jsonb->'properties')||(schema->'properties'))
WHERE system=true AND key='add_intent' AND jsonb_typeof(schema->'properties')='object'`, string(defaults)); err != nil {
		return err
	}
	if _, err := tx.Exec(`UPDATE tools SET schema=jsonb_set(schema,'{properties,intents,items,properties}',
($1::jsonb#>'{properties,intents,items,properties}')||coalesce(schema#>'{properties,intents,items,properties}','{}'::jsonb))
WHERE system=true AND key='add_intent' AND jsonb_typeof(schema#>'{properties,intents,items}')='object'
AND ($1::jsonb#>'{properties,intents,items,properties}') IS NOT NULL`, string(defaults)); err != nil {
		return err
	}
	return tx.Commit()
}
