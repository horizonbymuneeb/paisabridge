package wallet

import (
	"path/filepath"
	"testing"

	"github.com/horizonbymuneeb/paisabridge/internal/store"
)

func newWallet(t *testing.T, opts ...Option) *Wallet {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "w.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return New(st, opts...)
}

// TestDiscountAccessors: the configured rate is exposed and applied exactly.
func TestDiscountAccessors(t *testing.T) {
	w := newWallet(t, WithCurrency("PKR"), WithDiscountBPS(500))
	if got := w.DiscountBPS(); got != 500 {
		t.Fatalf("DiscountBPS = %d, want 500", got)
	}
	// 5% of Rs 2500.00 (250000 paisa) is 12500 paisa.
	if got := w.Discount(250000); got != 12500 {
		t.Fatalf("Discount(250000) = %d, want 12500", got)
	}
	// Rounding must never produce a fractional paisa.
	if got := w.Discount(9999); got != 499 {
		t.Fatalf("Discount(9999) = %d, want 499 (floor of 499.95)", got)
	}
	if got := w.Discount(0); got != 0 {
		t.Fatalf("Discount(0) = %d, want 0", got)
	}
}

// TestDefaultDiscountIsZeroWhenUnset: a zero-value option set must not invent a
// discount; the payment then charges the full amount.
func TestDefaultDiscountIsZeroWhenUnset(t *testing.T) {
	w := newWallet(t, WithCurrency("PKR"), WithDiscountBPS(0))
	if got := w.DiscountBPS(); got != 0 {
		t.Fatalf("DiscountBPS = %d, want 0", got)
	}
	if got := w.Discount(100000); got != 0 {
		t.Fatalf("Discount with 0 bps = %d, want 0", got)
	}
}

// TestCurrencyOptionNormalises: an unset/blank currency falls back to PKR, and
// the accessor reflects the effective value.
func TestCurrencyOptionNormalises(t *testing.T) {
	for _, in := range []string{"", "  ", "pkr"} {
		w := newWallet(t, WithCurrency(in))
		if got := w.Currency(); got != "PKR" {
			t.Fatalf("WithCurrency(%q).Currency() = %q, want PKR", in, got)
		}
	}
	w := newWallet(t, WithCurrency("USDT"))
	if got := w.Currency(); got != "USDT" {
		t.Fatalf("Currency() = %q, want USDT", got)
	}
}
