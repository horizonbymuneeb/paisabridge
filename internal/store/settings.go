package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// Integration keys. They are constants so a typo cannot silently write a
// setting nobody reads.
const (
	IntegrationTelegramToken  = "telegram_token"
	IntegrationTelegramChatID = "telegram_chat_id"
	IntegrationLastTestAt     = "telegram_last_test_at"
)

// Integration returns one stored setting. The bool is false when the key was
// never written, which the caller should treat as "not configured" rather than
// as an empty string it can act on.
func (s *Store) Integration(ctx context.Context, key string) (string, bool, error) {
	if err := s.checkDB(); err != nil {
		return "", false, err
	}
	var v string
	err := s.db.QueryRowContext(ctx, `SELECT value FROM integrations WHERE key = ?`, key).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("store: read integration %q: %w", key, err)
	}
	return v, true, nil
}

// SetIntegration writes one setting, upserting. An empty value is stored (the
// operator clearing a token is a real action) and callers decide what empty
// means for them.
func (s *Store) SetIntegration(ctx context.Context, key, value string) error {
	if err := s.checkDB(); err != nil {
		return err
	}
	if key == "" {
		return errors.New("store: integration key required")
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO integrations (key, value, updated_at)
		VALUES (?, ?, ?)
		ON CONFLICT(key) DO UPDATE SET value = excluded.value, updated_at = excluded.updated_at`,
		key, value, s.Now().UTC().Format(TimeLayout))
	if err != nil {
		return fmt.Errorf("store: write integration %q: %w", key, err)
	}
	return nil
}

// DeleteIntegration removes a key, so a later read reports "not configured"
// instead of an empty value.
func (s *Store) DeleteIntegration(ctx context.Context, key string) error {
	if err := s.checkDB(); err != nil {
		return err
	}
	if _, err := s.db.ExecContext(ctx, `DELETE FROM integrations WHERE key = ?`, key); err != nil {
		return fmt.Errorf("store: delete integration %q: %w", key, err)
	}
	return nil
}

// IntegrationUpdatedAt reports when a key was last written (zero when absent).
func (s *Store) IntegrationUpdatedAt(ctx context.Context, key string) (time.Time, bool, error) {
	if err := s.checkDB(); err != nil {
		return time.Time{}, false, err
	}
	var ts string
	err := s.db.QueryRowContext(ctx, `SELECT updated_at FROM integrations WHERE key = ?`, key).Scan(&ts)
	if errors.Is(err, sql.ErrNoRows) {
		return time.Time{}, false, nil
	}
	if err != nil {
		return time.Time{}, false, err
	}
	return mustParseTime(ts), true, nil
}
