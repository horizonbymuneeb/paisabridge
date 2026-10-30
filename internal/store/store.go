// Package store is the SQLite persistence layer for paisabridge.
//
// Everything that must survive a restart lives here: orders, the append-only
// order state machine journal, deduplicated inbound payment notices and the
// append-only wallet ledger. The package exposes *Store, whose method set is
// the single source of truth consumed by internal/match, internal/wallet,
// internal/api and cmd/paisabridge.
//
// Concurrency model: one *sql.DB with a tiny pool (MaxOpenConns 1 by default
// for modernc-style lock safety; see Open). Writers take a transaction with
// BEGIN IMMEDIATE semantics through the single connection, so the
// compare-and-set transitions below are serialised without relying on SQLite
// row-level locks (which it does not have).
//
// Idempotency: every state transition is a compare-and-set on the status
// column (`UPDATE ... WHERE id = ? AND status = ?`). Re-applying a transition
// that already happened returns ErrNoTransition and writes nothing, so a
// retried webhook or a double-clicked admin approval is a no-op rather than a
// second ledger entry.
package store

import (
	"context"
	"crypto/rand"
	"database/sql"
	"embed"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	_ "github.com/mattn/go-sqlite3" // registers the "sqlite3" driver

	"github.com/horizonbymuneeb/paisabridge/internal/types"
	"github.com/horizonbymuneeb/paisabridge/internal/unique"
)

//go:embed migrations/*.sql
var migrationsFS embed.FS

// Sentinel errors returned by the store. Callers map them to HTTP codes.
var (
	ErrNotFound     = errors.New("store: not found")
	ErrNoTransition = errors.New("store: transition not allowed from current state")
	ErrConflict     = errors.New("store: conflict")
	ErrDuplicate    = errors.New("store: duplicate")
	// ErrTransitionInvalid means the state machine forbids the edge, as
	// opposed to ErrNoTransition which means the row is not in fromStatus.
	ErrTransitionInvalid = errors.New("store: transition not in state machine")
	ErrClosed            = errors.New("store: closed")
)

// TimeLayout is how timestamps are persisted. Fixed-width UTC so string
// ordering and SQL MIN/MAX comparisons are chronological.

// Store is a handle to the paisabridge database.
type Store struct {
	db  *sql.DB
	now func() time.Time

	mu     sync.Mutex
	closed bool
}

// Option customises Open.
type Option func(*options)

type options struct {
	nowFn       func() time.Time
	maxOpen     int
	busyTimeout time.Duration
}

// WithClock injects a clock (tests use it to age orders past expiry).
func WithClock(fn func() time.Time) Option {
	return func(o *options) {
		if fn != nil {
			o.nowFn = fn
		}
	}
}

// WithMaxOpenConns overrides the connection pool size (default 1).
func WithMaxOpenConns(n int) Option {
	return func(o *options) {
		if n > 0 {
			o.maxOpen = n
		}
	}
}

// Open opens (and migrates) the SQLite database at path. Use ":memory:" or
// "file:name?mode=memory&cache=shared" for tests; the file form is the
// production path and gets the usual WAL + busy_timeout + foreign_keys
// pragmas.
func Open(path string, opts ...Option) (*Store, error) {
	o := options{
		nowFn:       func() time.Time { return time.Now().UTC() },
		maxOpen:     1,
		busyTimeout: 5 * time.Second,
	}
	for _, fn := range opts {
		fn(&o)
	}

	dsn := path
	if !strings.Contains(dsn, "?") {
		dsn += "?"
	} else {
		dsn += "&"
	}
	dsn += "_busy_timeout=" + fmt.Sprint(o.busyTimeout.Milliseconds())
	dsn += "&_foreign_keys=on"
	if !strings.Contains(path, ":memory:") {
		dsn += "&_journal_mode=WAL&_synchronous=NORMAL"
	}

	db, err := sql.Open("sqlite3", dsn)
	if err != nil {
		return nil, fmt.Errorf("store: open %s: %w", path, err)
	}
	db.SetMaxOpenConns(o.maxOpen)
	db.SetMaxIdleConns(o.maxOpen)
	db.SetConnMaxLifetime(0)

	s := &Store{db: db, now: o.nowFn}
	if err := s.migrate(context.Background()); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

// DB exposes the raw handle for packages that need a transaction of their own
// (the wallet). It is not part of the public API surface.
func (s *Store) DB() *sql.DB { return s.db }

// Now returns the store's clock in UTC. All timestamps written by the store
// come from here so tests can move time.
func (s *Store) Now() time.Time { return s.now().UTC() }

// Close closes the database handle.
func (s *Store) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil
	}
	s.closed = true
	return s.db.Close()
}

// Ping verifies the database answers (used by /api/healthz).
func (s *Store) Ping(ctx context.Context) error { return s.db.PingContext(ctx) }

// ---------------------------------------------------------------------------
// migrations
// ---------------------------------------------------------------------------

func (s *Store) migrate(ctx context.Context) error {
	if _, err := s.db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (
		version INTEGER PRIMARY KEY, applied_at TIMESTAMP NOT NULL)`); err != nil {
		return fmt.Errorf("store: bootstrap migrations table: %w", err)
	}

	names, err := migrationsFS.ReadDir("migrations")
	if err != nil {
		return fmt.Errorf("store: read migrations: %w", err)
	}
	sort.Slice(names, func(i, j int) bool { return names[i].Name() < names[j].Name() })

	for i, f := range names {
		version := int64(i + 1)
		var have int64
		if err := s.db.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM schema_migrations WHERE version = ?`, version).Scan(&have); err != nil {
			return fmt.Errorf("store: migration check %d: %w", version, err)
		}
		if have > 0 {
			continue
		}
		body, err := migrationsFS.ReadFile("migrations/" + f.Name())
		if err != nil {
			return fmt.Errorf("store: read %s: %w", f.Name(), err)
		}
		tx, err := s.db.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, string(body)); err != nil {
			tx.Rollback()
			return fmt.Errorf("store: apply %s: %w", f.Name(), err)
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO schema_migrations(version, applied_at) VALUES (?, ?)`,
			version, s.Now().Format(TimeLayout)); err != nil {
			tx.Rollback()
			return fmt.Errorf("store: record %s: %w", f.Name(), err)
		}
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("store: commit %s: %w", f.Name(), err)
		}
	}
	return nil
}

// ---------------------------------------------------------------------------
// orders
// ---------------------------------------------------------------------------

const orderColumns = `id, ref, product_id, product_name, catalog_price, amount,
	currency, method, status, customer_email, customer_phone, tx_ref,
	sms_otp, expires_at, created_at, updated_at`

// CreateOrderInput is the caller-facing order creation request. The store
// picks the unique amount and the order id/ref itself.
type CreateOrderInput struct {
	ProductID    string
	ProductName  string
	CatalogPrice int64
	Currency     string
	Method       types.Method
	// UserID scopes the wallet top-up / pay operations that follow this
	// order. types.Order has no user column, so the store does not persist
	// it; callers pass their authenticated identity to the wallet directly.
	UserID        string
	CustomerEmail string
	CustomerPhone string
	// TTL is how long the order stays matchable. Zero falls back to 30m.
	TTL time.Duration
	// AmountWindow is how far from CatalogPrice (in minor units) an adjusted
	// amount may land before we stop trying. Zero falls back to
	// unique.AdjustmentMax + one step (10 whole PKR / 0.1 USDT).
	AmountWindow int64
}

// CreateOrder inserts an order in status created and immediately records the
// created -> pending_payment transition, leaving the order matchable.
func (s *Store) CreateOrder(ctx context.Context, in CreateOrderInput) (*types.Order, error) {
	if in.ProductID == "" {
		return nil, fmt.Errorf("store: product id required")
	}
	if in.CatalogPrice <= 0 {
		return nil, fmt.Errorf("store: catalog price must be positive")
	}
	cur := in.Currency
	if cur == "" {
		cur = unique.CurrencyPKR
	}
	method := in.Method
	if method == "" {
		method = types.MethodEasypaisa
	}
	ttl := in.TTL
	if ttl <= 0 {
		ttl = 30 * time.Minute
	}

	now := s.Now()
	var lastErr error
	for attempt := 0; attempt < 64; attempt++ {
		// A unique-amount collision loses the race against a concurrent
		// insert (the partial unique index fires); retry with fresh state.
		ord, err := s.createOrderOnce(ctx, in, cur, method, ttl, now)
		if err == nil {
			return ord, nil
		}
		if !errors.Is(err, ErrConflict) && !errors.Is(err, ErrDuplicate) {
			return nil, err
		}
		lastErr = err
	}
	return nil, fmt.Errorf("store: create order: could not find a free unique amount: %w", lastErr)
}

func (s *Store) createOrderOnce(ctx context.Context, in CreateOrderInput, cur string, method types.Method, ttl time.Duration, now time.Time) (*types.Order, error) {
	taken, err := s.OpenAmounts(ctx, method, cur)
	if err != nil {
		return nil, err
	}
	amount, ok := pickWithin(in.CatalogPrice, cur, in.AmountWindow, taken)
	if !ok {
		return nil, fmt.Errorf("store: unique amount window exhausted for %s %s", method, cur)
	}
	id, err := randomID("ord_")
	if err != nil {
		return nil, err
	}
	ref, err := randomID("ref_")
	if err != nil {
		return nil, err
	}

	expires := now.Add(ttl)
	_, err = s.db.ExecContext(ctx, `INSERT INTO orders (`+orderColumns+`)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		id, ref, in.ProductID, in.ProductName, in.CatalogPrice, amount,
		cur, string(method), string(types.StatusCreated),
		in.CustomerEmail, in.CustomerPhone, "", "",
		expires.Format(TimeLayout), now.Format(TimeLayout), now.Format(TimeLayout))
	if err != nil {
		if isUniqueViolation(err) {
			return nil, fmt.Errorf("%w: unique amount %d on %s", ErrDuplicate, amount, method)
		}
		return nil, fmt.Errorf("store: insert order: %w", err)
	}

	if _, err := s.TransitionOrder(ctx, id, types.StatusCreated, types.StatusPendingPayment, "system", "order created"); err != nil {
		return nil, err
	}
	return s.Order(ctx, id)
}

// pickWithin calls unique.Pick and widens the search a little beyond the
// default window when the default is crowded, so a busy seller with many open
// orders still gets distinct amounts without leaving the "invisible
// adjustment" band by much.
func pickWithin(price int64, currency string, window int64, taken map[int64]bool) (int64, bool) {
	// unique.Pick is authoritative for the default band.
	if a, ok := unique.Pick(price, currency, taken); ok {
		return a, true
	}
	// Band exhausted: widen step-by-step in whole adjustment units (steps of
	// 1 PKR / 0.000001 USDT) up to the caller's window, so a busy seller can
	// still place an order instead of getting a hard failure.
	limit := window
	if limit <= 0 {
		limit = unique.AdjustmentMax[currency]
		if limit <= 0 {
			limit = 9
		}
	}
	for i := int64(1); i <= limit; i++ {
		for _, sign := range []int64{1, -1} {
			cand := price + sign*i
			if cand <= 0 || taken[cand] {
				continue
			}
			if currency == unique.CurrencyPKR && cand%100 != 0 {
				continue
			}
			return cand, true
		}
	}
	return 0, false
}

// OpenAmounts returns the amounts currently reserved by open orders for
// (method, currency) — the `taken` set fed to unique.Pick.
func (s *Store) OpenAmounts(ctx context.Context, method types.Method, currency string) (map[int64]bool, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT amount FROM orders
		WHERE method = ? AND currency = ? AND status IN (?,?,?)`,
		string(method), currency,
		string(types.StatusCreated), string(types.StatusPendingPayment), string(types.StatusVerified))
	if err != nil {
		return nil, fmt.Errorf("store: open amounts: %w", err)
	}
	defer rows.Close()
	taken := make(map[int64]bool)
	for rows.Next() {
		var a int64
		if err := rows.Scan(&a); err != nil {
			return nil, err
		}
		taken[a] = true
	}
	return taken, rows.Err()
}

// Order loads one order by id or ref.
func (s *Store) Order(ctx context.Context, id string) (*types.Order, error) {
	if id == "" {
		return nil, ErrNotFound
	}
	row := s.db.QueryRowContext(ctx, `SELECT `+orderColumns+` FROM orders WHERE id = ? OR ref = ?`, id, id)
	return scanOrder(row)
}

// OrderStatus returns just the current status (cheap poll used by the bot).
func (s *Store) OrderStatus(ctx context.Context, id string) (types.OrderStatus, error) {
	var st string
	err := s.db.QueryRowContext(ctx, `SELECT status FROM orders WHERE id = ? OR ref = ?`, id, id).Scan(&st)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrNotFound
	}
	if err != nil {
		return "", err
	}
	return types.OrderStatus(st), nil
}

// ListOrders returns the newest orders, optionally filtered by user.
func (s *Store) ListOrders(ctx context.Context, userID string, limit int) ([]*types.Order, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	q := `SELECT ` + orderColumns + ` FROM orders`
	args := []any{}
	if userID != "" {
		q += ` WHERE customer_email = ? OR customer_phone = ?`
		args = append(args, userID, userID)
	}
	q += ` ORDER BY created_at DESC, id DESC LIMIT ?`
	args = append(args, limit)

	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("store: list orders: %w", err)
	}
	defer rows.Close()
	out := []*types.Order{}
	for rows.Next() {
		o, err := scanOrder(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, o)
	}
	return out, rows.Err()
}

// OrderByTxRef finds the order already bound to an inbound transaction.
func (s *Store) OrderByTxRef(ctx context.Context, method types.Method, txnRef string) (*types.Order, error) {
	row := s.db.QueryRowContext(ctx, `SELECT `+orderColumns+` FROM orders
		WHERE method = ? AND tx_ref = ? AND tx_ref <> '' LIMIT 1`, string(method), txnRef)
	return scanOrder(row)
}

// FindOpenOrderByAmount is the matcher's primary lookup: at most one open
// order for (method, currency, amount), or ErrNotFound. The partial unique
// index makes the LIMIT 2 ambiguity impossible to hit; the matcher still
// guards for it defensively.
func (s *Store) FindOpenOrderByAmount(ctx context.Context, method types.Method, currency string, amount int64) (*types.Order, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+orderColumns+` FROM orders
		WHERE method = ? AND currency = ? AND amount = ? AND status IN (?,?)
		ORDER BY created_at ASC LIMIT 2`,
		string(method), currency, amount,
		string(types.StatusCreated), string(types.StatusPendingPayment))
	if err != nil {
		return nil, fmt.Errorf("store: find open order: %w", err)
	}
	defer rows.Close()
	var first *types.Order
	for rows.Next() {
		o, err := scanOrder(rows)
		if err != nil {
			return nil, err
		}
		if first == nil {
			first = o
			continue
		}
		return nil, fmt.Errorf("%w: amount %d has more than one open order", ErrConflict, amount)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if first == nil {
		return nil, ErrNotFound
	}
	return first, nil
}

// CountOpenOrdersByAmount reports how many open orders share an amount. Used
// by the matcher to be explicit about the ambiguous case in tests and logs.
func (s *Store) CountOpenOrdersByAmount(ctx context.Context, method types.Method, currency string, amount int64) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM orders
		WHERE method = ? AND currency = ? AND amount = ? AND status IN (?,?)`,
		string(method), currency, amount,
		string(types.StatusCreated), string(types.StatusPendingPayment)).Scan(&n)
	return n, err
}

// State machine. The graph is deliberately small and closed; anything not in
// this table is rejected before it reaches SQL.
//
//	created ──▶ pending_payment ──▶ verified ──▶ fulfilled
//	   │              │  │            │
//	   │              │  └──▶ review ─┘ (admin approve re-enters verified)
//	   │              ├──▶ expired
//	   └──────────────┴──▶ cancelled
//	verified ──▶ fulfilled | review | expired
//
// Terminal states (from types.IsTerminal): fulfilled, expired, cancelled.
var transitions = map[types.OrderStatus]map[types.OrderStatus]bool{
	types.StatusCreated: {
		types.StatusPendingPayment: true,
		types.StatusCancelled:      true,
		types.StatusExpired:        true,
	},
	types.StatusPendingPayment: {
		types.StatusVerified:  true, // matcher confirmed the inbound payment
		types.StatusReview:    true, // ambiguous amount / manual approve needed
		types.StatusCancelled: true,
		types.StatusExpired:   true,
	},
	types.StatusReview: {
		types.StatusVerified:  true, // admin approve
		types.StatusCancelled: true,
		types.StatusExpired:   true,
	},
	types.StatusVerified: {
		types.StatusFulfilled: true,
		types.StatusReview:    true, // delivery failed and needs a human
		// A verified-but-unfulfilled order can time out: the money was taken,
		// the goods were not delivered, so the sweep must be able to expire it
		// and the wallet refund follows.
		types.StatusExpired: true,
		// Cancellation is deliberately NOT an edge here: once a payment is
		// verified the only ways out are fulfilment, a review, or the expiry
		// sweep (which refunds). A paid order is not "cancelled" - it is
		// refunded, and that is a money movement with its own dedupe key.
	},
	// terminal states have no outgoing edges
}

// validStatus reports whether s is a known order status. types does not expose
// a validator, so the store owns the closed set.
func validStatus(s types.OrderStatus) bool {
	switch s {
	case types.StatusCreated, types.StatusPendingPayment, types.StatusVerified,
		types.StatusFulfilled, types.StatusReview, types.StatusExpired, types.StatusCancelled:
		return true
	}
	return false
}

// CanTransition reports whether the state machine allows from -> to.
func CanTransition(from, to types.OrderStatus) bool {
	return transitions[from][to]
}

// TransitionOrder is the only way an order changes state. It is a
// compare-and-set: the UPDATE only applies when the row is still in
// `fromStatus`, so concurrent/replayed transitions are no-ops returning
// ErrNoTransition and never write a second event.
func (s *Store) TransitionOrder(ctx context.Context, id string, fromStatus, toStatus types.OrderStatus, actor, detail string) (*types.Order, error) {
	if !validStatus(fromStatus) || !validStatus(toStatus) {
		return nil, fmt.Errorf("store: invalid transition %q -> %q", fromStatus, toStatus)
	}
	if fromStatus == toStatus {
		// Same-state "transition" is a replay: report it as such.
		return nil, fmt.Errorf("%w: already %s", ErrNoTransition, fromStatus)
	}
	if !CanTransition(fromStatus, toStatus) {
		return nil, fmt.Errorf("%w: %s -> %s is not allowed", ErrTransitionInvalid, fromStatus, toStatus)
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	now := s.Now().Format(TimeLayout)
	res, err := tx.ExecContext(ctx, `UPDATE orders SET status = ?, updated_at = ?, tx_ref = CASE WHEN ? <> '' THEN ? ELSE tx_ref END
		WHERE id = ? AND status = ?`,
		string(toStatus), now, "", "", id, string(fromStatus))
	if err != nil {
		return nil, fmt.Errorf("store: transition: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return nil, err
	}
	if n == 0 {
		// Either the order does not exist or it moved on. Distinguish them.
		var have string
		err := tx.QueryRowContext(ctx, `SELECT status FROM orders WHERE id = ?`, id).Scan(&have)
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		if err != nil {
			return nil, err
		}
		return nil, fmt.Errorf("%w: order %s is %s, want %s", ErrNoTransition, id, have, fromStatus)
	}

	if _, err := tx.ExecContext(ctx, `INSERT INTO order_events(order_id, from_state, to_state, actor, detail, created_at)
		VALUES (?,?,?,?,?,?)`, id, string(fromStatus), string(toStatus), actor, detail, now); err != nil {
		return nil, fmt.Errorf("store: journal: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return s.Order(ctx, id)
}

// BindTxRef stores the inbound transaction reference on an order. It is a
// one-way bind: a second, different reference for the same order is a
// conflict, because that would mean two payments claim one order.
func (s *Store) BindTxRef(ctx context.Context, id string, txnRef string) error {
	if txnRef == "" {
		return nil
	}
	res, err := s.db.ExecContext(ctx, `UPDATE orders SET tx_ref = ?, updated_at = ?
		WHERE id = ? AND (tx_ref = '' OR tx_ref = ?)`,
		txnRef, s.Now().Format(TimeLayout), id, txnRef)
	if err != nil {
		return fmt.Errorf("store: bind txn ref: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return fmt.Errorf("%w: order %s already bound to another txn ref", ErrConflict, id)
	}
	return nil
}

// SetSMSOTP attaches (or replaces) the OTP that arrived for a numeric order.
func (s *Store) SetSMSOTP(ctx context.Context, id, otp string) error {
	res, err := s.db.ExecContext(ctx, `UPDATE orders SET sms_otp = ?, updated_at = ? WHERE id = ?`,
		otp, s.Now().Format(TimeLayout), id)
	if err != nil {
		return fmt.Errorf("store: set otp: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// ExpireOpenOrders moves every created/pending_payment/verified order whose
// expiry has passed to expired and returns them. The caller (wallet) then
// refunds each one. Idempotent: the second run finds nothing. Runs inside a
// single transaction so concurrent workers cannot both claim the same expiry.
//
// `verified` is included on purpose. A wallet-paid order reaches verified at
// the moment the debit happens, before any delivery; if the payment window then
// elapsed without fulfilment the money must come back, and the only thing that
// can observe that is this sweep. Leaving verified out strands the customer's
// balance forever - they paid and received nothing.
func (s *Store) ExpireOpenOrders(ctx context.Context) ([]*types.Order, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	now := s.Now()
	const stale = 500
	rows, err := tx.QueryContext(ctx, `SELECT `+orderColumns+` FROM orders
		WHERE status IN (?,?,?) AND expires_at <= ?
		LIMIT ?`,
		string(types.StatusCreated), string(types.StatusPendingPayment),
		string(types.StatusVerified), now.Format(TimeLayout), stale)
	if err != nil {
		return nil, fmt.Errorf("store: scan expiring orders: %w", err)
	}
	var due []*types.Order
	for rows.Next() {
		o, err := scanOrder(rows)
		if err != nil {
			rows.Close()
			return nil, err
		}
		due = append(due, o)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	out := due[:0]
	for _, o := range due {
		res, err := tx.ExecContext(ctx, `UPDATE orders SET status = ?, updated_at = ?
			WHERE id = ? AND status IN (?,?,?)`,
			string(types.StatusExpired), now.Format(TimeLayout), o.ID,
			string(types.StatusCreated), string(types.StatusPendingPayment),
			string(types.StatusVerified))
		if err != nil {
			return nil, fmt.Errorf("store: expire order: %w", err)
		}
		if n, _ := res.RowsAffected(); n == 0 {
			continue
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO order_events(order_id, from_state, to_state, actor, detail, created_at)
			VALUES (?,?,?,?,?,?)`, o.ID, string(o.Status), string(types.StatusExpired), "expiry", "payment window elapsed", now.Format(TimeLayout)); err != nil {
			return nil, fmt.Errorf("store: journal expiry: %w", err)
		}
		o.Status = types.StatusExpired
		o.UpdatedAt = now
		out = append(out, o)
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return out, nil
}

// OrderEvents returns the journal for one order, oldest first.
type OrderEvent struct {
	OrderID   string            `json:"order_id"`
	FromState types.OrderStatus `json:"from_status"`
	ToState   types.OrderStatus `json:"to_status"`
	Actor     string            `json:"actor"`
	Detail    string            `json:"detail,omitempty"`
	CreatedAt time.Time         `json:"created_at"`
}

// Events returns an order's state journal.
func (s *Store) Events(ctx context.Context, orderID string) ([]OrderEvent, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT order_id, from_state, to_state, actor, detail, created_at
		FROM order_events WHERE order_id = ? ORDER BY id ASC`, orderID)
	if err != nil {
		return nil, fmt.Errorf("store: events: %w", err)
	}
	defer rows.Close()
	var out []OrderEvent
	for rows.Next() {
		var (
			e    OrderEvent
			ts   string
			from string
			to   string
		)
		if err := rows.Scan(&e.OrderID, &from, &to, &e.Actor, &e.Detail, &ts); err != nil {
			return nil, err
		}
		e.FromState, e.ToState = types.OrderStatus(from), types.OrderStatus(to)
		e.CreatedAt = mustParseTime(ts)
		out = append(out, e)
	}
	return out, rows.Err()
}

// ---------------------------------------------------------------------------
// payment notices
// ---------------------------------------------------------------------------

// RecordNotice persists an inbound payment exactly once. The second call for
// the same (method, txn_ref) returns ErrDuplicate and changes nothing.
func (s *Store) RecordNotice(ctx context.Context, n *types.PaymentNotice) error {
	if n == nil || n.TxnRef == "" {
		return fmt.Errorf("store: notice requires txn_ref")
	}
	now := s.Now()
	received := n.ReceivedAt
	if received.IsZero() {
		received = now
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO payment_notices
		(method, txn_ref, amount, currency, sender_name, sender_acct, received_at, raw, created_at)
		VALUES (?,?,?,?,?,?,?,?,?)`,
		string(n.Method), n.TxnRef, n.Amount, n.Currency, n.SenderName, n.SenderAcct,
		received.UTC().Format(TimeLayout), n.Raw, now.Format(TimeLayout))
	if err != nil {
		if isUniqueViolation(err) {
			return fmt.Errorf("%w: txn %s already processed", ErrDuplicate, n.TxnRef)
		}
		return fmt.Errorf("store: record notice: %w", err)
	}
	return nil
}

// NoticeRecorded reports whether a (method, txn_ref) was already seen.
func (s *Store) NoticeRecorded(ctx context.Context, method types.Method, txnRef string) (bool, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM payment_notices WHERE method = ? AND txn_ref = ?`,
		string(method), txnRef).Scan(&n)
	return n > 0, err
}

// ResolveNotice attaches the matcher's verdict to a recorded notice.
func (s *Store) ResolveNotice(ctx context.Context, method types.Method, txnRef, orderID, outcome, status string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE payment_notices SET order_id = ?, outcome = ?, status = ?
		WHERE method = ? AND txn_ref = ?`, orderID, outcome, status, string(method), txnRef)
	if err != nil {
		return fmt.Errorf("store: resolve notice: %w", err)
	}
	return nil
}

// Notice is a stored payment notice (admin/review tooling reads these).
type Notice struct {
	types.PaymentNotice
	OrderID   string    `json:"order_id,omitempty"`
	Outcome   string    `json:"outcome,omitempty"`
	Status    string    `json:"status"`
	CreatedAt time.Time `json:"created_at"`
}

// ListNotices returns the newest inbound payments.
func (s *Store) ListNotices(ctx context.Context, status string, limit int) ([]*Notice, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	q := `SELECT method, txn_ref, amount, currency, sender_name, sender_acct, received_at, raw,
		order_id, outcome, status, created_at FROM payment_notices`
	args := []any{}
	if status != "" {
		q += ` WHERE status = ?`
		args = append(args, status)
	}
	q += ` ORDER BY created_at DESC, txn_ref DESC LIMIT ?`
	args = append(args, limit)

	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("store: list notices: %w", err)
	}
	defer rows.Close()
	out := []*Notice{}
	for rows.Next() {
		var (
			n         Notice
			method    string
			received  string
			createdAt string
		)
		if err := rows.Scan(&method, &n.TxnRef, &n.Amount, &n.Currency, &n.SenderName, &n.SenderAcct,
			&received, &n.Raw, &n.OrderID, &n.Outcome, &n.Status, &createdAt); err != nil {
			return nil, err
		}
		n.Method = types.Method(method)
		n.ReceivedAt = mustParseTime(received)
		n.CreatedAt = mustParseTime(createdAt)
		out = append(out, &n)
	}
	return out, rows.Err()
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

type rowScanner interface {
	Scan(dest ...any) error
}

func scanOrder(r rowScanner) (*types.Order, error) {
	var (
		o                types.Order
		status, method   string
		expires, created string
		updated          string
	)
	err := r.Scan(&o.ID, &o.Ref, &o.ProductID, &o.ProductName, &o.CatalogPrice, &o.Amount,
		&o.Currency, &method, &status, &o.CustomerEmail, &o.CustomerPhone,
		&o.TxRef, &o.SMSOTP, &expires, &created, &updated)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("store: scan order: %w", err)
	}
	o.Method = types.Method(method)
	o.Status = types.OrderStatus(status)
	o.ExpiresAt = mustParseTime(expires)
	o.CreatedAt = mustParseTime(created)
	o.UpdatedAt = mustParseTime(updated)
	return &o, nil
}

func isUniqueViolation(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "UNIQUE constraint failed") ||
		strings.Contains(msg, "constraint failed: UNIQUE") ||
		strings.Contains(msg, "PRIMARY KEY")
}

func randomID(prefix string) (string, error) {
	b := make([]byte, 9)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("store: id: %w", err)
	}
	return prefix + hex.EncodeToString(b), nil
}
