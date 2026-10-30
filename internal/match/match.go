// Package match resolves an inbound PaymentNotice to exactly one order.
//
// A payment notice resolves to at most one order through these rules:
//
//  1. Every open order has an amount unique for its (method, currency), so
//     the payer never types a reference — the amount alone identifies the
//     order.
//  2. A notice is only matched when exactly ONE open order in the same
//     method/currency has that exact amount. Two open orders with the same
//     amount is a data-integrity bug (the store enforces uniqueness); the
//     matcher refuses to guess and routes the payment to review instead.
//  3. The order must not be expired. An expired order is closed (and
//     refunded by the wallet), and a payment against it is recorded but left
//     for an admin, never auto-fulfilled.
//  4. Every (method, txn_ref) is processed at most once. Replayed webhooks
//     are recognised before any state change, and the notice insert itself is
//     the hard guard.
//
// The package owns no SQL: it talks to the Store interface below, which
// *store.Store implements. That keeps the business rules unit-testable with a
// tiny fake and free of timing assumptions.
package match

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/horizonbymuneeb/paisabridge/internal/store"
	"github.com/horizonbymuneeb/paisabridge/internal/types"
)

// Outcome reasons. Stable strings: the API returns them and the tests and
// ops dashboards depend on them.
const (
	ReasonMatched        = "matched"
	ReasonDuplicateTxn   = "duplicate_txn_ref"
	ReasonAlreadyMatched = "already_matched"
	ReasonAmbiguous      = "ambiguous_amount"
	ReasonExpired        = "order_expired"
	ReasonNoOrder        = "no_open_order"
	ReasonCurrency       = "currency_mismatch"
	ReasonZeroAmount     = "invalid_amount"
	ReasonConflict       = "txn_ref_conflict"
)

// Store is what the matcher needs. *store.Store satisfies it.
type Store interface {
	NoticeRecorded(ctx context.Context, method types.Method, txnRef string) (bool, error)
	RecordNotice(ctx context.Context, n *types.PaymentNotice) error
	ResolveNotice(ctx context.Context, method types.Method, txnRef, orderID, outcome, status string) error
	FindOpenOrderByAmount(ctx context.Context, method types.Method, currency string, amount int64) (*types.Order, error)
	CountOpenOrdersByAmount(ctx context.Context, method types.Method, currency string, amount int64) (int, error)
	BindTxRef(ctx context.Context, id string, txnRef string) error
	OrderByTxRef(ctx context.Context, method types.Method, txnRef string) (*types.Order, error)
	TransitionOrder(ctx context.Context, id string, from, to types.OrderStatus, actor, detail string) (*types.Order, error)
	Now() time.Time
}

// Matcher evaluates notices against the order book.
type Matcher struct {
	store Store
	// TxnRefPrefix optionally namespaces inbound refs per adapter; adapters
	// already namespace their refs, so this is only for the generic feed.
	Actor string `json:"-"`
}

// New builds a Matcher. Nil store is a programming error and panics early.
func New(s Store) *Matcher {
	if s == nil {
		panic("match: nil store")
	}
	return &Matcher{store: s, Actor: "matcher"}
}

// Process records the notice (idempotently) and resolves it.
//
// Return semantics:
//   - Matched=true: exactly one order was verified and linked.
//   - Matched=false with a Reason: recorded but not auto-applied. The notice
//     is stored in payment_notices with status "unmatched" so an admin can
//     approve it by hand (POST /api/admin/approve).
//   - error: storage trouble. The notice may or may not be stored; callers
//     should treat it as retryable and rely on the duplicate guard on retry.
//
// A duplicate txn_ref is NOT an error: it returns Matched=false with
// ReasonDuplicateTxn, so a replaying webhook gets 200 and credits nothing.
func (m *Matcher) Process(ctx context.Context, n *types.PaymentNotice) (types.MatchOutcome, error) {
	if n == nil {
		return types.MatchOutcome{Reason: ReasonZeroAmount}, errors.New("match: nil notice")
	}
	if strings.TrimSpace(n.TxnRef) == "" {
		return types.MatchOutcome{Reason: ReasonDuplicateTxn}, errors.New("match: notice without txn_ref")
	}
	n.TxnRef = strings.TrimSpace(n.TxnRef)
	if n.Currency == "" {
		n.Currency = defaultCurrency(n.Method)
	}
	if n.Amount <= 0 {
		if err := m.store.RecordNotice(ctx, n); err != nil && !errors.Is(err, store.ErrDuplicate) {
			return types.MatchOutcome{Reason: ReasonZeroAmount}, err
		}
		_ = m.store.ResolveNotice(ctx, n.Method, n.TxnRef, "", ReasonZeroAmount, "unmatched")
		return types.MatchOutcome{Reason: ReasonZeroAmount}, nil
	}

	// 1. Hard duplicate guard BEFORE any state change.
	seen, err := m.store.NoticeRecorded(ctx, n.Method, n.TxnRef)
	if err != nil {
		return types.MatchOutcome{Reason: ReasonDuplicateTxn}, err
	}
	if seen {
		return types.MatchOutcome{Reason: ReasonDuplicateTxn}, nil
	}

	// 2. An order already bound to this ref (bell/duplicate with a new txn
	//    id from the provider, or a manual approval) is also a duplicate.
	if ord, err := m.store.OrderByTxRef(ctx, n.Method, n.TxnRef); err == nil && ord != nil {
		return types.MatchOutcome{Reason: ReasonAlreadyMatched, OrderID: ord.ID}, nil
	} else if err != nil && !errors.Is(err, store.ErrNotFound) {
		return types.MatchOutcome{Reason: ReasonAlreadyMatched}, err
	}

	// 3. Persist the notice: the PRIMARY KEY(method, txn_ref) is the real
	//    guard under concurrency, so a race loses here, not on the ledger.
	if err := m.store.RecordNotice(ctx, n); err != nil {
		if errors.Is(err, store.ErrDuplicate) {
			return types.MatchOutcome{Reason: ReasonDuplicateTxn}, nil
		}
		return types.MatchOutcome{Reason: ReasonNoOrder}, err
	}

	// 4. Ambiguity check: never auto-match a shared amount.
	count, err := m.store.CountOpenOrdersByAmount(ctx, n.Method, n.Currency, n.Amount)
	if err != nil {
		_ = m.store.ResolveNotice(ctx, n.Method, n.TxnRef, "", ReasonAmbiguous, "unmatched")
		return types.MatchOutcome{Reason: ReasonAmbiguous}, err
	}
	if count > 1 {
		_ = m.store.ResolveNotice(ctx, n.Method, n.TxnRef, "", ReasonAmbiguous, "review")
		return types.MatchOutcome{Reason: ReasonAmbiguous}, nil
	}
	if count == 0 {
		_ = m.store.ResolveNotice(ctx, n.Method, n.TxnRef, "", ReasonNoOrder, "unmatched")
		return types.MatchOutcome{Reason: ReasonNoOrder}, nil
	}

	ord, err := m.store.FindOpenOrderByAmount(ctx, n.Method, n.Currency, n.Amount)
	if errors.Is(err, store.ErrConflict) {
		_ = m.store.ResolveNotice(ctx, n.Method, n.TxnRef, "", ReasonAmbiguous, "review")
		return types.MatchOutcome{Reason: ReasonAmbiguous}, nil
	}
	if errors.Is(err, store.ErrNotFound) {
		_ = m.store.ResolveNotice(ctx, n.Method, n.TxnRef, "", ReasonNoOrder, "unmatched")
		return types.MatchOutcome{Reason: ReasonNoOrder}, nil
	}
	if err != nil {
		return types.MatchOutcome{Reason: ReasonNoOrder}, err
	}

	// 5. Method and currency must agree exactly (a PKR amount of 1500 must
	//    never settle a USDT order).
	if ord.Currency != n.Currency || ord.Method != n.Method {
		_ = m.store.ResolveNotice(ctx, n.Method, n.TxnRef, ord.ID, ReasonCurrency, "unmatched")
		return types.MatchOutcome{Reason: ReasonCurrency, OrderID: ord.ID}, nil
	}

	// 6. Expiry. The window is exclusive on both ends: a payment received
	//    after ExpiresAt is late even if the sweeper has not run yet.
	now := m.store.Now()
	if !ord.ExpiresAt.IsZero() && !now.Before(ord.ExpiresAt) {
		_ = m.store.ResolveNotice(ctx, n.Method, n.TxnRef, ord.ID, ReasonExpired, "late")
		return types.MatchOutcome{Reason: ReasonExpired, OrderID: ord.ID}, nil
	}

	// 7. Bind then verify. BindTxRef refuses to bind an order that already
	//    carries a different ref; that is a conflict, not a match.
	if err := m.store.BindTxRef(ctx, ord.ID, n.TxnRef); err != nil {
		reason := ReasonConflict
		if errors.Is(err, store.ErrNotFound) {
			reason = ReasonNoOrder
		}
		_ = m.store.ResolveNotice(ctx, n.Method, n.TxnRef, ord.ID, reason, "unmatched")
		return types.MatchOutcome{Reason: reason, OrderID: ord.ID}, nil
	}
	if _, err := m.store.TransitionOrder(ctx, ord.ID, ord.Status, types.StatusVerified, m.Actor, "payment "+string(n.Method)+" "+n.TxnRef); err != nil {
		if errors.Is(err, store.ErrNoTransition) {
			// Lost a race: someone else moved the order between our read and
			// our write. The ledger side is guarded by the notice key, so
			// reporting a duplicate is the honest answer.
			_ = m.store.ResolveNotice(ctx, n.Method, n.TxnRef, ord.ID, ReasonAlreadyMatched, "duplicate")
			return types.MatchOutcome{Reason: ReasonAlreadyMatched, OrderID: ord.ID}, nil
		}
		return types.MatchOutcome{Reason: ReasonConflict, OrderID: ord.ID}, err
	}

	_ = m.store.ResolveNotice(ctx, n.Method, n.TxnRef, ord.ID, ReasonMatched, "matched")
	return types.MatchOutcome{Matched: true, OrderID: ord.ID, Reason: ReasonMatched}, nil
}

// Approve is the admin path for notices the matcher will not auto-apply
// (ambiguous amount, expired-looking order, unmatched feed entry). It moves
// the order through the same state machine — review -> verified, or
// pending_payment -> verified — so nothing bypasses the journal.
//
// It requires an order that the admin picked; `store.ApproveOrder` does the
// load/transition work. Idempotent under replay: approving an already verified
// order returns ErrNoTransition from the store and credits nothing.
func (m *Matcher) Approve(ctx context.Context, ord *types.Order, txnRef, actor string) (types.MatchOutcome, error) {
	if ord == nil || ord.ID == "" {
		return types.MatchOutcome{}, errors.New("match: approve requires an order")
	}
	if actor == "" {
		actor = "admin"
	}
	if txnRef != "" {
		if err := m.store.BindTxRef(ctx, ord.ID, txnRef); err != nil {
			if errors.Is(err, store.ErrConflict) {
				return types.MatchOutcome{Reason: ReasonConflict, OrderID: ord.ID}, nil
			}
			return types.MatchOutcome{Reason: ReasonConflict, OrderID: ord.ID}, err
		}
	}
	if _, err := m.store.TransitionOrder(ctx, ord.ID, ord.Status, types.StatusVerified, actor, "manual approve "+txnRef); err != nil {
		if errors.Is(err, store.ErrNoTransition) {
			return types.MatchOutcome{Reason: ReasonAlreadyMatched, OrderID: ord.ID}, nil
		}
		return types.MatchOutcome{Reason: ReasonConflict, OrderID: ord.ID}, err
	}
	if txnRef != "" {
		_ = m.store.ResolveNotice(ctx, ord.Method, txnRef, ord.ID, "manual_approve", "matched")
	}
	return types.MatchOutcome{Matched: true, OrderID: ord.ID, Reason: "manual_approve"}, nil
}

func defaultCurrency(m types.Method) string {
	switch m {
	case types.MethodUSDT, types.MethodBinance:
		return "USDT"
	default:
		return "PKR"
	}
}
