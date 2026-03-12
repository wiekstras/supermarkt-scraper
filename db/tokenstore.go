package db

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

// DBTokenStore implementeert ahclient.TokenStore op PostgreSQL.
// Tokens worden bewaard in de `api_tokens` tabel zodat ze Railway restarts overleven.
type DBTokenStore struct{}

// NewTokenStore maakt een nieuwe DBTokenStore aan.
// Vereist dat db.Init() al is aangeroepen.
func NewTokenStore() *DBTokenStore {
	return &DBTokenStore{}
}

// LoadToken haalt een token op uit de DB. Geeft een lege string terug als het
// token niet bestaat of verlopen is.
func (s *DBTokenStore) LoadToken(_ context.Context, key string) (string, time.Time, error) {
	row := pool.QueryRow(`
		SELECT token_value, expires_at
		FROM api_tokens
		WHERE token_key = $1
	`, key)

	var value string
	var expiresAt time.Time
	if err := row.Scan(&value, &expiresAt); err != nil {
		if err == sql.ErrNoRows {
			return "", time.Time{}, nil
		}
		return "", time.Time{}, fmt.Errorf("LoadToken(%q): %w", key, err)
	}
	return value, expiresAt, nil
}

// SaveToken slaat een token op in de DB (upsert).
func (s *DBTokenStore) SaveToken(_ context.Context, key, value string, expiresAt time.Time) error {
	_, err := pool.Exec(`
		INSERT INTO api_tokens (token_key, token_value, expires_at, opgeslagen_op)
		VALUES ($1, $2, $3, NOW())
		ON CONFLICT (token_key) DO UPDATE
			SET token_value   = EXCLUDED.token_value,
			    expires_at    = EXCLUDED.expires_at,
			    opgeslagen_op = NOW()
	`, key, value, expiresAt)
	if err != nil {
		return fmt.Errorf("SaveToken(%q): %w", key, err)
	}
	return nil
}
