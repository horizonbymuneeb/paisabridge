package unique

import "testing"

// TestPickRejectsInvalidPrices: a non-positive price has no payable amount.
func TestPickRejectsInvalidPrices(t *testing.T) {
	for _, price := range []int64{0, -1, -149900} {
		if _, ok := Pick(price, CurrencyPKR, map[int64]bool{}); ok {
			t.Fatalf("price %d: expected refusal", price)
		}
	}
}

// TestPickUnknownCurrencyUsesUnitStep: an unlisted currency still produces a
// distinct amount; the picker only special-cases the currencies it knows.
func TestPickUnknownCurrencyUsesUnitStep(t *testing.T) {
	taken := map[int64]bool{}
	got := map[int64]bool{}
	for i := 0; i < 5; i++ {
		amt, ok := Pick(1000, "EUR", taken)
		if !ok {
			t.Fatalf("order %d: picker exhausted for an unknown currency", i)
		}
		if got[amt] {
			t.Fatalf("duplicate amount %d", amt)
		}
		got[amt] = true
		taken[amt] = true
	}
}

// TestPickUSDTUsesMicroUnits: the USDT band is expressed in micro-units and must
// never round to a whole USDT.
func TestPickUSDTUsesMicroUnits(t *testing.T) {
	const price = int64(5_000_000) // 5.000000 USDT
	amt, ok := Pick(price, CurrencyUSDT, map[int64]bool{})
	if !ok {
		t.Fatal("USDT pick refused")
	}
	if amt == price {
		t.Fatal("USDT pick used the bare price")
	}
	if d := amt - price; d > AdjustmentMax[CurrencyUSDT] || d < -AdjustmentMax[CurrencyUSDT] {
		t.Fatalf("USDT offset out of bounds: %d", d)
	}
}

// TestPickSkipsAmountsAtOrBelowZero: a subtraction that would cross zero is
// never returned, even when it is the only candidate left.
func TestPickSkipsAmountsAtOrBelowZero(t *testing.T) {
	// A price of one rupee: half the band is negative and must be skipped.
	for i := 0; i < 12; i++ {
		amt, ok := Pick(100, CurrencyPKR, map[int64]bool{})
		if ok && amt <= 0 {
			t.Fatalf("returned non-positive amount %d", amt)
		}
		_ = i
	}
}
