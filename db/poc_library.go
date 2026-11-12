package db

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"github.com/Autumn-27/artex/kb"
	_ "modernc.org/sqlite"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
)

func (d *DB) KBEntries(ctx context.Context) ([]kb.Entry, error) {
	rows, err := d.QueryContext(ctx, `SELECT id,code,body,polarity,hit_count,used_on,created_at,updated_at FROM kb_entries ORDER BY id DESC LIMIT 10000`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []kb.Entry{}
	for rows.Next() {
		var id, h int64
		var code, p, u string
		var b []byte
		var c, m time.Time
		if err = rows.Scan(&id, &code, &b, &p, &h, &u, &c, &m); err != nil {
			return nil, err
		}
		var e kb.Entry
		if err = json.Unmarshal(b, &e); err != nil {
			return nil, err
		}
		e.ID = id
		e.Code = code
		e.Polarity = p
		e.HitCount = h
		e.UsedOn = u
		e.CreatedAt = c.Format(time.RFC3339)
		e.UpdatedAt = m.Format(time.RFC3339)
		e.HasContent = e.Content != ""
		out = append(out, e)
	}
	return out, rows.Err()
}
func (d *DB) KBGet(ctx context.Context, code string, count bool, usedOn string) (kb.Entry, error) {
	var e kb.Entry
	var b []byte
	var c, m time.Time
	var row *sql.Row
	if count {
		row = d.QueryRowContext(ctx, `UPDATE kb_entries SET hit_count=hit_count+1,used_on=$2,updated_at=now() WHERE code=$1 AND (source_finding_id IS NULL OR EXISTS(SELECT 1 FROM findings f WHERE f.id=source_finding_id AND (body->>'verified'!='true' OR f.verification_status='verified'))) RETURNING id,body,polarity,hit_count,used_on,created_at,updated_at`, code, usedOn)
	} else {
		row = d.QueryRowContext(ctx, `SELECT id,body,polarity,hit_count,used_on,created_at,updated_at FROM kb_entries WHERE code=$1 AND (source_finding_id IS NULL OR EXISTS(SELECT 1 FROM findings f WHERE f.id=source_finding_id AND (body->>'verified'!='true' OR f.verification_status='verified')))`, code)
	}
	var id, h int64
	var p, u string
	if err := row.Scan(&id, &b, &p, &h, &u, &c, &m); err != nil {
		return e, err
	}
	if err := json.Unmarshal(b, &e); err != nil {
		return e, err
	}
	e.ID = id
	e.Code = code
	e.Polarity = p
	e.HitCount = h
	e.UsedOn = u
	e.CreatedAt = c.Format(time.RFC3339)
	e.UpdatedAt = m.Format(time.RFC3339)
	e.HasContent = e.Content != ""
	return e, nil
}
func (d *DB) KBPut(ctx context.Context, e kb.Entry) (kb.Entry, bool, error) {
	if err := e.Validate(); err != nil {
		return e, false, err
	}
	b, err := json.Marshal(e)
	if err != nil {
		return e, false, err
	}
	var created bool
	err = d.QueryRowContext(ctx, `INSERT INTO kb_entries(code,body,polarity,source_finding_id) VALUES($1,$2::jsonb,$3,$4) ON CONFLICT(code) DO UPDATE SET body=EXCLUDED.body,polarity=EXCLUDED.polarity,source_finding_id=EXCLUDED.source_finding_id,updated_at=now() RETURNING id,(xmax=0)`, e.Code, b, e.Polarity, nilIfZero(e.SourceFindingID)).Scan(&e.ID, &created)
	return e, created, err
}

// ImportLegacyKB reads only the configured database, never follows its POC paths,
// and never executes imported content. Existing human edits are preserved.
func (d *DB) ImportLegacyKB(ctx context.Context, dir string) (int, error) {
	p := filepath.Join(dir, "knowledge.db")
	fi, err := os.Lstat(p)
	if err != nil {
		return 0, err
	}
	if !fi.Mode().IsRegular() || fi.Size() > 256<<20 {
		return 0, fmt.Errorf("knowledge.db must be a regular database under 256 MiB")
	}
	metadata := map[string]struct {
		Polarity string `json:"polarity"`
		Category string `json:"category"`
	}{}
	mp := filepath.Join(dir, "entry-meta.json")
	if fi, err := os.Lstat(mp); err == nil {
		if !fi.Mode().IsRegular() || fi.Size() > 1<<20 {
			return 0, fmt.Errorf("invalid entry metadata file")
		}
		b, err := os.ReadFile(mp)
		if err != nil {
			return 0, err
		}
		if err = json.Unmarshal(b, &metadata); err != nil {
			return 0, fmt.Errorf("invalid entry metadata JSON")
		}
	}
	u := url.URL{Scheme: "file", Path: p}
	q := u.Query()
	q.Set("mode", "ro")
	u.RawQuery = q.Encode()
	s, err := sql.Open("sqlite", u.String())
	if err != nil {
		return 0, err
	}
	defer s.Close()
	s.SetMaxOpenConns(1)
	rows, err := s.QueryContext(ctx, `SELECT code,title,kind,category,cve,component,versions,severity,language,source,source_url,description,usage,content,path,verified,verified_note,hit_count,used_on,engagement_id,engagement_name,asset_target,found_by_agent,tags,created_by FROM poc LIMIT 10001`)
	if err != nil {
		return 0, err
	}
	defer rows.Close()
	tx, err := d.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	n := 0
	scanned := 0
	for rows.Next() {
		scanned++
		if scanned > 10000 {
			return 0, fmt.Errorf("knowledge import exceeds 10000 entries")
		}
		if n >= 10000 {
			return 0, fmt.Errorf("knowledge import exceeds 10000 entries")
		}
		vals := make([]sql.NullString, 25)
		dest := make([]any, len(vals))
		for i := range vals {
			dest[i] = &vals[i]
		}
		if err = rows.Scan(dest...); err != nil {
			return 0, err
		}
		f := func(i int) string { return vals[i].String }
		e := kb.Entry{Code: f(0), Title: f(1), Kind: f(2), Category: f(3), CVE: f(4), Component: f(5), Versions: f(6), Severity: f(7), Language: f(8), Source: f(9), SourceURL: f(10), Description: f(11), Usage: f(12), Content: f(13), Path: portablePOCPath(dir, f(14)), Verified: f(15) == "1", VerifiedNote: f(16), UsedOn: f(18), EngagementID: f(19), EngagementName: f(20), AssetTarget: f(21), FoundByAgent: f(22), Tags: f(23), CreatedBy: f(24)}
		e.Polarity = kb.GuessPolarity(e)
		if meta, ok := metadata[e.Code]; ok {
			if meta.Polarity == "positive" || meta.Polarity == "context" || meta.Polarity == "negative" {
				e.Polarity = meta.Polarity
			}
			if meta.Category != "" {
				e.Category = meta.Category
			}
		}
		b, er := json.Marshal(e)
		if er != nil {
			return 0, er
		}
		if len(b) > kb.MaxContent+(128<<10) {
			return 0, fmt.Errorf("knowledge entry too large")
		}
		res, er := tx.ExecContext(ctx, `INSERT INTO kb_entries(code,body,polarity) VALUES($1,$2::jsonb,$3) ON CONFLICT(code) DO NOTHING`, e.Code, b, e.Polarity)
		if er != nil {
			return 0, er
		}
		v, _ := res.RowsAffected()
		n += int(v)
	}
	if err = rows.Err(); err != nil {
		return 0, err
	}
	return n, tx.Commit()
}
func portablePOCPath(dir, p string) string {
	p = strings.ReplaceAll(p, "\\", "/")
	i := strings.LastIndex("/"+p, "/pocs/")
	if i < 0 {
		return ""
	}
	tail := ("/" + p)[i+6:]
	root := filepath.Join(dir, "pocs")
	out := filepath.Join(root, filepath.FromSlash(tail))
	rel, err := filepath.Rel(root, out)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return ""
	}
	if fi, err := os.Lstat(out); err != nil || !fi.Mode().IsRegular() {
		return ""
	}
	realRoot, e1 := filepath.EvalSymlinks(root)
	realOut, e2 := filepath.EvalSymlinks(out)
	if e1 != nil || e2 != nil {
		return ""
	}
	rel, err = filepath.Rel(realRoot, realOut)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return ""
	}
	return realOut
}

// KBSearchEntries ranks full stored text in PostgreSQL, but only materializes
// bounded metadata for candidate selection. Agent startup never loads every
// POC body into the process or sends full scripts implicitly.
func (d *DB) KBSearchEntries(ctx context.Context, q kb.Query) ([]kb.Entry, error) {
	terms := kb.Terms(q.Q)
	args := []any{}
	score := "1"
	if len(terms) > 0 {
		parts := []string{}
		for _, t := range terms {
			args = append(args, "%"+escapeLikeOperator(t)+"%")
			p := fmt.Sprintf("$%d", len(args))
			for _, f := range []struct {
				field  string
				weight int
			}{{"title", 20}, {"code", 10}, {"cve", 15}, {"component", 10}, {"tags", 6}, {"description", 3}, {"content", 1}} {
				parts = append(parts, fmt.Sprintf("CASE WHEN lower(COALESCE(body->>'%s','')) LIKE %s ESCAPE '\\' THEN %d ELSE 0 END", f.field, p, f.weight))
			}
		}
		score = strings.Join(parts, "+")
	}
	where := []string{"(source_finding_id IS NULL OR EXISTS(SELECT 1 FROM findings f WHERE f.id=source_finding_id AND (body->>'verified'!='true' OR f.verification_status='verified')))"}
	for _, f := range []struct{ field, value string }{{"category", q.Category}, {"kind", q.Kind}, {"polarity", q.Polarity}} {
		if f.value != "" {
			args = append(args, f.value)
			expr := "body->>'" + f.field + "'"
			if f.field == "polarity" {
				expr = "polarity"
			}
			where = append(where, fmt.Sprintf("%s=$%d", expr, len(args)))
		}
	}
	query := `SELECT id,code,jsonb_build_object('title',left(body->>'title',1000),'category',body->>'category','kind',body->>'kind','cve',body->>'cve','component',left(body->>'component',1000),'versions',left(body->>'versions',1000),'description',left(body->>'description',2000),'tags',left(body->>'tags',1000),'verified',COALESCE((body->>'verified')::boolean,false),'has_content',COALESCE(body->>'content','')!=''),polarity,hit_count,used_on,rank FROM (SELECT *,(` + score + `) AS rank FROM kb_entries WHERE ` + strings.Join(where, " AND ") + `) c WHERE rank>0 ORDER BY rank DESC,code LIMIT 400`
	rows, err := d.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []kb.Entry{}
	for rows.Next() {
		var e kb.Entry
		var b []byte
		if err = rows.Scan(&e.ID, &e.Code, &b, &e.Polarity, &e.HitCount, &e.UsedOn, &e.Score); err != nil {
			return nil, err
		}
		var v kb.Entry
		if err = json.Unmarshal(b, &v); err != nil {
			return nil, err
		}
		v.ID = e.ID
		v.Code = e.Code
		v.Polarity = e.Polarity
		v.HitCount = e.HitCount
		v.UsedOn = e.UsedOn
		v.Score = e.Score
		out = append(out, v)
	}
	return out, rows.Err()
}
func escapeLikeOperator(s string) string {
	return strings.NewReplacer("\\", "\\\\", "%", "\\%", "_", "\\_").Replace(s)
}
func (d *DB) KBStats(ctx context.Context) (int, int, int, int64, error) {
	var total, verified, negative int
	var reused int64
	err := d.QueryRowContext(ctx, `SELECT count(*),count(*) FILTER (WHERE body->>'verified'='true'),count(*) FILTER (WHERE polarity='negative'),COALESCE(sum(hit_count),0) FROM kb_entries`).Scan(&total, &verified, &negative, &reused)
	return total, verified, negative, reused, err
}

// Automatic capture never overwrites a human-edited entry with the same code.
func (d *DB) KBInsertIfAbsent(ctx context.Context, e kb.Entry) (kb.Entry, bool, error) {
	if err := e.Validate(); err != nil {
		return e, false, err
	}
	b, err := json.Marshal(e)
	if err != nil {
		return e, false, err
	}
	err = d.QueryRowContext(ctx, `INSERT INTO kb_entries(code,body,polarity,source_finding_id) VALUES($1,$2::jsonb,$3,$4) ON CONFLICT(code) DO NOTHING RETURNING id`, e.Code, b, e.Polarity, nilIfZero(e.SourceFindingID)).Scan(&e.ID)
	if err == sql.ErrNoRows {
		return e, false, nil
	}
	return e, err == nil, err
}
