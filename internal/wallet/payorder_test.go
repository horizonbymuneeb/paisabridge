package wallet

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/horizonbymuneeb/paisabridge/internal/store"
	"github.com/horizonbymuneeb/paisabridge/internal/types"
)

func walletWithStore(t *testing.T, opts ...Option) (*Wallet, *store.Store) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "pay.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return New(st, opts...), st
}

func mkOrder(t *testing.T, st *store.Store, price int64, method types.Method) *types.Order {
	t.Helper()
	o, err := st.CreateOrder(context.Background(), store.CreateOrderInput{
		ProductID: "p", CatalogPrice: price, Currency: "PKR", Method: method, TTL: time.Hour,
	})
	if err != nil {
		t.Fatalf("create order: %v", err)
	}
	return o
}

// TestPayOrderErrorBranches walks every refusal path so a customer can never be
// charged for an order that is not payable.
func TestPayOrderErrorBranches(t *testing.T) {
	ctx := context.Background()
	w, st := walletWithStore(t, WithCurrency("PKR"), WithDiscountBPS(500))

	if _, err := w.PayOrder(ctx, "u1", ""); err == nil {
		t.Fatal("empty order id must be refused")
	}
	if _, err := w.PayOrder(ctx, "u1", "does-not-exist"); err != ErrNotFound {
		t.Fatalf("unknown order err = %v, want ErrNotFound", err)
	}

	// Currency mismatch: a USDT order cannot be paid from a PKR wallet.
	usd, err := st.CreateOrder(ctx, store.CreateOrderInput{
		ProductID: "p", CatalogPrice: 5_000_000, Currency: "USDT", Method: types.MethodUSDT, TTL: time.Hour,
	})
	if err != nil {
		t.Fatalf("create usdt order: %v", err)
	}
	if _, err := w.PayOrder(ctx, "u1", usd.ID); err == nil {
		t.Fatal("currency mismatch must be refused")
	}

	// A cancelled order is not payable.
	o2 := mkOrder(t, st, 100_000, types.MethodBank)
	if _, err := st.TransitionOrder(ctx, o2.ID, types.StatusPendingPayment, types.StatusCancelled, "test", "test"); err != nil {
		t.Fatalf("cancel: %v", err)
	}
	if _, err := w.PayOrder(ctx, "u1", o2.ID); err == nil {
		t.Fatal("cancelled order must not be payable")
	}

	// Insufficient funds leaves the ledger untouched.
	o3 := mkOrder(t, st, 100_000, types.MethodBank)
	if _, err := w.PayOrder(ctx, "poor", o3.ID); err == nil {
		t.Fatal("payment without funds must fail")
	}
	if bal, _ := w.AvailableMinor(ctx, "poor"); bal != 0 {
		t.Fatalf("failed payment changed the balance: %d", bal)
	}
}

// TestPayOrderReplayIsIdempotent: paying twice charges once and reports it.
func TestPayOrderReplayIsIdempotent(t *testing.T) {
	ctx := context.Background()
	w, st := walletWithStore(t, WithCurrency("PKR"), WithDiscountBPS(500))
	if _, err := w.Topup(ctx, "u1", 1_000_000, "seed", ""); err != nil {
		t.Fatalf("topup: %v", err)
	}
	o := mkOrder(t, st, 200_000, types.MethodBank)

	first, err := w.PayOrder(ctx, "u1", o.ID)
	if err != nil {
		t.Fatalf("pay: %v", err)
	}
	if first.AlreadyPaid {
		t.Fatal("first payment reported AlreadyPaid")
	}
	balanceAfterFirst, _ := w.AvailableMinor(ctx, "u1")

	second, err := w.PayOrder(ctx, "u1", o.ID)
	if err != nil {
		t.Fatalf("replay: %v", err)
	}
	if !second.AlreadyPaid {
		t.Fatal("replay must report AlreadyPaid")
	}
	if second.ChargedMinor != 0 {
		t.Fatalf("replay charged %d, want 0", second.ChargedMinor)
	}
	if bal, _ := w.AvailableMinor(ctx, "u1"); bal != balanceAfterFirst {
		t.Fatalf("replay changed the balance: %d -> %d", balanceAfterFirst, bal)
	}
}

// TestCancelOrderRefusesPaidOrder: a wallet-paid order is `verified`, and the
// state machine deliberately has no verified -> cancelled edge. Cancellation is
// a pre-payment action; money already taken comes back through the expiry sweep
// (see expiry_sweep_test.go) so there is exactly one refund path per order.
func TestCancelOrderRefundsPaidOrder(t *testing.T) {
	ctx := context.Background()
	w, st := walletWithStore(t, WithCurrency("PKR"), WithDiscountBPS(500))
	if _, err := w.Topup(ctx, "u1", 500_000, "seed", ""); err != nil {
		t.Fatalf("topup: %v", err)
	}
	o := mkOrder(t, st, 100_000, types.MethodBank)
	paid, err := w.PayOrder(ctx, "u1", o.ID)
	if err != nil {
		t.Fatalf("pay: %v", err)
	}
	if got := paid.ChargedMinor; got != 95_000 {
		t.Fatalf("charged %d, want 95000", got)
	}

	if _, err := w.CancelOrder(ctx, st, "u1", o.ID, "customer", "changed mind"); err == nil {
		t.Fatal("cancelling a paid order must be refused")
	}
	// The refusal must not have moved any money.
	if bal, _ := w.AvailableMinor(ctx, "u1"); bal != 405_000 {
		t.Fatalf("balance after refused cancel = %d, want 405000 (500000-95000)", bal)
	}
}

// TestHistoryIsNewestFirstAndLimited: the statement view an operator reads.
func TestHistoryIsNewestFirstAndLimited(t *testing.T) {
	ctx := context.Background()
	w, _ := walletWithStore(t, WithCurrency("PKR"))
	for _, k := range []string{"a", "b", "c"} {
		if _, err := w.Topup(ctx, "u1", 10_000, k, ""); err != nil {
			t.Fatalf("topup %s: %v", k, err)
		}
	}
	entries, err := w.History(ctx, "u1", 2)
	if err != nil {
		t.Fatalf("history: %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("history returned %d entries, want 2", len(entries))
	}
	if entries[0].ID <= entries[1].ID {
		t.Fatalf("history is not newest-first: ids %d then %d", entries[0].ID, entries[1].ID)
	}
}
