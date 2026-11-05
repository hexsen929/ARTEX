package db

// These are handler-owned limits, not editable descriptions or bindings. Repair
// existing schema metadata as well as new seeds so models can correct long input.
func (d *DB) refreshVerificationLimits() error {
	_, err := d.Exec(`UPDATE tools SET schema=jsonb_set(jsonb_set(schema,
'{properties,note,maxLength}','4000'::jsonb),'{properties,additional_evidence,maxLength}','32000'::jsonb)
WHERE system=true AND key='set_finding_verification'
AND jsonb_typeof(schema->'properties'->'note')='object'
AND jsonb_typeof(schema->'properties'->'additional_evidence')='object'
AND (schema#>>'{properties,note,maxLength}' IS DISTINCT FROM '4000' OR schema#>>'{properties,additional_evidence,maxLength}' IS DISTINCT FROM '32000')`)
	return err
}
