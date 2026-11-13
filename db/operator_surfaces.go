package db

import (
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"strings"
)

//go:embed operator_surface_categories.json
var operatorSurfaceCategories []byte

type FindingSurfaceCategory struct {
	Key      string   `json:"key"`
	Label    string   `json:"label"`
	Keywords []string `json:"keywords"`
}

func FindingSurfaceCategories() []FindingSurfaceCategory {
	var v []FindingSurfaceCategory
	_ = json.Unmarshal(operatorSurfaceCategories, &v)
	return v
}
func ValidFindingSurface(key string) bool {
	for _, v := range FindingSurfaceCategories() {
		if key == v.Key {
			return true
		}
	}
	return false
}
func SurfaceKeyFor(class string) string {
	class = strings.ToLower(strings.TrimSpace(class))
	for _, v := range FindingSurfaceCategories() {
		for _, word := range v.Keywords {
			if strings.Contains(class, word) {
				return v.Key
			}
		}
	}
	return "other"
}
func (d *DB) SurfaceOverrides(ctx context.Context) (map[string]string, error) {
	rows, err := d.QueryContext(ctx, "SELECT vulnclass,surface FROM finding_surface_overrides")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var class, surface string
		if err = rows.Scan(&class, &surface); err != nil {
			return nil, err
		}
		if ValidFindingSurface(surface) {
			out[class] = surface
		}
	}
	return out, rows.Err()
}
func (d *DB) UpsertSurfaceOverrides(ctx context.Context, values map[string]string) error {
	if len(values) > 512 {
		return fmt.Errorf("surface mapping exceeds 512 classes")
	}
	tx, err := d.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for class, surface := range values {
		if strings.TrimSpace(class) == "" || len(class) > 1000 || !ValidFindingSurface(surface) {
			return fmt.Errorf("invalid surface mapping")
		}
		if _, err = tx.ExecContext(ctx, `INSERT INTO finding_surface_overrides(vulnclass,surface) VALUES($1,$2) ON CONFLICT(vulnclass) DO UPDATE SET surface=EXCLUDED.surface,updated_at=now()`, class, surface); err != nil {
			return err
		}
	}
	return tx.Commit()
}
func (d *DB) FindingVulnClassList(ctx context.Context) ([]string, error) {
	rows, err := d.QueryContext(ctx, "SELECT DISTINCT vulnclass FROM findings WHERE trim(vulnclass)!='' ORDER BY vulnclass LIMIT 4096")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []string{}
	for rows.Next() {
		var v string
		if err = rows.Scan(&v); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}
func (d *DB) applyFindingSurfaces(ctx context.Context, records []*DBFinding) error {
	if len(records) == 0 {
		return nil
	}
	overrides, err := d.SurfaceOverrides(ctx)
	if err != nil {
		return err
	}
	for _, f := range records {
		f.Surface = overrides[f.VulnClass]
		if f.Surface == "" {
			f.Surface = SurfaceKeyFor(f.VulnClass)
		}
	}
	return nil
}
func surfaceWhereSQL() string {
	// Both the categories and column expression are code-owned. Request values
	// remain bound parameters in FindingFilter.where().
	quote := func(s string) string { return "'" + strings.ReplaceAll(s, "'", "''") + "'" }
	parts := []string{"CASE"}
	for _, v := range FindingSurfaceCategories() {
		if len(v.Keywords) == 0 {
			continue
		}
		terms := []string{}
		for _, word := range v.Keywords {
			terms = append(terms, "strpos(lower(trim(f.vulnclass)),"+quote(word)+")>0")
		}
		parts = append(parts, "WHEN ("+strings.Join(terms, " OR ")+") THEN "+quote(v.Key))
	}
	parts = append(parts, "ELSE 'other' END")
	return "COALESCE((SELECT NULLIF(surface,'') FROM finding_surface_overrides so WHERE so.vulnclass=f.vulnclass)," + strings.Join(parts, " ") + ")"
}
func (d *DB) FindingSurfaceCounts(ctx context.Context, filter FindingFilter) (map[string]int, error) {
	filter.Surface = ""
	f, err := d.applyAssetScope(filter)
	if err != nil {
		return nil, err
	}
	where, args := f.where()
	rows, err := d.QueryContext(ctx, "SELECT "+surfaceWhereSQL()+" AS surface,count(*) FROM findings f"+where+" GROUP BY surface", args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]int{}
	for _, v := range FindingSurfaceCategories() {
		out[v.Key] = 0
	}
	for rows.Next() {
		var key string
		var n int
		if err = rows.Scan(&key, &n); err != nil {
			return nil, err
		}
		out[key] += n
	}
	return out, rows.Err()
}
