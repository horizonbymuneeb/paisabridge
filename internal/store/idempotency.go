package store

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// ---------------------------------------------------------------------------
// Idempotency keys
// ---------------------------------------------------------------------------
//
// This file is the durable half of retry safety for client-supplied keys. It
// is deliberately separate from store.go so it can be reviewed on its own: it
// touches exactly one table (idempotency_keys, created by migration 0003) and
// no order logic.
//
// The contract every caller must follow:
//
//  1. Claim(ctx, scope, key, hash). Three outcomes:
//     - claimed: you own the key, go do the work, then call Complete;
//     - replay:  the key already finished; look at the returned record and
//       answer the request from it (same body hash) or refuse it
//       (different hash);
//     - ErrIdemInFlight: another request holds the key and has not finished.
//       The caller retries Claim or answers 409/503 - it must NOT do the work.
//  2. Complete(ctx, scope, key, orderID) records what the key produced.
//  3. Abort(ctx, scope, key) releases a claim whose work failed, so the client
//     can retry with the same key instead of being stuck forever.
//
// Records expire: PruneIdempotencyKeys deletes rows older than the retention
// window. A retried request older than that creates a new order, which is the
// accepted trade (the window is longer than any order TTL and any sane client
// retry budget).

// ErrIdemInFlight means another request claimed the key and has not produced
// its result yet. It is not an error in the caller's request; it is a signal
// to wait or to answer with a retryable status.
var ErrIdemInFlight = errors.New("store: idempotency key in flight")

// DefaultIdempotencyRetention is how long a completed key is remembered.
// One day is well beyond any plausible client retry, and well beyond the
// longest order TTL, so a replay always finds its record.
const DefaultIdempotencyRetention = 24 * time.Hour

// IdemState is the lifecycle of one key.
type IdemState string

const (
	// IdemPending: claimed, work not finished.
	IdemPending IdemState = "pending"
	// IdemDone: work finished, order_id holds the result.
	IdemDone IdemState = "done"
)

// IdemRecord is one row of idempotency_keys.
type IdemRecord struct {
	Scope       string
	Key         string
	RequestHash string
	OrderID     string
	State       IdemState
	CreatedAt   time.Time
}

// IdemOutcome tells the caller what happened in Claim.
type IdemOutcome int

const (
	// IdemClaimed: the caller now owns the key and must call Complete or Abort.
	IdemClaimed IdemOutcome = iota
	// IdemReplayed: the key already finished; the record carries the result.
	IdemReplayed
)

// ClaimIdempotencyKey atomically reserves (scope, key) for this request.
//
// hash is a digest of the normalised request body. The caller compares it with
// the stored hash to tell "the same request retried" (safe to replay the
// stored order) from "the same key reused for different content" (a client
// bug, answer 409).
//
// A completed record is returned as IdemReplayed. A live record belonging to
// another request returns ErrIdemInFlight. A record left pending by a crashed
// request older than staleAfter is re-claimed, so a process that dies mid
// request cannot block a key until the retention window ends.
func (s *Store) ClaimIdempotencyKey(ctx context.Context, scope, key, hash string, staleAfter time.Duration) (IdemOutcome, IdemRecord, error) {
	if err := s.checkDB(); err != nil {
		return 0, IdemRecord{}, err
	}
	if scope == "" {
		return 0, IdemRecord{}, errors.New("store: idempotency scope required")
	}
	if key == "" {
		return 0, IdemRecord{}, errors.New("store: idempotency key required")
	}
	if staleAfter <= 0 {
		staleAfter = 30 * time.Second
	}
	now := s.Now()

	// The claim is ONE statement, on purpose.
	//
	// An earlier version read the row, then did INSERT OR IGNORE, then read
	// again. That had a lost-update window: two requests could both pass the
	// first read, both insert-or-ignore against the same row, and both see
	// "pending with my hash" afterwards - and both created an order. Under
	// -race it reproduced 3 winners out of 8, reliably.
	//
	// UPSERT ... ON CONFLICT DO UPDATE is atomic and serialised by SQLite's
	// write lock. The DO UPDATE carries a WHEN guard that only fires when the
	// stored row is a stale pending claim; if it does not fire the row is
	// untouched and RowsAffected() is 0, which means "you did not get the
	// key". A DO NOTHING form cannot be used here because SQLite reports 0
	// rows for it whether or not the conflict happened, so the winner would
	// be indistinguishable from the loser.
	staleBefore := now.Add(-staleAfter).Format(TimeLayout)
	res, err := s.db.ExecContext(ctx, `INSERT INTO idempotency_keys
		(scope, key, request_hash, order_id, state, created_at)
		VALUES (?, ?, ?, '', ?, ?)
		ON CONFLICT(scope, key) DO UPDATE SET
			request_hash = excluded.request_hash,
			order_id     = '',
			created_at   = excluded.created_at
		WHERE idempotency_keys.state = ? AND idempotency_keys.created_at < ?`,
		scope, key, hash, string(IdemPending), now.Format(TimeLayout),
		string(IdemPending), staleBefore)
	if err != nil {
		return 0, IdemRecord{}, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, IdemRecord{}, err
	}

	// Read back for the record the caller needs in either case.
	rec, found, err := s.idemGet(ctx, scope, key)
	if err != nil {
		return 0, IdemRecord{}, err
	}
	if !found {
		return 0, IdemRecord{}, errors.New("store: idempotency row vanished after claim")
	}
	if n == 1 {
		// We won the insert, or took over a stale claim.
		return IdemClaimed, rec, nil
	}
	switch rec.State {
	case IdemDone:
		return IdemReplayed, rec, nil
	case IdemPending:
		// Someone else holds a live claim (or a fresh takeover). If their
		// hash differs this is a different request on the same key; the
		// caller decides between "wait" and "conflict", so report in flight
		// and let the record's hash be compared.
		return 0, rec, ErrIdemInFlight
	}
	return 0, rec, ErrIdemInFlight
}

// idemGet loads one row.
func (s *Store) idemGet(ctx context.Context, scope, key string) (IdemRecord, bool, error) {
	var (
		row       IdemRecord
		state     string
		createdAt string
	)
	err := s.db.QueryRowContext(ctx, `SELECT scope, key, request_hash, order_id, state, created_at
		FROM idempotency_keys WHERE scope = ? AND key = ?`, scope, key).
		Scan(&row.Scope, &row.Key, &row.RequestHash, &row.OrderID, &state, &createdAt)
	if errors.Is(err, sql.ErrNoRows) {
		return IdemRecord{}, false, nil
	}
	if err != nil {
		return IdemRecord{}, false, err
	}
	row.State = IdemState(state)
	row.CreatedAt = mustParseTime(createdAt)
	return row, true, nil
}

// CompleteIdempotencyKey stores the result of a claimed key and marks it done.
// It reports false when the claim is no longer ours (a stale pending row was
// taken over by another request); the caller should not treat that as success.
func (s *Store) CompleteIdempotencyKey(ctx context.Context, scope, key, hash, orderID string) (bool, error) {
	if err := s.checkDB(); err != nil {
		return false, err
	}
	res, err := s.db.ExecContext(ctx, `UPDATE idempotency_keys
		SET order_id = ?, state = ?
		WHERE scope = ? AND key = ? AND state = ? AND request_hash = ?`,
		orderID, string(IdemDone), scope, key, string(IdemPending), hash)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	return n == 1, nil
}

// AbortIdempotencyKey releases a claim whose work failed so a retry can claim
// the key again. Only the current owner's claim is deleted.
func (s *Store) AbortIdempotencyKey(ctx context.Context, scope, key, hash string) error {
	if err := s.checkDB(); err != nil {
		return err
	}
	_, err := s.db.ExecContext(ctx, `DELETE FROM idempotency_keys
		WHERE scope = ? AND key = ? AND state = ? AND request_hash = ?`,
		scope, key, string(IdemPending), hash)
	return err
}

// IdempotencyRecord looks up one record without claiming it (diagnostics and
// tests). The bool is false when no row exists.
func (s *Store) IdempotencyRecord(ctx context.Context, scope, key string) (IdemRecord, bool, error) {
	if err := s.checkDB(); err != nil {
		return IdemRecord{}, false, err
	}
	return s.idemGet(ctx, scope, key)
}

// PruneIdempotencyKeys deletes records created before cutoff and reports how
// many were removed. Called by the expiry sweeper next to PruneReplayGuard.
func (s *Store) PruneIdempotencyKeys(cutoff time.Time) (int64, error) {
	if err := s.checkDB(); err != nil {
		return 0, err
	}
	res, err := s.db.Exec(`DELETE FROM idempotency_keys WHERE created_at < ?`,
		cutoff.UTC().Format(TimeLayout))
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}
