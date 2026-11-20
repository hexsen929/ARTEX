package db

import (
	"context"
	"fmt"
)

func (s *SessionStore) UpdateConnection(ctx context.Context, rec *SessionRecord) error {
	enc, err := s.encrypt(rec.Secret)
	if err != nil {
		return err
	}
	res, err := s.d.ExecContext(ctx, `UPDATE sessions SET kind=$2,url=$3,secret=$4,lang=$5,status=$6 WHERE id=$1`, rec.ID, rec.Kind, rec.URL, enc, rec.Lang, rec.Status)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return fmt.Errorf("session not found")
	}
	return nil
}
func (d *DB) WebshellMetadata(ctx context.Context, id int64) (map[string]any, error) {
	var name, remark, method, param, encoding, target string
	err := d.QueryRowContext(ctx, `SELECT name,remark,method,cmd_param,encoding,target_os FROM sessions WHERE id=$1`, id).Scan(&name, &remark, &method, &param, &encoding, &target)
	return map[string]any{"name": name, "remark": remark, "method": method, "cmd_param": param, "encoding": encoding, "target_os": target}, err
}
func (d *DB) SaveWebshellMetadata(ctx context.Context, id int64, v map[string]any) error {
	_, err := d.ExecContext(ctx, `UPDATE sessions SET name=$2,remark=$3,method=$4,cmd_param=$5,encoding=$6,target_os=$7 WHERE id=$1`, id, v["name"], v["remark"], v["method"], v["cmd_param"], v["encoding"], v["target_os"])
	return err
}
func (d *DB) RecordWebshellCommand(ctx context.Context, id int64, command, stdout, stderr string, ms int64, ok bool) error {
	_, err := d.ExecContext(ctx, `INSERT INTO webshell_commands(session_id,command,stdout,stderr,ms,ok) VALUES($1,$2,$3,$4,$5,$6,$7)`, id, command, stdout, stderr, ms, ok)
	return err
}
