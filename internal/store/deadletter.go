// Dead-letter storage for outbound deliveries.
//
// internal/outbound owns *when* a delivery fails hard enough to be parked; this
// file owns *where* it is parked. Migration 0004 creates the table; the three
// methods below are the whole surface the sender and the admin endpoints need.
//
// A dead-letter row is a money question, not a log line: if the row is lost the
// integrator's storefront never hears about a fulfilment. So the writes here are
// deliberately boring and durable, and every state change keeps the previous
// state readable so an operator can still see what happened after a retry.
package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// DeadLetter is one parked outbound delivery.
type DeadLetter struct {
	ID string `json:"id"`
	// DeliveryID is the X-PB-Delivery value the receiver saw (and will see
	// again on a retry, so it stays its idempotency key).
	DeliveryID string `json:"delivery_id"`
	Event      string `json:"event"`
	OrderID    string `json:"order_id,omitempty"`
	// LastError is the last transport/status error, kept verbatim so the
	// operator sees "status 502" rather than "delivery failed".
	LastError string `json:"last_error,omitempty"`
	// Attempts counts sends made against this row: the retry loop's attempts
	// plus every admin retry.
	Attempts int `json:"attempts"`
	// Body is the exact JSON that was signed and POSTed, verbatim, so a
	// retry re-signs the same bytes and the receiver's stored copy still
	// verifies. It is the last field in the JSON object because it is a whole
	// event body and would bury the scalar fields that matter for triage.
	Body []byte `json:"-"`

	CreatedAt time.Time `json:"created_at"`
	// RetriedAt is the last time an operator retried this row (zero = never).
	RetriedAt time.Time `json:"retried_at,omitzero"`
	// Resolved means the delivery eventually succeeded.
	Resolved   bool      `json:"resolved"`
	ResolvedAt time.Time `json:"resolved_at,omitzero"`
}

const deadLetterColumns = `id, delivery_id, event, order_id, last_error, attempts, body,
	created_at, retried_at, resolved_at`

// PutDeadLetter parks a failed delivery. It is idempotent on delivery_id: the
// same delivery failing twice updates the existing row (latest error, latest
// attempt count) instead of adding a second one, because the receiver treats
// delivery_id as the idempotency key and a duplicate row would let an operator
// "retry" the same delivery twice.
//
// An empty body is recorded as "{}" rather than rejected: an empty body is a
// sender-side bug, and losing the record of the failure because of it would be
// worse than storing a placeholder.
func (s *Store) PutDeadLetter(ctx context.Context, dl *DeadLetter) error {
	if err := s.checkDB(); err != nil {
		return err
	}
	if dl == nil {
		return errors.New("store: nil dead letter")
	}
	if strings.TrimSpace(dl.DeliveryID) == "" {
		return errors.New("store: dead letter needs a delivery id")
	}
	if strings.TrimSpace(dl.Event) == "" {
		return errors.New("store: dead letter needs an event")
	}
	now := s.Now().Format(TimeLayout)
	if dl.CreatedAt.IsZero() {
		dl.CreatedAt = s.Now()
	}
	body := string(dl.Body)
	if body == "" {
		body = "{}"
	}
	id := dl.ID
	if id == "" {
		var err error
		if id, err = randomID("dl_"); err != nil {
			return err
		}
	}

	// One statement, so a concurrent retry can never observe a row that
	// exists but has no body. On conflict the row keeps its original id and
	// created_at (it is the same delivery) and takes the fresh failure data.
	_, err := s.db.ExecContext(ctx, `INSERT INTO outbound_deadletter
		(id, delivery_id, event, order_id, last_error, attempts, body, created_at, retried_at, resolved_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, '', '')
		ON CONFLICT(delivery_id) DO UPDATE SET
			last_error = excluded.last_error,
			attempts   = MAX(attempts, excluded.attempts),
			event      = excluded.event,
			order_id   = CASE WHEN excluded.order_id <> '' THEN excluded.order_id ELSE order_id END`,
		id, dl.DeliveryID, dl.Event, dl.OrderID, dl.LastError, dl.Attempts, body, now)
	if err != nil {
		return fmt.Errorf("store: put dead letter: %w", err)
	}
	dl.ID = id
	return nil
}

// DeadLetters lists parked deliveries, newest first. status selects which rows:
//
//	"" / "open" -> open rows only (the operator's to-do list; the default)
//	"done"      -> resolved rows only (the delivery history)
//	"all"       -> both
//
// orderID, when non-empty, narrows to one order's deliveries, which is how an
// operator answers "did the storefront ever hear about this order?".
func (s *Store) DeadLetters(ctx context.Context, status, orderID string, limit int) ([]*DeadLetter, error) {
	if err := s.checkDB(); err != nil {
		return nil, err
	}
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	var where []string
	var args []any
	switch strings.ToLower(strings.TrimSpace(status)) {
	case "", "open", "pending":
		where = append(where, `resolved_at = ''`)
	case "done", "resolved":
		where = append(where, `resolved_at <> ''`)
	case "all":
	default:
		return nil, fmt.Errorf("store: unknown dead letter status %q (want open|done|all)", status)
	}
	if orderID != "" {
		where = append(where, `order_id = ?`)
		args = append(args, orderID)
	}
	q := `SELECT ` + deadLetterColumns + ` FROM outbound_deadletter`
	if len(where) > 0 {
		q += ` WHERE ` + strings.Join(where, " AND ")
	}
	q += ` ORDER BY resolved_at ASC, created_at DESC, id DESC LIMIT ?`
	args = append(args, limit)

	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("store: list dead letters: %w", err)
	}
	defer rows.Close()
	out := []*DeadLetter{}
	for rows.Next() {
		dl, err := scanDeadLetter(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, dl)
	}
	return out, rows.Err()
}

// DeadLetter loads one row by id. Unknown ids return ErrNotFound so the HTTP
// layer can answer 404 with the same code it uses for orders.
func (s *Store) DeadLetter(ctx context.Context, id string) (*DeadLetter, error) {
	if err := s.checkDB(); err != nil {
		return nil, err
	}
	if id == "" {
		return nil, ErrNotFound
	}
	row := s.db.QueryRowContext(ctx,
		`SELECT `+deadLetterColumns+` FROM outbound_deadletter WHERE id = ?`, id)
	return scanDeadLetter(row)
}

// DeadLetterCounts reports open and total rows in one round trip (the /metrics
// scraper should not pay two queries for two numbers).
func (s *Store) DeadLetterCounts(ctx context.Context) (open, total int64, err error) {
	if err := s.checkDB(); err != nil {
		return 0, 0, err
	}
	err = s.db.QueryRowContext(ctx, `SELECT
		COALESCE(SUM(CASE WHEN resolved_at = '' THEN 1 ELSE 0 END), 0),
		COUNT(*) FROM outbound_deadletter`).Scan(&open, &total)
	if err != nil {
		return 0, 0, fmt.Errorf("store: count dead letters: %w", err)
	}
	return open, total, nil
}

// FinishDeadLetter records the outcome of a retry attempt: delivered=true
// resolves the row, delivered=false stores the new error and leaves it open for
// the next attempt. Either way retried_at and attempts move forward, so an
// operator can tell "never retried" from "retried four times, still failing".
//
// doneAt is supplied by the caller (the API's clock) so tests can pin it.
func (s *Store) FinishDeadLetter(ctx context.Context, id string, delivered bool, lastErr string, doneAt time.Time) error {
	if err := s.checkDB(); err != nil {
		return err
	}
	if id == "" {
		return ErrNotFound
	}
	if doneAt.IsZero() {
		doneAt = s.Now()
	}
	now := doneAt.UTC().Format(TimeLayout)

	var (
		res sql.Result
		err error
	)
	if delivered {
		// Keep last_error: it is the history of why this row exists at all,
		// and a delivery that took four retries is worth knowing about.
		res, err = s.db.ExecContext(ctx, `UPDATE outbound_deadletter
			SET resolved_at = ?, retried_at = ?, attempts = attempts + 1
			WHERE id = ?`, now, now, id)
	} else {
		res, err = s.db.ExecContext(ctx, `UPDATE outbound_deadletter
			SET retried_at = ?, last_error = ?, attempts = attempts + 1
			WHERE id = ? AND resolved_at = ''`, now, lastErr, id)
	}
	if err != nil {
		return fmt.Errorf("store: finish dead letter: %w", err)
	}
	if n, err := res.RowsAffected(); err != nil {
		return err
	} else if n == 0 {
		return ErrNotFound
	}
	return nil
}

// CountOpenDeadLetters is the gauge behind paisabridge_outbound_deadletter_open:
// one number an operator can alert on ("anything > 0 for 15 minutes").
func (s *Store) CountOpenDeadLetters(ctx context.Context) (int64, error) {
	if err := s.checkDB(); err != nil {
		return 0, err
	}
	var n int64
	err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM outbound_deadletter WHERE resolved_at = ''`).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("store: count dead letters: %w", err)
	}
	return n, nil
}

func scanDeadLetter(r rowScanner) (*DeadLetter, error) {
	var (
		dl                     DeadLetter
		body                   string
		created, retried, done string
	)
	err := r.Scan(&dl.ID, &dl.DeliveryID, &dl.Event, &dl.OrderID, &dl.LastError,
		&dl.Attempts, &body, &created, &retried, &done)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("store: scan dead letter: %w", err)
	}
	dl.Body = []byte(body)
	dl.CreatedAt, _ = time.Parse(TimeLayout, created)
	dl.RetriedAt, _ = time.Parse(TimeLayout, retried)
	dl.ResolvedAt, _ = time.Parse(TimeLayout, done)
	dl.Resolved = done != ""
	return &dl, nil
}
