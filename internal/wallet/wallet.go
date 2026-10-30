// Package wallet is the append-only money ledger.
//
// Invariants (each one is enforced in SQL or a single transaction, never by
// hoping the caller behaves):
//
//	I1  The balance is SUM(amount_minor) over wallet_ledger for a user.
//	    There is no balance column to drift.
//	I2  Every ledger row carries a dedupe_key with a UNIQUE index. Two
//	    "credit order X" attempts cannot both land; the second gets
//	    ErrDuplicate and changes nothing.
//	I3  Credits also require (user_id, idempotency_key) uniqueness, so a
//	    retried HTTP call with the same key is a pure no-op.
//	I4  A debit (paying with wallet) only happens inside a BEGIN IMMEDIATE
//	    transaction that re-checks the balance before inserting. The store
//	    uses a single connection, so no interleaving is possible.
//	I5  The prepay discount is a basis-points value on the *order* amount;
//	    it is recorded as its own ledger row (kind=discount) so the customer
//	    sees the saving and the seller sees exactly how much they gave away.
//	I6  Refunds are keyed refund:<order>:<why>; the expiry sweeper can run
//	    any number of times and will only ever refund an order once.
//
// Money moves between these kinds only:
//
//	topup        +amount   external money in (easypaisa, jazzcash, ...)
//	order_pay    -amount   wallet pays an order (net of discount)
//	discount     0         informational row recording the 5% saving
//	refund       +amount   order expired/cancelled -> money back
//	adjust       ±amount   admin correction (always memo'd)
package wallet

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/horizonbymuneeb/paisabridge/internal/store"
	"github.com/horizonbymuneeb/paisabridge/internal/types"
	"github.com/horizonbymuneeb/paisabridge/internal/unique"
)

// Ledger kinds.
const (
	KindTopup    = "topup"
	KindOrderPay = "order_pay"
	KindDiscount = "discount"
	KindRefund   = "refund"
	KindAdjust   = "adjust"
)

// DefaultDiscountBPS is the prepay discount: 500 basis points = 5%.
const DefaultDiscountBPS int64 = 500

var (
	ErrInsufficientFunds = errors.New("wallet: insufficient funds")
	ErrDuplicate         = store.ErrDuplicate
	ErrNotFound          = store.ErrNotFound
)

// Ledger is one immutable money movement.
type Ledger struct {
	ID             int64     `json:"id"`
	UserID         string    `json:"user_id"`
	Kind           string    `json:"kind"`
	AmountMinor    int64     `json:"amount_minor"`
	Currency       string    `json:"currency"`
	OrderID        string    `json:"order_id,omitempty"`
	IdempotencyKey string    `json:"-"`
	Memo           string    `json:"memo,omitempty"`
	CreatedAt      time.Time `json:"created_at"`
}

// Balance is a wallet snapshot.
type Balance struct {
	UserID      string `json:"user_id"`
	Currency    string `json:"currency"`
	Available   int64  `json:"available_minor"`
	DiscountBPS int64  `json:"discount_bps"`
}

// Wallet is the ledger service. It owns its own SQL over the shared *store.Store
// handle, because the interesting operations are multi-statement transactions
// that the generic store API would only obscure.
type Wallet struct {
	db       *sql.DB
	now      func() time.Time
	currency string
	discount int64
}

// Option customises the wallet.
type Option func(*Wallet)

// WithDiscountBPS overrides the prepay discount (default 5%).
func WithDiscountBPS(bps int64) Option {
	return func(w *Wallet) {
		if bps >= 0 && bps <= 10000 {
			w.discount = bps
		}
	}
}

// WithCurrency sets the settlement currency (default PKR).
func WithCurrency(cur string) Option {
	return func(w *Wallet) {
		if cur != "" {
			w.currency = cur
		}
	}
}

// New wires a wallet onto a store.
func New(s *store.Store, opts ...Option) *Wallet {
	w := &Wallet{
		db:       s.DB(),
		now:      s.Now,
		currency: unique.CurrencyPKR,
		discount: DefaultDiscountBPS,
	}
	for _, fn := range opts {
		fn(w)
	}
	return w
}

// DiscountBPS exposes the configured prepay discount.
func (w *Wallet) DiscountBPS() int64 { return w.discount }

// Discount returns the money saved by paying `amount` from the wallet.
func (w *Wallet) Discount(amount int64) int64 { return unique.PercentOff(amount, w.discount) }

// ---------------------------------------------------------------------------
// reads
// ---------------------------------------------------------------------------

// Balance reads the ledger sum. userID "" is the shared/prepaid pool.
func (w *Wallet) Balance(ctx context.Context, userID string) (*Balance, error) {
	var sum int64
	err := w.db.QueryRowContext(ctx, `SELECT COALESCE(SUM(amount_minor), 0) FROM wallet_ledger
		WHERE user_id = ? AND currency = ?`, normUser(userID), w.currency).Scan(&sum)
	if err != nil {
		return nil, fmt.Errorf("wallet: balance: %w", err)
	}
	return &Balance{UserID: userID, Currency: w.currency, Available: sum, DiscountBPS: w.discount}, nil
}

// AvailableMinor is Balance().Available without the wrapper.
func (w *Wallet) AvailableMinor(ctx context.Context, userID string) (int64, error) {
	var sum int64
	err := w.db.QueryRowContext(ctx, `SELECT COALESCE(SUM(amount_minor), 0) FROM wallet_ledger
		WHERE user_id = ? AND currency = ?`, normUser(userID), w.currency).Scan(&sum)
	if err != nil {
		return 0, fmt.Errorf("wallet: available: %w", err)
	}
	return sum, nil
}

// History returns the ledger, newest first.
func (w *Wallet) History(ctx context.Context, userID string, limit int) ([]Ledger, error) {
	if limit <= 0 || limit > 500 {
		limit = 50
	}
	rows, err := w.db.QueryContext(ctx, `SELECT id, user_id, kind, amount_minor, currency, order_id, memo, created_at
		FROM wallet_ledger WHERE user_id = ? AND currency = ?
		ORDER BY id DESC LIMIT ?`, normUser(userID), w.currency, limit)
	if err != nil {
		return nil, fmt.Errorf("wallet: history: %w", err)
	}
	defer rows.Close()
	out := make([]Ledger, 0, limit)
	for rows.Next() {
		var (
			l  Ledger
			ts string
		)
		if err := rows.Scan(&l.ID, &l.UserID, &l.Kind, &l.AmountMinor, &l.Currency, &l.OrderID, &l.Memo, &ts); err != nil {
			return nil, err
		}
		l.CreatedAt, _ = time.Parse(store.TimeLayout, ts)
		out = append(out, l)
	}
	return out, rows.Err()
}

// ---------------------------------------------------------------------------
// writes
// ---------------------------------------------------------------------------

// Topup credits external money. Idempotent on idemKey: replaying the same key
// (a retried HTTP request, a re-delivered payment bell) returns the balance
// unchanged and no new row.
func (w *Wallet) Topup(ctx context.Context, userID string, amount int64, idemKey, memo string) (*Balance, error) {
	if amount <= 0 {
		return nil, errors.New("wallet: topup amount must be positive")
	}
	if strings.TrimSpace(idemKey) == "" {
		return nil, errors.New("wallet: topup requires an idempotency key")
	}
	uid := normUser(userID)
	_, err := w.insert(ctx, Ledger{
		UserID:         uid,
		Kind:           KindTopup,
		AmountMinor:    amount,
		Currency:       w.currency,
		IdempotencyKey: idemKey,
		Memo:           memo,
	}, "topup:"+uid+":"+idemKey)
	if err != nil {
		if errors.Is(err, store.ErrDuplicate) {
			// Replay: not an error, the money is already there exactly once.
			return w.Balance(ctx, userID)
		}
		return nil, err
	}
	return w.Balance(ctx, userID)
}

// PayResult explains a wallet purchase.
type PayResult struct {
	Balance       *Balance     `json:"balance"`
	Order         *types.Order `json:"order,omitempty"`
	PriceMinor    int64        `json:"price_minor"`
	DiscountMinor int64        `json:"discount_minor"`
	ChargedMinor  int64        `json:"charged_minor"`
	AlreadyPaid   bool         `json:"already_paid"`
}

// PayOrder charges an order to the wallet at a prepay discount.
//
// The discount is computed on the order's catalog price and the *discounted*
// amount is what leaves the wallet. If the wallet is short, ErrInsufficientFunds
// is returned and nothing is written. If the order is already paid (verified or
// fulfilled), AlreadyPaid=true and nothing is charged again.
//
// Everything happens in one transaction: balance re-check -> debit row ->
// discount row -> order transition to verified. So "money left the wallet but
// the order is still pending" cannot happen.
func (w *Wallet) PayOrder(ctx context.Context, userID, orderID string) (*PayResult, error) {
	if orderID == "" {
		return nil, errors.New("wallet: pay requires an order id")
	}
	uid := normUser(userID)

	tx, err := w.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	var (
		status   string
		price    int64
		currency string
	)
	err = tx.QueryRowContext(ctx,
		`SELECT status, catalog_price, currency FROM orders WHERE id = ? OR ref = ?`, orderID, orderID).
		Scan(&status, &price, &currency)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("wallet: load order: %w", err)
	}
	if currency != w.currency {
		return nil, fmt.Errorf("wallet: order currency %s != wallet currency %s", currency, w.currency)
	}

	st := types.OrderStatus(status)
	switch st {
	case types.StatusVerified, types.StatusFulfilled:
		// Replay of an already-paid order: report the balance, charge nothing.
		// Read it inside the tx — the pool is one connection wide, so calling
		// w.Balance here would deadlock against the open transaction.
		avail, err := txSum(tx, ctx, uid, w.currency)
		if err != nil {
			return nil, err
		}
		return &PayResult{Balance: &Balance{UserID: userID, Currency: w.currency, Available: avail, DiscountBPS: w.discount},
			AlreadyPaid: true, PriceMinor: price}, nil
	case types.StatusExpired, types.StatusCancelled:
		return nil, fmt.Errorf("wallet: order %s is %s and cannot be paid", orderID, st)
	}

	discount := unique.PercentOff(price, w.discount)
	charged := price - discount
	if charged < 0 {
		charged = 0
	}

	var avail int64
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE(SUM(amount_minor),0) FROM wallet_ledger
		WHERE user_id = ? AND currency = ?`, uid, w.currency).Scan(&avail); err != nil {
		return nil, fmt.Errorf("wallet: balance in tx: %w", err)
	}
	if avail < charged {
		return nil, fmt.Errorf("%w: need %d, have %d", ErrInsufficientFunds, charged, avail)
	}

	now := w.now().UTC().Format(store.TimeLayout)
	if _, err := tx.ExecContext(ctx, `INSERT INTO wallet_ledger
		(user_id, kind, amount_minor, currency, order_id, idempotency_key, dedupe_key, memo, created_at)
		VALUES (?,?,?,?,?,?,?,?,?)`,
		uid, KindOrderPay, -charged, w.currency, orderID, "pay:"+orderID, "pay:"+orderID,
		fmt.Sprintf("wallet pay for order %s", orderID), now); err != nil {
		if isUnique(err) {
			// Someone already paid this order from this wallet: no-op.
			avail, berr := txSum(tx, ctx, uid, w.currency)
			if berr != nil {
				return nil, berr
			}
			return &PayResult{Balance: &Balance{UserID: userID, Currency: w.currency, Available: avail, DiscountBPS: w.discount},
				AlreadyPaid: true, PriceMinor: price}, nil
		}
		return nil, fmt.Errorf("wallet: pay insert: %w", err)
	}
	if discount > 0 {
		if _, err := tx.ExecContext(ctx, `INSERT INTO wallet_ledger
			(user_id, kind, amount_minor, currency, order_id, idempotency_key, dedupe_key, memo, created_at)
			VALUES (?,?,?,?,?,?,?,?,?)`,
			uid, KindDiscount, 0, w.currency, orderID, "discount:"+orderID, "discount:"+orderID,
			fmt.Sprintf("%d bps prepay discount on %d", w.discount, price), now); err != nil {
			if !isUnique(err) {
				return nil, fmt.Errorf("wallet: discount insert: %w", err)
			}
		}
	}

	res, err := tx.ExecContext(ctx, `UPDATE orders SET status = ?, updated_at = ?
		WHERE (id = ? OR ref = ?) AND status IN (?,?,?)`,
		string(types.StatusVerified), now, orderID, orderID,
		string(types.StatusCreated), string(types.StatusPendingPayment), string(types.StatusReview))
	if err != nil {
		return nil, fmt.Errorf("wallet: pay transition: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return nil, store.ErrNoTransition
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO order_events(order_id, from_state, to_state, actor, detail, created_at)
		SELECT id, ?, ?, 'wallet', ?, ? FROM orders WHERE id = ? OR ref = ?`,
		status, string(types.StatusVerified), fmt.Sprintf("wallet pay %d (discount %d)", charged, discount), now, orderID, orderID); err != nil {
		return nil, fmt.Errorf("wallet: pay journal: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}

	bal, err := w.Balance(ctx, userID)
	if err != nil {
		return nil, err
	}
	return &PayResult{
		Balance:       bal,
		PriceMinor:    price,
		DiscountMinor: discount,
		ChargedMinor:  charged,
	}, nil
}

// Refund returns a paid order's money to the wallet. It is idempotent per
// order (`refund:<order>` dedupe key), so the expiry sweeper, an admin cancel
// and a double-clicked button all collapse to one refund.
//
// Only orders that actually debited this wallet are refunded: the function
// looks up the original order_pay row and refunds exactly that amount.
func (w *Wallet) Refund(ctx context.Context, userID, orderID, reason string) (*Ledger, error) {
	if orderID == "" {
		return nil, errors.New("wallet: refund requires an order id")
	}
	uid := normUser(userID)

	var (
		paidBy string
		paid   int64
	)
	err := w.db.QueryRowContext(ctx, `SELECT user_id, -amount_minor FROM wallet_ledger
		WHERE kind = ? AND order_id = ? ORDER BY id ASC LIMIT 1`,
		KindOrderPay, orderID).Scan(&paidBy, &paid)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("wallet: find payment: %w", err)
	}
	if uid == "" {
		uid = paidBy
	}
	memo := reason
	if memo == "" {
		memo = "order closed"
	}
	l, err := w.insert(ctx, Ledger{
		UserID:         paidBy,
		Kind:           KindRefund,
		AmountMinor:    paid,
		Currency:       w.currency,
		OrderID:        orderID,
		IdempotencyKey: "refund:" + orderID,
		Memo:           "refund: " + memo,
	}, "refund:"+orderID)
	if err != nil {
		if errors.Is(err, store.ErrDuplicate) {
			return nil, nil // already refunded
		}
		return nil, err
	}
	return l, nil
}

// Adjust records an admin correction. Positive credits, negative debits.
func (w *Wallet) Adjust(ctx context.Context, userID string, delta int64, idemKey, memo string) (*Balance, error) {
	if delta == 0 {
		return nil, errors.New("wallet: adjust of zero")
	}
	if strings.TrimSpace(idemKey) == "" {
		return nil, errors.New("wallet: adjust requires an idempotency key")
	}
	uid := normUser(userID)
	if delta < 0 {
		avail, err := w.AvailableMinor(ctx, uid)
		if err != nil {
			return nil, err
		}
		if avail+delta < 0 {
			return nil, ErrInsufficientFunds
		}
	}
	if _, err := w.insert(ctx, Ledger{
		UserID:         uid,
		Kind:           KindAdjust,
		AmountMinor:    delta,
		Currency:       w.currency,
		IdempotencyKey: idemKey,
		Memo:           memo,
	}, "adjust:"+uid+":"+idemKey); err != nil {
		return nil, err
	}
	return w.Balance(ctx, userID)
}

// RefundExpired is the sweeper: expire every past-due order through the store
// state machine, then refund any that had been paid from the wallet. Returns
// the orders it expired. Safe to run on a ticker from many goroutines: the
// store's expiry is compare-and-set and the refund key is unique.
func (w *Wallet) RefundExpired(ctx context.Context, s *store.Store) ([]*types.Order, error) {
	expired, err := s.ExpireOpenOrders(ctx)
	if err != nil {
		return nil, err
	}
	for _, o := range expired {
		if _, err := w.Refund(ctx, "", o.ID, "order expired"); err != nil && !errors.Is(err, ErrNotFound) {
			return expired, fmt.Errorf("wallet: refund expired %s: %w", o.ID, err)
		}
	}
	return expired, nil
}

// Cancel and refund one order, in that order.
func (w *Wallet) CancelOrder(ctx context.Context, s *store.Store, userID, orderID, actor, reason string) (*types.Order, error) {
	ord, err := s.Order(ctx, orderID)
	if err != nil {
		return nil, err
	}
	if ord.IsTerminal() {
		return nil, fmt.Errorf("%w: order is %s", store.ErrNoTransition, ord.Status)
	}
	if _, err := s.TransitionOrder(ctx, ord.ID, ord.Status, types.StatusCancelled, actor, reason); err != nil {
		return nil, err
	}
	if _, err := w.Refund(ctx, userID, ord.ID, reason); err != nil && !errors.Is(err, ErrNotFound) {
		return nil, err
	}
	return s.Order(ctx, ord.ID)
}

// ---------------------------------------------------------------------------
// internals
// ---------------------------------------------------------------------------

// insert appends one ledger row. dedupeKey is the hard uniqueness guard;
// idemKey is the caller-facing retry guard. Both are UNIQUE.
func (w *Wallet) insert(ctx context.Context, l Ledger, dedupeKey string) (*Ledger, error) {
	if l.Currency == "" {
		l.Currency = w.currency
	}
	res, err := w.db.ExecContext(ctx, `INSERT INTO wallet_ledger
		(user_id, kind, amount_minor, currency, order_id, idempotency_key, dedupe_key, memo, created_at)
		VALUES (?,?,?,?,?,?,?,?,?)`,
		l.UserID, l.Kind, l.AmountMinor, l.Currency, l.OrderID, l.IdempotencyKey, dedupeKey, l.Memo,
		w.now().UTC().Format(store.TimeLayout))
	if err != nil {
		if isUnique(err) {
			return nil, fmt.Errorf("%w: ledger %s", store.ErrDuplicate, dedupeKey)
		}
		return nil, fmt.Errorf("wallet: insert ledger: %w", err)
	}
	id, _ := res.LastInsertId()
	l.ID = id
	l.CreatedAt = w.now()
	return &l, nil
}

// txSum is the in-transaction balance read. Every balance-dependent decision
// inside PayOrder must go through it: the pool has a single connection, so a
// second connection would block until the transaction finishes (forever).
func txSum(tx *sql.Tx, ctx context.Context, uid, currency string) (int64, error) {
	var sum int64
	err := tx.QueryRowContext(ctx, `SELECT COALESCE(SUM(amount_minor),0) FROM wallet_ledger
		WHERE user_id = ? AND currency = ?`, uid, currency).Scan(&sum)
	if err != nil {
		return 0, fmt.Errorf("wallet: balance in tx: %w", err)
	}
	return sum, nil
}

func normUser(uid string) string {
	uid = strings.TrimSpace(uid)
	if uid == "" {
		return "default"
	}
	return uid
}

func isUnique(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "UNIQUE constraint failed") || strings.Contains(msg, "constraint failed: UNIQUE")
}
