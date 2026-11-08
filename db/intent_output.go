package db

import "database/sql"

// LatestIntentOutput reads the final result directly rather than scanning an
// oldest-first activity page. Source tasks expose only settled intent outputs.
func (s *ExplorationStore) LatestIntentOutput(id int64) (*Activity, error) {
	if node, err := s.GetNode(id); err != nil {
		return nil, err
	} else if node != nil {
		return s.latestIntentOutput(id, false)
	}
	sources, err := s.DirectSourceStores()
	if err != nil {
		return nil, err
	}
	for _, source := range sources {
		node, err := source.Store.GetNode(id)
		if err != nil {
			return nil, err
		}
		if node == nil || node.Kind != KindIntent {
			continue
		}
		result, err := source.Store.latestIntentOutput(id, true)
		if result != nil {
			result.SourceTaskID = source.Task.TaskID
			result.Inherited = true
		}
		return result, err
	}
	return nil, nil
}

func (s *ExplorationStore) latestIntentOutput(id int64, settledOnly bool) (*Activity, error) {
	var a Activity
	err := s.db.QueryRow(`SELECT a.id,a.node_id,coalesce(a.worker,''),a.kind,a.is_error,coalesce(a.summary,''),coalesce(a.detail,''),a.created_at
FROM activity a JOIN exploration_nodes n ON n.id=a.node_id AND n.exploration_id=a.exploration_id
WHERE a.exploration_id=$1 AND n.id=$2 AND n.kind='intent' AND a.kind IN ('result','text')
AND (NOT $3 OR n.state IN ('done','blocked','exhausted','stopped'))
ORDER BY CASE WHEN a.kind='result' THEN 0 ELSE 1 END,a.id DESC LIMIT 1`, s.expID, id, settledOnly).Scan(&a.ID, &a.NodeID, &a.Worker, &a.Kind, &a.IsError, &a.Summary, &a.Detail, &a.CreatedAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &a, nil
}
