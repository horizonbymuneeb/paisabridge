package match_test

// Coverage for the matcher's rejection paths and the admin approve branch.
// Every case here is a way a real webhook can fail to match, and the point of
// each assertion is that money never moves on a guess.

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/horizonbymuneeb/paisabridge/internal/match"
	"github.com/horizonbymuneeb/paisabridge/internal/store"
	"github.com/horizonbymuneeb/paisabridge/internal/types"
)

// ---------------------------------------------------------------------------
// input validation
// ---------------------------------------------------------------------------

func TestNewPanicsOnNilStore(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("New(nil) must panic rather than build a matcher that cannot match")
		}
	}()
	_ = match.New(nil)
}

func TestProcessNilNotice(t *testing.T) {
	e := newEnv(t)
	out, err := e.matcher.Process(context.Background(), nil)
	if err == nil {
		t.Fatal("a nil notice must be an error")
	}
	if out.Matched {
		t.Fatal("a nil notice must never match")
	}
	if out.Reason != match.ReasonZeroAmount {
		t.Fatalf("want reason %q, got %q", match.ReasonZeroAmount, out.Reason)
	}
}

func TestProcessBlankTxnRef(t *testing.T) {
	e := newEnv(t)
	o := e.order(t, 150000, 30*time.Minute)
	n := e.notice(o, "   ")
	out, err := e.matcher.Process(context.Background(), n)
	if err == nil {
		t.Fatal("a blank txn_ref must be an error: it is the only dedupe key")
	}
	if out.Matched {
		t.Fatal("a blank txn_ref must never match")
	}
	if out.Reason != match.ReasonDuplicateTxn {
		t.Fatalf("want reason %q, got %q", match.ReasonDuplicateTxn, out.Reason)
	}
}

// A txn_ref with surrounding whitespace is the same payment as the trimmed
// one: the matcher must normalise before the duplicate check, otherwise a
// replayed webhook with a stray space would match twice.
func TestTxnRefIsTrimmedBeforeDedupe(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	o := e.order(t, 150000, 30*time.Minute)

	first := e.notice(o, "TX-SPACE")
	first.TxnRef = "  TX-SPACE  "
	out, err := e.matcher.Process(ctx, first)
	if err != nil {
		t.Fatalf("first: %v", err)
	}
	if !out.Matched {
		t.Fatalf("first payment must match, got %+v", out)
	}

	second := e.notice(o, "TX-SPACE")
	out, err = e.matcher.Process(ctx, second)
	if err != nil {
		t.Fatalf("replay: %v", err)
	}
	if out.Matched {
		t.Fatal("the trimmed duplicate must not match again")
	}
	if out.Reason != match.ReasonDuplicateTxn {
		t.Fatalf("want %q, got %q", match.ReasonDuplicateTxn, out.Reason)
	}
}

// ---------------------------------------------------------------------------
// currency defaulting
// ---------------------------------------------------------------------------

// A notice that omits the currency is assumed to carry the rail's own
// currency. Getting this wrong would let a PKR amount settle a USDT order.
func TestDefaultCurrencyPerRail(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()

	usdt, err := e.store.CreateOrder(ctx, store.CreateOrderInput{
		ProductID: "p", CatalogPrice: 1_500_000, Currency: "USDT", Method: types.MethodUSDT,
	})
	if err != nil {
		t.Fatalf("create usdt order: %v", err)
	}
	n := &types.PaymentNotice{
		TxnRef: "TX-USDT", Method: types.MethodUSDT, Amount: usdt.Amount,
		// Currency deliberately empty: the matcher must infer USDT.
	}
	out, err := e.matcher.Process(ctx, n)
	if err != nil {
		t.Fatalf("process: %v", err)
	}
	if !out.Matched {
		t.Fatalf("USDT notice without a currency must still match, got %+v", out)
	}
	if out.OrderID != usdt.ID {
		t.Fatalf("matched the wrong order: got %s, want %s", out.OrderID, usdt.ID)
	}
}

func TestDefaultCurrencyBinanceIsUSDT(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	o, err := e.store.CreateOrder(ctx, store.CreateOrderInput{
		ProductID: "p", CatalogPrice: 2_000_000, Currency: "USDT", Method: types.MethodBinance,
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	out, err := e.matcher.Process(ctx, &types.PaymentNotice{
		TxnRef: "TX-BINANCE", Method: types.MethodBinance, Amount: o.Amount,
	})
	if err != nil {
		t.Fatalf("process: %v", err)
	}
	if !out.Matched {
		t.Fatalf("binance must default to USDT and match, got %+v", out)
	}
}

// A PKR-rail notice with no currency must default to PKR, not to whatever the
// first row happens to be.
func TestDefaultCurrencyNonStablecoinIsPKR(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	var matched int
	for _, m := range []types.Method{
		types.MethodEasypaisa, types.MethodJazzCash, types.MethodNayaPay,
		types.MethodSadaPay, types.MethodBank,
	} {
		o, err := e.store.CreateOrder(ctx, store.CreateOrderInput{
			ProductID: "p", CatalogPrice: 150000, Currency: "PKR", Method: m,
		})
		if err != nil {
			t.Fatalf("create %s: %v", m, err)
		}
		out, err := e.matcher.Process(ctx, &types.PaymentNotice{
			TxnRef: "TX-" + string(m), Method: m, Amount: o.Amount,
		})
		if err != nil {
			t.Fatalf("process %s: %v", m, err)
		}
		if !out.Matched {
			t.Fatalf("%s: PKR default did not match, got %+v", m, out)
		}
		matched++
	}
	if matched != 5 {
		t.Fatalf("want 5 rails matched, got %d", matched)
	}
}

// ---------------------------------------------------------------------------
// zero / negative amounts
// ---------------------------------------------------------------------------

// A zero-amount notice is recorded (so an admin can see the garbage feed) but
// never applied to an order.
func TestZeroAndNegativeAmountsAreRecordedNotMatched(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	o := e.order(t, 150000, 30*time.Minute)

	for i, amt := range []int64{0, -1, -500000} {
		n := &types.PaymentNotice{TxnRef: "TX-Z" + itoa(int64(i)), Method: types.MethodEasypaisa, Amount: amt}
		out, err := e.matcher.Process(ctx, n)
		if err != nil {
			t.Fatalf("amount %d: %v", amt, err)
		}
		if out.Matched || out.Reason != match.ReasonZeroAmount {
			t.Fatalf("amount %d: want %q and no match, got %+v", amt, match.ReasonZeroAmount, out)
		}
	}
	// The orders themselves are untouched.
	st, err := e.store.OrderStatus(ctx, o.ID)
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	if st != types.StatusPendingPayment {
		t.Fatalf("a junk-amount notice must not move an order, got %s", st)
	}
}

// ---------------------------------------------------------------------------
// no matching order
// ---------------------------------------------------------------------------

func TestNoOpenOrderForAmount(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	_ = e.order(t, 150000, 30*time.Minute)

	out, err := e.matcher.Process(ctx, &types.PaymentNotice{
		TxnRef: "TX-ORPHAN", Method: types.MethodEasypaisa, Amount: 999900, Currency: "PKR",
	})
	if err != nil {
		t.Fatalf("process: %v", err)
	}
	if out.Matched || out.Reason != match.ReasonNoOrder {
		t.Fatalf("want %q and no match, got %+v", match.ReasonNoOrder, out)
	}
}

// An amount that is open on another rail must not match: the amount is only
// unique per (method, currency).
func TestAmountOnAnotherRailDoesNotMatch(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	o, err := e.store.CreateOrder(ctx, store.CreateOrderInput{
		ProductID: "p", CatalogPrice: 250000, Currency: "PKR", Method: types.MethodJazzCash,
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	out, err := e.matcher.Process(ctx, &types.PaymentNotice{
		TxnRef: "TX-WRONG-RAIL", Method: types.MethodEasypaisa, Amount: o.Amount, Currency: "PKR",
	})
	if err != nil {
		t.Fatalf("process: %v", err)
	}
	if out.Matched {
		t.Fatal("an easypaisa notice must not settle a jazzcash order")
	}
	st, _ := e.store.OrderStatus(ctx, o.ID)
	if st != types.StatusPendingPayment {
		t.Fatalf("the jazzcash order must be untouched, got %s", st)
	}
}

// A closed (cancelled) order must not be matched by a late payment.
func TestCancelledOrderIsNotMatched(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	o := e.order(t, 150000, 30*time.Minute)
	if _, err := e.store.TransitionOrder(ctx, o.ID, types.StatusPendingPayment, types.StatusCancelled, "test", "cancel"); err != nil {
		t.Fatalf("cancel: %v", err)
	}
	out, err := e.matcher.Process(ctx, e.notice(o, "TX-LATE"))
	if err != nil {
		t.Fatalf("process: %v", err)
	}
	if out.Matched {
		t.Fatal("a cancelled order must not be matched")
	}
	if out.Reason != match.ReasonNoOrder {
		t.Fatalf("want %q, got %q", match.ReasonNoOrder, out.Reason)
	}
}

// ---------------------------------------------------------------------------
// already-bound txn ref
// ---------------------------------------------------------------------------

// If an order already carries this txn_ref (say a manual approval bound it),
// a fresh notice for the same ref is a duplicate, not a second payment.
func TestNoticeForAlreadyBoundRefIsDuplicate(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	o := e.order(t, 150000, 30*time.Minute)
	if err := e.store.BindTxRef(ctx, o.ID, "TX-BOUND"); err != nil {
		t.Fatalf("bind: %v", err)
	}
	out, err := e.matcher.Process(ctx, e.notice(o, "TX-BOUND"))
	if err != nil {
		t.Fatalf("process: %v", err)
	}
	if out.Matched {
		t.Fatal("an already-bound ref must not match a second time")
	}
	if out.Reason != match.ReasonAlreadyMatched {
		t.Fatalf("want %q, got %q", match.ReasonAlreadyMatched, out.Reason)
	}
	if out.OrderID != o.ID {
		t.Fatalf("the outcome should name the order it is already bound to, got %q", out.OrderID)
	}
}

// ---------------------------------------------------------------------------
// expiry boundary
// ---------------------------------------------------------------------------

// Expiry is exclusive at the far end: a payment one second before ExpiresAt
// matches, one at exactly ExpiresAt does not.
func TestExpiryBoundaryIsExclusive(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	o := e.order(t, 150000, 10*time.Minute)
	expires := o.ExpiresAt

	// One second early: still inside the window.
	*e.now = expires.Add(-time.Second)
	in := e.notice(o, "TX-EARLY")
	in.ReceivedAt = *e.now
	out, err := e.matcher.Process(ctx, in)
	if err != nil {
		t.Fatalf("process early: %v", err)
	}
	if !out.Matched {
		t.Fatalf("a payment inside the window must match, got %+v", out)
	}

	// Fresh order, this time exactly at the boundary.
	o2 := e.order(t, 250000, 10*time.Minute)
	*e.now = o2.ExpiresAt
	out, err = e.matcher.Process(ctx, e.notice(o2, "TX-EXACT"))
	if err != nil {
		t.Fatalf("process exact: %v", err)
	}
	if out.Matched {
		t.Fatal("a payment exactly at ExpiresAt must be treated as late")
	}
	if out.Reason != match.ReasonExpired {
		t.Fatalf("want %q, got %q", match.ReasonExpired, out.Reason)
	}
}

// ---------------------------------------------------------------------------
// Approve: the admin escape hatch
// ---------------------------------------------------------------------------

func TestApproveRequiresAnOrder(t *testing.T) {
	e := newEnv(t)
	if _, err := e.matcher.Approve(context.Background(), nil, "TX-1", "admin"); err == nil {
		t.Fatal("Approve(nil) must be an error")
	}
	if _, err := e.matcher.Approve(context.Background(), &types.Order{}, "TX-1", "admin"); err == nil {
		t.Fatal("Approve with an empty order id must be an error")
	}
}

func TestApproveMovesReviewToVerifiedAndResolvesNotice(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	o := e.order(t, 150000, 30*time.Minute)

	// Get the order into review via an ambiguous amount, then clear it.
	if _, err := e.store.TransitionOrder(ctx, o.ID, types.StatusPendingPayment, types.StatusReview, "test", "ambiguous"); err != nil {
		t.Fatalf("to review: %v", err)
	}
	// The admin decides the payment belongs to this order.
	if err := e.store.RecordNotice(ctx, &types.PaymentNotice{
		Method: types.MethodEasypaisa, TxnRef: "TX-MANUAL", Amount: o.Amount, Currency: "PKR",
	}); err != nil {
		t.Fatalf("record notice: %v", err)
	}
	review, err := e.store.Order(ctx, o.ID)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	out, err := e.matcher.Approve(ctx, review, "TX-MANUAL", "ops-alice")
	if err != nil {
		t.Fatalf("approve: %v", err)
	}
	if !out.Matched || out.OrderID != o.ID {
		t.Fatalf("approve must match the order, got %+v", out)
	}

	st, err := e.store.OrderStatus(ctx, o.ID)
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	if st != types.StatusVerified {
		t.Fatalf("approved order must be verified, got %s", st)
	}
	// The journal must attribute the transition to the admin, and the notice
	// must be resolved as matched so an admin does not see it twice.
	ev, err := e.store.Events(ctx, o.ID)
	if err != nil {
		t.Fatalf("events: %v", err)
	}
	var sawAdmin bool
	for _, x := range ev {
		if x.ToState == types.StatusVerified && x.Actor == "ops-alice" {
			sawAdmin = true
		}
	}
	if !sawAdmin {
		t.Fatalf("journal must record the approving admin: %+v", ev)
	}
	got, err := e.store.Order(ctx, o.ID)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if got.TxRef != "TX-MANUAL" {
		t.Fatalf("approve must bind the txn ref, got %q", got.TxRef)
	}
}

func TestApproveDefaultsActorAndAllowsEmptyTxnRef(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	o := e.order(t, 150000, 30*time.Minute)
	ord, err := e.store.Order(ctx, o.ID)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	out, err := e.matcher.Approve(ctx, ord, "", "")
	if err != nil {
		t.Fatalf("approve: %v", err)
	}
	if !out.Matched {
		t.Fatalf("approve without a txn ref must still verify, got %+v", out)
	}
	ev, err := e.store.Events(ctx, o.ID)
	if err != nil {
		t.Fatalf("events: %v", err)
	}
	if len(ev) < 2 || ev[len(ev)-1].Actor != "admin" {
		t.Fatalf("empty actor must default to \"admin\": %+v", ev)
	}
}

func TestApproveIsIdempotent(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	o := e.order(t, 150000, 30*time.Minute)
	ord, _ := e.store.Order(ctx, o.ID)
	if _, err := e.matcher.Approve(ctx, ord, "TX-ONCE", "admin"); err != nil {
		t.Fatalf("first approve: %v", err)
	}
	// The stale handle still says pending_payment; the second approve must
	// lose the compare-and-set and report "already matched" rather than
	// writing a second journal row.
	out, err := e.matcher.Approve(ctx, ord, "TX-ONCE", "admin")
	if err != nil {
		t.Fatalf("second approve: %v", err)
	}
	if out.Matched {
		t.Fatal("a replayed approve must not report a fresh match")
	}
	if out.Reason != match.ReasonAlreadyMatched {
		t.Fatalf("want %q, got %q", match.ReasonAlreadyMatched, out.Reason)
	}
}

// Approving an order that is already bound to a DIFFERENT txn_ref is a
// conflict: two payments would be claiming one order.
func TestApproveConflictsOnForeignTxnRef(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	o := e.order(t, 150000, 30*time.Minute)
	if err := e.store.BindTxRef(ctx, o.ID, "TX-ORIGINAL"); err != nil {
		t.Fatalf("bind: %v", err)
	}
	ord, err := e.store.Order(ctx, o.ID)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	out, err := e.matcher.Approve(ctx, ord, "TX-DIFFERENT", "admin")
	if err != nil {
		t.Fatalf("approve: %v", err)
	}
	if out.Matched {
		t.Fatal("approve must not overwrite an existing txn ref")
	}
	if out.Reason != match.ReasonConflict {
		t.Fatalf("want %q, got %q", match.ReasonConflict, out.Reason)
	}
	got, _ := e.store.Order(ctx, o.ID)
	if got.TxRef != "TX-ORIGINAL" {
		t.Fatalf("the original binding must survive, got %q", got.TxRef)
	}
	if got.Status != types.StatusPendingPayment {
		t.Fatalf("a conflicting approve must not move the order, got %s", got.Status)
	}
}

// ---------------------------------------------------------------------------
// fake-store driven branches
// ---------------------------------------------------------------------------

// scriptedStore lets us drive the storage-error branches that a healthy SQLite
// cannot produce on demand.
type scriptedStore struct {
	noticeRecorded func(ctx context.Context, m types.Method, ref string) (bool, error)
	recordNotice   func(ctx context.Context, n *types.PaymentNotice) error
	resolveNotice  func(ctx context.Context, m types.Method, ref, orderID, outcome, status string) error
	findOpen       func(ctx context.Context, m types.Method, cur string, amt int64) (*types.Order, error)
	countOpen      func(ctx context.Context, m types.Method, cur string, amt int64) (int, error)
	bindTxRef      func(ctx context.Context, id, ref string) error
	orderByTxRef   func(ctx context.Context, m types.Method, ref string) (*types.Order, error)
	transition     func(ctx context.Context, id string, from, to types.OrderStatus, actor, detail string) (*types.Order, error)
	now            func() time.Time

	resolved []string
}

func (f *scriptedStore) NoticeRecorded(ctx context.Context, m types.Method, ref string) (bool, error) {
	if f.noticeRecorded != nil {
		return f.noticeRecorded(ctx, m, ref)
	}
	return false, nil
}
func (f *scriptedStore) RecordNotice(ctx context.Context, n *types.PaymentNotice) error {
	if f.recordNotice != nil {
		return f.recordNotice(ctx, n)
	}
	return nil
}
func (f *scriptedStore) ResolveNotice(ctx context.Context, m types.Method, ref, orderID, outcome, status string) error {
	f.resolved = append(f.resolved, outcome+"/"+status)
	if f.resolveNotice != nil {
		return f.resolveNotice(ctx, m, ref, orderID, outcome, status)
	}
	return nil
}
func (f *scriptedStore) FindOpenOrderByAmount(ctx context.Context, m types.Method, cur string, amt int64) (*types.Order, error) {
	if f.findOpen != nil {
		return f.findOpen(ctx, m, cur, amt)
	}
	return nil, store.ErrNotFound
}
func (f *scriptedStore) CountOpenOrdersByAmount(ctx context.Context, m types.Method, cur string, amt int64) (int, error) {
	if f.countOpen != nil {
		return f.countOpen(ctx, m, cur, amt)
	}
	return 0, nil
}
func (f *scriptedStore) BindTxRef(ctx context.Context, id, ref string) error {
	if f.bindTxRef != nil {
		return f.bindTxRef(ctx, id, ref)
	}
	return nil
}
func (f *scriptedStore) OrderByTxRef(ctx context.Context, m types.Method, ref string) (*types.Order, error) {
	if f.orderByTxRef != nil {
		return f.orderByTxRef(ctx, m, ref)
	}
	return nil, store.ErrNotFound
}
func (f *scriptedStore) TransitionOrder(ctx context.Context, id string, from, to types.OrderStatus, actor, detail string) (*types.Order, error) {
	if f.transition != nil {
		return f.transition(ctx, id, from, to, actor, detail)
	}
	return &types.Order{ID: id, Status: to}, nil
}
func (f *scriptedStore) Now() time.Time {
	if f.now != nil {
		return f.now()
	}
	return time.Date(2026, 10, 31, 12, 0, 0, 0, time.UTC)
}

func goodNotice() *types.PaymentNotice {
	return &types.PaymentNotice{TxnRef: "TX-FAKE", Method: types.MethodEasypaisa, Amount: 150000, Currency: "PKR"}
}

func goodOrder() *types.Order {
	return &types.Order{
		ID: "ord_1", Method: types.MethodEasypaisa, Currency: "PKR", Amount: 150000,
		Status: types.StatusPendingPayment,
	}
}

func TestFakeStoreStorageErrors(t *testing.T) {
	ctx := context.Background()
	boom := errors.New("disk on fire")

	t.Run("NoticeRecorded error aborts before any write", func(t *testing.T) {
		f := &scriptedStore{noticeRecorded: func(context.Context, types.Method, string) (bool, error) {
			return false, boom
		}}
		out, err := match.New(f).Process(ctx, goodNotice())
		if !errors.Is(err, boom) {
			t.Fatalf("want the storage error, got %v", err)
		}
		if out.Matched {
			t.Fatal("must not match when the duplicate check failed")
		}
	})

	t.Run("zero amount record error is surfaced", func(t *testing.T) {
		f := &scriptedStore{recordNotice: func(context.Context, *types.PaymentNotice) error { return boom }}
		n := goodNotice()
		n.Amount = 0
		out, err := match.New(f).Process(ctx, n)
		if !errors.Is(err, boom) {
			t.Fatalf("want the storage error, got %v", err)
		}
		if out.Reason != match.ReasonZeroAmount {
			t.Fatalf("want %q, got %q", match.ReasonZeroAmount, out.Reason)
		}
	})

	t.Run("zero amount duplicate is swallowed", func(t *testing.T) {
		f := &scriptedStore{recordNotice: func(context.Context, *types.PaymentNotice) error {
			return store.ErrDuplicate
		}}
		n := goodNotice()
		n.Amount = 0
		out, err := match.New(f).Process(ctx, n)
		if err != nil {
			t.Fatalf("a duplicate zero-amount notice is not an error, got %v", err)
		}
		if out.Reason != match.ReasonZeroAmount {
			t.Fatalf("want %q, got %q", match.ReasonZeroAmount, out.Reason)
		}
	})

	t.Run("OrderByTxRef storage error", func(t *testing.T) {
		f := &scriptedStore{orderByTxRef: func(context.Context, types.Method, string) (*types.Order, error) {
			return nil, boom
		}}
		out, err := match.New(f).Process(ctx, goodNotice())
		if !errors.Is(err, boom) {
			t.Fatalf("want the storage error, got %v", err)
		}
		if out.Matched {
			t.Fatal("must not match when the ref lookup failed")
		}
	})

	t.Run("RecordNotice storage error is retryable", func(t *testing.T) {
		f := &scriptedStore{recordNotice: func(context.Context, *types.PaymentNotice) error { return boom }}
		_, err := match.New(f).Process(ctx, goodNotice())
		if !errors.Is(err, boom) {
			t.Fatalf("want the storage error, got %v", err)
		}
	})

	t.Run("CountOpenOrders error routes to review", func(t *testing.T) {
		f := &scriptedStore{countOpen: func(context.Context, types.Method, string, int64) (int, error) {
			return 0, boom
		}}
		out, err := match.New(f).Process(ctx, goodNotice())
		if !errors.Is(err, boom) {
			t.Fatalf("want the storage error, got %v", err)
		}
		if out.Matched {
			t.Fatal("an unreadable count must never match")
		}
		if out.Reason != match.ReasonAmbiguous {
			t.Fatalf("want %q, got %q", match.ReasonAmbiguous, out.Reason)
		}
	})

	t.Run("count zero is no-open-order", func(t *testing.T) {
		f := &scriptedStore{}
		out, err := match.New(f).Process(ctx, goodNotice())
		if err != nil {
			t.Fatalf("process: %v", err)
		}
		if out.Reason != match.ReasonNoOrder {
			t.Fatalf("want %q, got %q", match.ReasonNoOrder, out.Reason)
		}
	})

	t.Run("FindOpenOrder storage error", func(t *testing.T) {
		f := &scriptedStore{
			countOpen: func(context.Context, types.Method, string, int64) (int, error) { return 1, nil },
			findOpen: func(context.Context, types.Method, string, int64) (*types.Order, error) {
				return nil, boom
			},
		}
		_, err := match.New(f).Process(ctx, goodNotice())
		if !errors.Is(err, boom) {
			t.Fatalf("want the storage error, got %v", err)
		}
	})

	t.Run("currency mismatch names the order", func(t *testing.T) {
		f := &scriptedStore{
			countOpen: func(context.Context, types.Method, string, int64) (int, error) { return 1, nil },
			findOpen: func(context.Context, types.Method, string, int64) (*types.Order, error) {
				o := goodOrder()
				o.Currency = "USDT"
				return o, nil
			},
		}
		out, err := match.New(f).Process(ctx, goodNotice())
		if err != nil {
			t.Fatalf("process: %v", err)
		}
		if out.Matched {
			t.Fatal("a currency mismatch must never match")
		}
		if out.Reason != match.ReasonCurrency {
			t.Fatalf("want %q, got %q", match.ReasonCurrency, out.Reason)
		}
	})

	t.Run("BindTxRef not found", func(t *testing.T) {
		f := &scriptedStore{
			countOpen: func(context.Context, types.Method, string, int64) (int, error) { return 1, nil },
			findOpen: func(context.Context, types.Method, string, int64) (*types.Order, error) {
				return goodOrder(), nil
			},
			bindTxRef: func(context.Context, string, string) error { return store.ErrNotFound },
		}
		out, err := match.New(f).Process(ctx, goodNotice())
		if err != nil {
			t.Fatalf("process: %v", err)
		}
		if out.Reason != match.ReasonNoOrder {
			t.Fatalf("want %q, got %q", match.ReasonNoOrder, out.Reason)
		}
	})

	t.Run("BindTxRef conflict", func(t *testing.T) {
		f := &scriptedStore{
			countOpen: func(context.Context, types.Method, string, int64) (int, error) { return 1, nil },
			findOpen: func(context.Context, types.Method, string, int64) (*types.Order, error) {
				return goodOrder(), nil
			},
			bindTxRef: func(context.Context, string, string) error { return store.ErrConflict },
		}
		out, err := match.New(f).Process(ctx, goodNotice())
		if err != nil {
			t.Fatalf("process: %v", err)
		}
		if out.Reason != match.ReasonConflict {
			t.Fatalf("want %q, got %q", match.ReasonConflict, out.Reason)
		}
	})

	t.Run("TransitionOrder non-CAS error is surfaced", func(t *testing.T) {
		f := &scriptedStore{
			countOpen: func(context.Context, types.Method, string, int64) (int, error) { return 1, nil },
			findOpen: func(context.Context, types.Method, string, int64) (*types.Order, error) {
				return goodOrder(), nil
			},
			transition: func(context.Context, string, types.OrderStatus, types.OrderStatus, string, string) (*types.Order, error) {
				return nil, boom
			},
		}
		_, err := match.New(f).Process(ctx, goodNotice())
		if !errors.Is(err, boom) {
			t.Fatalf("want the storage error, got %v", err)
		}
	})

	t.Run("TransitionOrder race reports already matched", func(t *testing.T) {
		f := &scriptedStore{
			countOpen: func(context.Context, types.Method, string, int64) (int, error) { return 1, nil },
			findOpen: func(context.Context, types.Method, string, int64) (*types.Order, error) {
				return goodOrder(), nil
			},
			transition: func(context.Context, string, types.OrderStatus, types.OrderStatus, string, string) (*types.Order, error) {
				return nil, store.ErrNoTransition
			},
		}
		out, err := match.New(f).Process(ctx, goodNotice())
		if err != nil {
			t.Fatalf("a lost race is not an error, got %v", err)
		}
		if out.Reason != match.ReasonAlreadyMatched {
			t.Fatalf("want %q, got %q", match.ReasonAlreadyMatched, out.Reason)
		}
	})

	t.Run("Approve bind error is surfaced", func(t *testing.T) {
		f := &scriptedStore{bindTxRef: func(context.Context, string, string) error { return boom }}
		_, err := match.New(f).Approve(ctx, goodOrder(), "TX-1", "admin")
		if !errors.Is(err, boom) {
			t.Fatalf("want the storage error, got %v", err)
		}
	})

	t.Run("Approve transition error is surfaced", func(t *testing.T) {
		f := &scriptedStore{transition: func(context.Context, string, types.OrderStatus, types.OrderStatus, string, string) (*types.Order, error) {
			return nil, boom
		}}
		_, err := match.New(f).Approve(ctx, goodOrder(), "", "admin")
		if !errors.Is(err, boom) {
			t.Fatalf("want the storage error, got %v", err)
		}
	})
}

// A single open order whose ExpiresAt is zero is treated as "no expiry"
// rather than instantly late.
func TestZeroExpiryIsNotLate(t *testing.T) {
	f := &scriptedStore{
		countOpen: func(context.Context, types.Method, string, int64) (int, error) { return 1, nil },
		findOpen: func(context.Context, types.Method, string, int64) (*types.Order, error) {
			o := goodOrder()
			o.ExpiresAt = time.Time{}
			return o, nil
		},
	}
	out, err := match.New(f).Process(context.Background(), goodNotice())
	if err != nil {
		t.Fatalf("process: %v", err)
	}
	if !out.Matched {
		t.Fatalf("an order with no expiry must be matchable, got %+v", out)
	}
}

// Integration: the actor label on Process comes from the Matcher field, and
// the two-rail isolation holds end to end.
func TestTwoRailsSameAmountStayIsolated(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()

	// Force the same amount on two rails by creating then retargeting.
	ep, err := e.store.CreateOrder(ctx, store.CreateOrderInput{
		ProductID: "p", CatalogPrice: 300000, Currency: "PKR", Method: types.MethodEasypaisa,
	})
	if err != nil {
		t.Fatalf("create easypaisa: %v", err)
	}
	jc, err := e.store.CreateOrder(ctx, store.CreateOrderInput{
		ProductID: "p", CatalogPrice: 300000, Currency: "PKR", Method: types.MethodJazzCash,
	})
	if err != nil {
		t.Fatalf("create jazzcash: %v", err)
	}
	if _, err := e.store.DB().ExecContext(ctx, `UPDATE orders SET amount = ? WHERE id IN (?,?)`,
		ep.Amount, ep.ID, jc.ID); err != nil {
		t.Fatalf("retarget: %v", err)
	}

	// The easypaisa payment must settle the easypaisa order only.
	out, err := e.matcher.Process(ctx, &types.PaymentNotice{
		TxnRef: "TX-EP", Method: types.MethodEasypaisa, Amount: ep.Amount, Currency: "PKR",
	})
	if err != nil {
		t.Fatalf("process: %v", err)
	}
	if !out.Matched || out.OrderID != ep.ID {
		t.Fatalf("want the easypaisa order matched, got %+v", out)
	}
	st, err := e.store.OrderStatus(ctx, jc.ID)
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	if st != types.StatusPendingPayment {
		t.Fatalf("the jazzcash order must be untouched, got %s", st)
	}
}

func itoa(n int64) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}

var _ = filepath.Join
