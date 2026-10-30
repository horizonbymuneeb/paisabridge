package wallet_test

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/horizonbymuneeb/paisabridge/internal/store"
	"github.com/horizonbymuneeb/paisabridge/internal/types"
	"github.com/horizonbymuneeb/paisabridge/internal/wallet"
)

// TestSweepRefundsVerifiedWalletOrder is the regression test for a
// money-stranding bug: paying from the wallet moves an order straight to
// `verified`, and the expiry sweep only looked at created/pending_payment. A
// paid order that was never delivered therefore stayed verified forever - the
// customer's balance was gone and the product was never handed over. The sweep
// must now expire it and the refund must return the exact charged amount.
func TestSweepRefundsVerifiedWalletOrder(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)

	st, err := store.Open(filepath.Join(t.TempDir(), "sweep.db"), store.WithClock(func() time.Time { return now }))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer st.Close()

	w := wallet.New(st, wallet.WithCurrency("PKR"), wallet.WithDiscountBPS(500))

	if _, err := w.Topup(ctx, "u1", 1_000_000, "seed", ""); err != nil {
		t.Fatalf("topup: %v", err)
	}

	o, err := st.CreateOrder(ctx, store.CreateOrderInput{
		ProductID:    "p1",
		CatalogPrice: 500_000,
		Currency:     "PKR",
		Method:       types.MethodEasypaisa,
		TTL:          time.Minute,
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	paid, err := w.PayOrder(ctx, "u1", o.ID)
	if err != nil {
		t.Fatalf("pay: %v", err)
	}
	if paid.ChargedMinor != 475_000 {
		t.Fatalf("charged %d, want 475000 (5%% off 500000)", paid.ChargedMinor)
	}
	if bal, _ := w.AvailableMinor(ctx, "u1"); bal != 525_000 {
		t.Fatalf("balance after pay = %d, want 525000", bal)
	}

	// The clock advances past the payment window with no delivery.
	now = now.Add(2 * time.Hour)

	expired, err := w.RefundExpired(ctx, st)
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if len(expired) != 1 {
		t.Fatalf("swept %d orders, want 1 (a verified, unpaid-out order must expire)", len(expired))
	}
	if expired[0].ID != o.ID {
		t.Fatalf("swept the wrong order: %s", expired[0].ID)
	}

	got, err := st.Order(ctx, o.ID)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if got.Status != types.StatusExpired {
		t.Fatalf("status = %s, want expired", got.Status)
	}
	if bal, _ := w.AvailableMinor(ctx, "u1"); bal != 1_000_000 {
		t.Fatalf("balance after refund = %d, want the original 1000000", bal)
	}

	// A second sweep must not move again or refund twice.
	again, err := w.RefundExpired(ctx, st)
	if err != nil {
		t.Fatalf("second sweep: %v", err)
	}
	if len(again) != 0 {
		t.Fatalf("second sweep expired %d more orders, want 0", len(again))
	}
	if bal, _ := w.AvailableMinor(ctx, "u1"); bal != 1_000_000 {
		t.Fatalf("balance after second sweep = %d, want 1000000", bal)
	}
}

// TestSweepLeavesOpenOrdersAlone: an order inside its window is untouched.
func TestSweepLeavesOpenOrdersAlone(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	st, err := store.Open(filepath.Join(t.TempDir(), "sweep2.db"), store.WithClock(func() time.Time { return now }))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer st.Close()
	w := wallet.New(st, wallet.WithCurrency("PKR"), wallet.WithDiscountBPS(500))

	o, err := st.CreateOrder(ctx, store.CreateOrderInput{
		ProductID: "p1", CatalogPrice: 100_000, Currency: "PKR",
		Method: types.MethodEasypaisa, TTL: time.Hour,
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	expired, err := w.RefundExpired(ctx, st)
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if len(expired) != 0 {
		t.Fatalf("swept %d fresh orders, want 0", len(expired))
	}
	if got, _ := st.Order(ctx, o.ID); got.Status == types.StatusExpired {
		t.Fatal("a fresh order must not expire")
	}
}
