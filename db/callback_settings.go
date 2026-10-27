package db

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/Autumn-27/artex/config"
)

type callbackProbeStore struct {
	ctx context.Context
	pg  *sql.DB
}

func (s callbackProbeStore) GetSetting(key string) (string, bool, error) {
	// A fresh installation may not have run its schema migration yet. Checking
	// metadata here keeps doctor read-only and preserves the file/env defaults.
	var exists bool
	if err := s.pg.QueryRowContext(s.ctx, `SELECT to_regclass('public.settings') IS NOT NULL`).Scan(&exists); err != nil {
		return "", false, err
	}
	if !exists {
		return "", false, nil
	}
	var value string
	err := s.pg.QueryRowContext(s.ctx, `SELECT value FROM settings WHERE key=$1`, key).Scan(&value)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	return value, err == nil, err
}

// ProbeCallbackAddr uses the runtime resolver without opening migrations,
// seeding settings, writing data, or starting any network listener.
func ProbeCallbackAddr(dsn string) (config.CallbackAddress, error) {
	pg, err := sql.Open("pgx", dsn)
	if err != nil {
		return config.CallbackAddress{}, err
	}
	defer pg.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return config.ResolveCallbackAddr(callbackProbeStore{ctx: ctx, pg: pg})
}
