package store

import (
	"database/sql"
	"errors"
	"time"
)

// Seen reports whether the envelope id has already been accepted.
func (s *Store) Seen(id string) bool {
	var one int
	err := s.db.QueryRow(`SELECT 1 FROM webhook_replay WHERE envelope_id = ?`, id).Scan(&one)
	return err == nil
}

// Remember records the envelope id. It returns false when the id was already
// present, which is the atomic "claim" the replay guard needs: two concurrent
// requests carrying the same id cannot both get true.
func (s *Store) Remember(id string) bool {
	return s.RememberFor(id, "", time.Now().UTC())
}

// RememberFor is Remember with the rail and timestamp recorded.
func (s *Store) RememberFor(id, method string, at time.Time) bool {
	if id == "" {
		return false
	}
	res, err := s.db.Exec(
		`INSERT OR IGNORE INTO webhook_replay (envelope_id, method, first_seen) VALUES (?, ?, ?)`,
		id, method, at.Format(time.RFC3339Nano))
	if err != nil {
		// A storage failure must fail closed: the caller treats false as
		// "already seen" and refuses the request.
		return false
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false
	}
	return n == 1
}

// PruneReplayGuard deletes ids first seen before cutoff and reports how many
// rows were removed. Called by the sweeper.
func (s *Store) PruneReplayGuard(cutoff time.Time) (int64, error) {
	res, err := s.db.Exec(`DELETE FROM webhook_replay WHERE first_seen < ?`,
		cutoff.Format(time.RFC3339Nano))
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// errNoDB guards against a nil store in tests.
var errNoDB = errors.New("store: nil database")

func (s *Store) checkDB() error {
	if s == nil || s.db == nil {
		return errNoDB
	}
	return nil
}

var _ = sql.ErrNoRows
