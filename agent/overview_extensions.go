package agent

import (
	"fmt"

	"github.com/Autumn-27/artex/db"
)

// Keep the current bounded overview contract while adding the fork's metadata:
// no full PoCs or unbounded asset lists are repeatedly copied into each prompt.
func (t *ToolSet) enrichFindingOverview(out map[string]any, findings []map[string]any) {
	counts, _ := out["finding_verification_counts"].(map[string]int)
	out["findings"] = counts[db.VerificationConfirmed]
	if t.as == nil || t.taskID <= 0 {
		return
	}
	if metadata, err := t.as.FindingMetaByNodeID(t.taskID); err == nil {
		for _, row := range findings {
			id, _ := row["id"].(int64)
			meta, ok := metadata[id]
			if !ok {
				continue
			}
			row["status"] = meta.Status // triage only, never used as verification
			ids := meta.AssetIDs
			if len(ids) > 8 {
				ids = ids[:8]
				row["assets_truncated"] = true
			}
			assets, _ := t.as.GetByIDs(ids)
			byID := make(map[int64]*db.Asset, len(assets))
			for _, a := range assets {
				byID[a.ID] = a
			}
			labels := make([]string, 0, len(ids))
			for _, assetID := range ids {
				label := ""
				if a := byID[assetID]; a != nil {
					label = assetValue(a)
				}
				if label == "" {
					label = fmt.Sprintf("#%d", assetID)
				}
				labels = append(labels, firstLine(label, 500))
			}
			if len(labels) > 0 {
				row["assets"] = labels
			}
		}
	}
	if pots, err := t.as.HoneypotAssetsByTask(t.taskID, 20); err == nil && len(pots) > 0 {
		rows := make([]map[string]any, 0, len(pots))
		for _, p := range pots {
			rows = append(rows, map[string]any{"id": p.ID, "asset": firstLine(p.Label, 500), "score": p.Score, "signatures": firstLine(p.Evidence, 1000)})
		}
		out["honeypot_assets"] = rows
	}
	// Scope is a boundary, not a coverage metric, and remains available even
	// when task coverage accounting is disabled. Large scopes stay paged/bounded.
	if scope, err := t.as.ListTaskScopeWithSources(t.taskID); err == nil && len(scope) > 0 {
		out["task_scope_total"] = len(scope)
		if len(scope) > 20 {
			scope = scope[:20]
			out["task_scope_truncated"] = true
		}
		rows := make([]map[string]any, 0, len(scope))
		for _, s := range scope {
			value := s.Domain
			if value == "" {
				value = s.Net
			}
			if value == "" {
				value = s.Value
			}
			r := map[string]any{"kind": s.Kind, "value": firstLine(value, 500), "source": s.Source, "task_id": s.TaskID}
			if s.CompanyID != nil {
				r["company_id"] = *s.CompanyID
			}
			if s.TaskID != t.taskID {
				inheritedMap(r, s.TaskID)
			}
			rows = append(rows, r)
		}
		out["task_scope"] = rows
	}
}

func assetValue(a *db.Asset) string {
	switch {
	case a.URL != "":
		if a.Method != "" {
			return a.Method + " " + a.URL
		}
		return a.URL
	case a.Domain != "":
		if a.Port != nil {
			return fmt.Sprintf("%s:%d", a.Domain, *a.Port)
		}
		return a.Domain
	case a.IP != "":
		if a.Port != nil {
			return fmt.Sprintf("%s:%d", a.IP, *a.Port)
		}
		return a.IP
	case a.AppName != "":
		return a.AppName
	case a.ServiceName != "":
		return a.ServiceName
	}
	return ""
}
