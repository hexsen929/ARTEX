package db

// The persisted goal audit is the authority for this label. Only a verdict for
// the current evidence and task contract is usable; historical review survives
// for audit but disappears from the label when an input changes. Callers alias
// the standalone finding as f.
const findingEvidenceAssessmentSQL = `CASE WHEN btrim(f.evidence)='' THEN 'needs_evidence' ELSE COALESCE((
 SELECT CASE a->>'event' WHEN 'prove' THEN 'supported_for_goal' ELSE 'unsupported_for_goal' END
 FROM exploration_nodes g
 JOIN tasks owner ON owner.exploration_id=g.exploration_id AND owner.deleted_at IS NULL
 JOIN exploration_nodes ev ON ev.id=f.node_id
 CROSS JOIN LATERAL jsonb_array_elements(g.goal_audit) a
 WHERE g.kind='goal' AND g.state IN ('open','met') AND ev.state='confirmed' AND a->>'event' IN ('prove','reject')
 AND (a->>'evidence_id')::bigint=f.node_id
 AND NOT EXISTS(SELECT 1 FROM jsonb_array_elements(g.goal_audit) revision WHERE revision->>'event' IN ('edit','reopen','scope_edit') AND (revision->>'goal_version')::integer>=(a->>'goal_version')::integer)
 AND g.payload=a->'snapshot'->'goal'->'payload'
 AND ev.payload=a->'snapshot'->'evidence'->'payload'
 AND ev.content_version=(a->'snapshot'->'evidence'->>'content_version')::integer
 AND COALESCE(a->'snapshot'->'supporting_facts','[]'::jsonb)=` + supportingFactsSQL + `
 AND owner.goal=a->'snapshot'->>'task_goal' AND owner.description=a->'snapshot'->>'task_description'
 AND a->'snapshot'->'finding' @> jsonb_build_array(jsonb_build_object('id',f.id,'evidence_version',f.evidence_version,'status',f.status,'evidence',f.evidence,'summary',f.summary,'vulnclass',f.vulnclass))
 AND a->'snapshot'->'constraints'=(SELECT COALESCE(jsonb_agg(to_jsonb(c) ORDER BY c.id),'[]'::jsonb) FROM task_constraints c WHERE c.exploration_id=g.exploration_id)
 AND a->'snapshot'->'scopes'=(SELECT COALESCE(jsonb_agg(to_jsonb(sc) ORDER BY sc.id),'[]'::jsonb) FROM task_scope sc WHERE sc.task_id=owner.id)
 AND (a->>'event'='reject' OR (g.state='met' AND EXISTS(SELECT 1 FROM exploration_edges pe WHERE pe.exploration_id=g.exploration_id AND pe.src_id=f.node_id AND pe.rel='proves' AND pe.dst_id=g.id)))
 ORDER BY a->>'at' DESC LIMIT 1
),'needs_review') END`
