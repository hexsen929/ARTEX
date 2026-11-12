package db

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
)

// OperatorSeal uses a separate domain from stored task sessions. Neither API
// responses nor database plaintext contain listener/VPS credentials.
func (d *DB) OperatorSeal(master []byte, plain []byte) (string, error) {
	h := sha256.Sum256(append([]byte("artex/operator-modules/v1\x00"), master...))
	if len(master) == 0 {
		return "", fmt.Errorf("operator key unavailable")
	}
	return NewSessionStore(d, h[:]).encrypt(plain)
}
func (d *DB) OperatorOpen(master []byte, enc string) ([]byte, error) {
	h := sha256.Sum256(append([]byte("artex/operator-modules/v1\x00"), master...))
	if len(master) == 0 {
		return nil, fmt.Errorf("operator key unavailable")
	}
	return NewSessionStore(d, h[:]).decrypt(enc)
}
func (d *DB) OperatorSetting(ctx context.Context, key string, v any) error {
	var b []byte
	if err := d.QueryRowContext(ctx, "SELECT value FROM operator_module_settings WHERE key=$1", key).Scan(&b); err != nil {
		return err
	}
	return json.Unmarshal(b, v)
}
func (d *DB) SaveOperatorSetting(ctx context.Context, key string, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	_, err = d.ExecContext(ctx, `INSERT INTO operator_module_settings(key,value) VALUES($1,$2::jsonb) ON CONFLICT(key) DO UPDATE SET value=EXCLUDED.value,updated_at=now()`, key, b)
	return err
}
