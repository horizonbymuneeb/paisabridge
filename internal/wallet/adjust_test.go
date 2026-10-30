package wallet

import (
	"context"
	"errors"
	"testing"
)

// TestAdjustValidatesInput covers the three refusals: a zero delta (a no-op
// recorded as a ledger row would be noise), a missing idempotency key (a retry
// would double-credit), and a negative delta that would overdraw.
func TestAdjustValidatesInput(t *testing.T) {
	ctx := context.Background()
	w, _ := walletWithStore(t, WithCurrency("PKR"))

	if _, err := w.Adjust(ctx, "u1", 0, "k", "noop"); err == nil {
		t.Fatal("zero delta must be refused")
	}
	if _, err := w.Adjust(ctx, "u1", 500, "   ", "nokey"); err == nil {
		t.Fatal("blank idempotency key must be refused")
	}
	if _, err := w.Adjust(ctx, "u1", -100, "k2", "overdraw"); !errors.Is(err, ErrInsufficientFunds) {
		t.Fatalf("overdraw err = %v, want ErrInsufficientFunds", err)
	}
	if bal, _ := w.AvailableMinor(ctx, "u1"); bal != 0 {
		t.Fatalf("a refused adjust changed the balance: %d", bal)
	}
}

// TestAdjustIsIdempotentAndBalances: a credit lands once, a replay is a no-op,
// and a later debit may draw it down.
func TestAdjustIsIdempotentAndBalances(t *testing.T) {
	ctx := context.Background()
	w, _ := walletWithStore(t, WithCurrency("PKR"))

	bal, err := w.Adjust(ctx, "u1", 25_000, "grant-1", "goodwill")
	if err != nil {
		t.Fatalf("credit: %v", err)
	}
	if bal.Available != 25_000 {
		t.Fatalf("balance after credit = %d, want 25000", bal.Available)
	}

	again, err := w.Adjust(ctx, "u1", 25_000, "grant-1", "goodwill")
	if err != nil {
		t.Fatalf("replay: %v", err)
	}
	if again.Available != 25_000 {
		t.Fatalf("replay double-credited: %d", again.Available)
	}

	if _, err := w.Adjust(ctx, "u1", -10_000, "debit-1", "clawback"); err != nil {
		t.Fatalf("debit: %v", err)
	}
	if bal, _ := w.AvailableMinor(ctx, "u1"); bal != 15_000 {
		t.Fatalf("balance after debit = %d, want 15000", bal)
	}
}
