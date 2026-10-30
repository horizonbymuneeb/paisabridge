package wallet_test

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/horizonbymuneeb/paisabridge/internal/store"
	"github.com/horizonbymuneeb/paisabridge/internal/types"
	"github.com/horizonbymuneeb/paisabridge/internal/wallet"
)

func newStore(t *testing.T, clock func() time.Time) *store.Store {
	t.Helper()
	s, err := store.Open(filepath.Join(t.TempDir(), "wallet.db"), store.WithClock(clock))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func newOrder(t *testing.T, s *store.Store, price int64, ttl time.Duration) *types.Order {
	t.Helper()
	o, err := s.CreateOrder(context.Background(), store.CreateOrderInput{
		ProductID: "p", ProductName: "x", CatalogPrice: price,
		Currency: "PKR", Method: types.MethodEasypaisa, TTL: ttl,
	})
	if err != nil {
		t.Fatalf("create order: %v", err)
	}
	return o
}

func TestTopupAndBalanceFromLedger(t *testing.T) {
	s := newStore(t, nil)
	w := wallet.New(s)
	ctx := context.Background()

	bal, err := w.Balance(ctx, "u1")
	if err != nil {
		t.Fatalf("balance: %v", err)
	}
	if bal.Available != 0 {
		t.Fatalf("fresh wallet balance = %d, want 0", bal.Available)
	}

	if _, err := w.Topup(ctx, "u1", 100000, "top-1", "easypaisa credit"); err != nil {
		t.Fatalf("topup: %v", err)
	}
	if _, err := w.Topup(ctx, "u1", 50000, "top-2", "jazzcash credit"); err != nil {
		t.Fatalf("topup 2: %v", err)
	}

	bal, _ = w.Balance(ctx, "u1")
	if bal.Available != 150000 {
		t.Fatalf("balance = %d, want 150000", bal.Available)
	}

	// Balance must equal the ledger sum, with two rows, no balance column.
	rows, err := w.History(ctx, "u1", 10)
	if err != nil {
		t.Fatalf("history: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("history rows = %d, want 2", len(rows))
	}
	var sum int64
	for _, r := range rows {
		sum += r.AmountMinor
	}
	if sum != bal.Available {
		t.Fatalf("ledger sum %d != reported balance %d", sum, bal.Available)
	}
	if _, ok := any(bal).(*wallet.Balance); !ok {
		t.Fatal("unreachable")
	}
}

func TestTopupIdempotent(t *testing.T) {
	s := newStore(t, nil)
	w := wallet.New(s)
	ctx := context.Background()

	for i := 0; i < 3; i++ {
		bal, err := w.Topup(ctx, "u1", 25000, "same-key", "retry")
		if err != nil {
			t.Fatalf("topup %d: %v", i, err)
		}
		if bal.Available != 25000 {
			t.Fatalf("topup %d balance = %d, want 25000 (replayed key credited again)", i, bal.Available)
		}
	}
	rows, _ := w.History(ctx, "u1", 10)
	if len(rows) != 1 {
		t.Fatalf("ledger rows = %d, want exactly 1 after 3 replays", len(rows))
	}
}

func TestTopupRequiresIdempotencyKey(t *testing.T) {
	s := newStore(t, nil)
	w := wallet.New(s)
	if _, err := w.Topup(context.Background(), "u1", 1000, "", ""); err == nil {
		t.Fatal("topup without idempotency key must fail")
	}
	if _, err := w.Topup(context.Background(), "u1", 0, "k", ""); err == nil {
		t.Fatal("topup of zero must fail")
	}
}

func TestConcurrentTopupsSameKeyCreditOnce(t *testing.T) {
	s := newStore(t, nil)
	w := wallet.New(s)
	ctx := context.Background()

	const n = 12
	var wg sync.WaitGroup
	errs := make(chan error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := w.Topup(ctx, "race", 10000, "race-key", "")
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent topup: %v", err)
		}
	}
	bal, _ := w.Balance(ctx, "race")
	if bal.Available != 10000 {
		t.Fatalf("balance after 12 concurrent same-key topups = %d, want 10000", bal.Available)
	}
}

func TestPayOrderWithDiscount(t *testing.T) {
	s := newStore(t, nil)
	w := wallet.New(s) // default 500 bps = 5%
	ctx := context.Background()

	if _, err := w.Topup(ctx, "u1", 100000, "t1", ""); err != nil {
		t.Fatalf("topup: %v", err)
	}
	o := newOrder(t, s, 50000, 30*time.Minute)

	res, err := w.PayOrder(ctx, "u1", o.ID)
	if err != nil {
		t.Fatalf("pay: %v", err)
	}
	// 5% of 50000 = 2500, so 47500 leaves the wallet.
	if res.DiscountMinor != 2500 {
		t.Fatalf("discount = %d, want 2500", res.DiscountMinor)
	}
	if res.ChargedMinor != 47500 {
		t.Fatalf("charged = %d, want 47500", res.ChargedMinor)
	}
	if res.Balance.Available != 52500 {
		t.Fatalf("balance = %d, want 52500", res.Balance.Available)
	}

	got, _ := s.Order(ctx, o.ID)
	if got.Status != types.StatusVerified {
		t.Fatalf("order status = %s, want verified", got.Status)
	}

	// The discount is visible in the ledger as its own row.
	var discountRows int
	if err := s.DB().QueryRowContext(ctx,
		`SELECT COUNT(*) FROM wallet_ledger WHERE kind = ? AND order_id = ?`, wallet.KindDiscount, o.ID).Scan(&discountRows); err != nil {
		t.Fatalf("count discount rows: %v", err)
	}
	if discountRows != 1 {
		t.Fatalf("discount rows = %d, want 1", discountRows)
	}
}

func TestDiscountBPSConfigurable(t *testing.T) {
	s := newStore(t, nil)
	w := wallet.New(s, wallet.WithDiscountBPS(1000)) // 10%
	ctx := context.Background()
	if _, err := w.Topup(ctx, "u1", 200000, "t", ""); err != nil {
		t.Fatalf("topup: %v", err)
	}
	o := newOrder(t, s, 100000, 30*time.Minute)
	res, err := w.PayOrder(ctx, "u1", o.ID)
	if err != nil {
		t.Fatalf("pay: %v", err)
	}
	if res.DiscountMinor != 10000 || res.ChargedMinor != 90000 {
		t.Fatalf("with 1000 bps: discount=%d charged=%d, want 10000/90000", res.DiscountMinor, res.ChargedMinor)
	}
}

func TestPayOrderInsufficientFunds(t *testing.T) {
	s := newStore(t, nil)
	w := wallet.New(s)
	ctx := context.Background()
	if _, err := w.Topup(ctx, "u1", 1000, "t", ""); err != nil {
		t.Fatalf("topup: %v", err)
	}
	o := newOrder(t, s, 50000, 30*time.Minute)

	_, err := w.PayOrder(ctx, "u1", o.ID)
	if !errors.Is(err, wallet.ErrInsufficientFunds) {
		t.Fatalf("want ErrInsufficientFunds, got %v", err)
	}
	bal, _ := w.Balance(ctx, "u1")
	if bal.Available != 1000 {
		t.Fatalf("balance changed to %d on a failed payment", bal.Available)
	}
	got, _ := s.Order(ctx, o.ID)
	if got.Status != types.StatusPendingPayment {
		t.Fatalf("order moved to %s on insufficient funds", got.Status)
	}
}

func TestPayOrderTwiceChargesOnce(t *testing.T) {
	s := newStore(t, nil)
	w := wallet.New(s)
	ctx := context.Background()
	if _, err := w.Topup(ctx, "u1", 100000, "t", ""); err != nil {
		t.Fatalf("topup: %v", err)
	}
	o := newOrder(t, s, 40000, 30*time.Minute)

	if _, err := w.PayOrder(ctx, "u1", o.ID); err != nil {
		t.Fatalf("first pay: %v", err)
	}
	second, err := w.PayOrder(ctx, "u1", o.ID)
	if err != nil {
		t.Fatalf("second pay: %v", err)
	}
	if !second.AlreadyPaid {
		t.Fatal("second pay should report AlreadyPaid")
	}
	bal, _ := w.Balance(ctx, "u1")
	// 100000 - (40000 - 2000 discount) = 62000
	if bal.Available != 62000 {
		t.Fatalf("balance = %d, want 62000 (double charge?)", bal.Available)
	}
}

func TestConcurrentPayOrderChargesOnce(t *testing.T) {
	s := newStore(t, nil)
	w := wallet.New(s)
	ctx := context.Background()
	if _, err := w.Topup(ctx, "u1", 200000, "t", ""); err != nil {
		t.Fatalf("topup: %v", err)
	}
	o := newOrder(t, s, 40000, 30*time.Minute)

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = w.PayOrder(ctx, "u1", o.ID)
		}()
	}
	wg.Wait()

	bal, _ := w.Balance(ctx, "u1")
	if bal.Available != 200000-38000 {
		t.Fatalf("balance = %d, want %d (concurrent pays double-charged)", bal.Available, 200000-38000)
	}
	var pays int
	s.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM wallet_ledger WHERE kind = ? AND order_id = ?`, wallet.KindOrderPay, o.ID).Scan(&pays)
	if pays != 1 {
		t.Fatalf("%d order_pay rows, want 1", pays)
	}
}

func TestRefundOnExpiry(t *testing.T) {
	now := time.Date(2026, 10, 31, 10, 0, 0, 0, time.UTC)
	cur := now
	s := newStore(t, func() time.Time { return cur })
	w := wallet.New(s)
	ctx := context.Background()

	if _, err := w.Topup(ctx, "u1", 100000, "t", ""); err != nil {
		t.Fatalf("topup: %v", err)
	}
	o := newOrder(t, s, 50000, 10*time.Minute)

	// Pay, then sit on the order until it expires without delivery.
	if _, err := w.PayOrder(ctx, "u1", o.ID); err != nil {
		t.Fatalf("pay: %v", err)
	}
	bal, _ := w.Balance(ctx, "u1")
	if bal.Available != 52500 {
		t.Fatalf("after pay balance = %d, want 52500", bal.Available)
	}

	// A paid order is verified, not expired by the open-order sweeper; drive
	// the expiry path the way an admin/system would: close it as expired.
	cur = cur.Add(15 * time.Minute)
	if _, err := s.TransitionOrder(ctx, o.ID, types.StatusVerified, types.StatusReview, "system", "delivery window elapsed"); err != nil {
		t.Fatalf("to review: %v", err)
	}
	if _, err := s.TransitionOrder(ctx, o.ID, types.StatusReview, types.StatusExpired, "expiry", "unfulfilled"); err != nil {
		t.Fatalf("to expired: %v", err)
	}
	l, err := w.Refund(ctx, "", o.ID, "order expired")
	if err != nil {
		t.Fatalf("refund: %v", err)
	}
	if l == nil || l.AmountMinor != 47500 {
		t.Fatalf("refund ledger = %+v, want +47500", l)
	}
	bal, _ = w.Balance(ctx, "u1")
	if bal.Available != 100000 {
		t.Fatalf("balance after refund = %d, want 100000 (full prepaid amount back)", bal.Available)
	}

	// Replay the refund as many times as the sweeper likes: money comes back once.
	for i := 0; i < 3; i++ {
		if _, err := w.Refund(ctx, "", o.ID, "order expired"); err != nil {
			t.Fatalf("replay refund %d: %v", i, err)
		}
	}
	bal, _ = w.Balance(ctx, "u1")
	if bal.Available != 100000 {
		t.Fatalf("balance = %d after 3 replayed refunds, want 100000", bal.Available)
	}
}

func TestRefundExpiredUnpaidOrderIsNoop(t *testing.T) {
	now := time.Date(2026, 10, 31, 10, 0, 0, 0, time.UTC)
	cur := now
	s := newStore(t, func() time.Time { return cur })
	w := wallet.New(s)
	ctx := context.Background()

	o := newOrder(t, s, 50000, 5*time.Minute)
	cur = cur.Add(6 * time.Minute)

	expired, err := w.RefundExpired(ctx, s)
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if len(expired) != 1 || expired[0].ID != o.ID {
		t.Fatalf("swept %+v, want exactly %s", expired, o.ID)
	}
	bal, _ := w.Balance(ctx, "u1")
	if bal.Available != 0 {
		t.Fatalf("unpaid order refunded %d into a wallet", bal.Available)
	}
	if _, err := w.Refund(ctx, "", o.ID, "expired"); !errors.Is(err, wallet.ErrNotFound) {
		t.Fatalf("refund of unpaid order: want ErrNotFound, got %v", err)
	}
}

func TestCancelOrder(t *testing.T) {
	s := newStore(t, nil)
	w := wallet.New(s)
	ctx := context.Background()

	// An unpaid pending order cancels cleanly and moves no money.
	unpaid := newOrder(t, s, 60000, 30*time.Minute)
	got, err := w.CancelOrder(ctx, s, "u1", unpaid.ID, "customer", "changed my mind")
	if err != nil {
		t.Fatalf("cancel unpaid: %v", err)
	}
	if got.Status != types.StatusCancelled {
		t.Fatalf("status = %s, want cancelled", got.Status)
	}
	bal, _ := w.Balance(ctx, "u1")
	if bal.Available != 0 {
		t.Fatalf("cancelling an unpaid order produced %d", bal.Available)
	}

	// A paid (verified) order is not cancellable by the customer: that would
	// be a refund of a delivered purchase. The state machine refuses it.
	if _, err := w.Topup(ctx, "u1", 100000, "t", ""); err != nil {
		t.Fatalf("topup: %v", err)
	}
	paid := newOrder(t, s, 70000, 30*time.Minute)
	if _, err := w.PayOrder(ctx, "u1", paid.ID); err != nil {
		t.Fatalf("pay: %v", err)
	}
	if _, err := w.CancelOrder(ctx, s, "u1", paid.ID, "customer", "too late"); !errors.Is(err, store.ErrTransitionInvalid) {
		t.Fatalf("cancel verified order: want ErrTransitionInvalid, got %v", err)
	}
	bal, _ = w.Balance(ctx, "u1")
	// 100000 - (70000 - 3500) = 33500: no refund happened.
	if bal.Available != 33500 {
		t.Fatalf("balance = %d, want 33500 (verified order must not be refunded)", bal.Available)
	}
}

func TestCancelPaidOrderRefundsFromReview(t *testing.T) {
	s := newStore(t, nil)
	w := wallet.New(s)
	ctx := context.Background()
	if _, err := w.Topup(ctx, "u1", 100000, "t", ""); err != nil {
		t.Fatalf("topup: %v", err)
	}
	o := newOrder(t, s, 60000, 30*time.Minute)
	if _, err := w.PayOrder(ctx, "u1", o.ID); err != nil {
		t.Fatalf("pay: %v", err)
	}
	// Delivery failed: verified -> review -> cancelled, and the money returns.
	if _, err := s.TransitionOrder(ctx, o.ID, types.StatusVerified, types.StatusReview, "fulfiller", "out of stock"); err != nil {
		t.Fatalf("to review: %v", err)
	}
	got, err := w.CancelOrder(ctx, s, "u1", o.ID, "admin", "out of stock")
	if err != nil {
		t.Fatalf("cancel: %v", err)
	}
	if got.Status != types.StatusCancelled {
		t.Fatalf("status = %s, want cancelled", got.Status)
	}
	bal, _ := w.Balance(ctx, "u1")
	if bal.Available != 100000 {
		t.Fatalf("balance = %d, want 100000 after refund", bal.Available)
	}
}

func TestAdjustRequiresFundsForDebit(t *testing.T) {
	s := newStore(t, nil)
	w := wallet.New(s)
	ctx := context.Background()
	if _, err := w.Topup(ctx, "u1", 5000, "t", ""); err != nil {
		t.Fatalf("topup: %v", err)
	}
	if _, err := w.Adjust(ctx, "u1", -6000, "adj-1", "clawback"); !errors.Is(err, wallet.ErrInsufficientFunds) {
		t.Fatalf("over-debit: want ErrInsufficientFunds, got %v", err)
	}
	bal, err := w.Adjust(ctx, "u1", -2000, "adj-2", "clawback")
	if err != nil {
		t.Fatalf("debit: %v", err)
	}
	if bal.Available != 3000 {
		t.Fatalf("balance = %d, want 3000", bal.Available)
	}
}
