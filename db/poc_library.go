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
		row = d.QueryRowContext(ctx, `UPDATE kb_entries SET hit_count=hit_count+1,used_on=$2,updated_at=now() WHERE code=$1 RETURNING id,body,polarity,hit_count,used_on,created_at,updated_at`, code, usedOn)
	} else {
		row = d.QueryRowContext(ctx, `SELECT id,body,polarity,hit_count,used_on,created_at,updated_at FROM kb_entries WHERE code=$1`, code)
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
	for rows.Next() {
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
	return out
}
