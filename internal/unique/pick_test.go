package unique

import "testing"

// TestEveryOrderCarriesAnOffset pins the property the matcher depends on: the
// very first order at a price already differs from the list price, so an
// inbound payment can never be ambiguous with "the price I paid was the price
// on the site".
func TestEveryOrderCarriesAnOffset(t *testing.T) {
	taken := map[int64]bool{}
	for i := 0; i < 5; i++ {
		amt, ok := Pick(149900, CurrencyPKR, taken)
		if !ok {
			t.Fatalf("exhausted after %d orders", i)
		}
		if amt == 149900 {
			t.Fatalf("order %d used the bare list price (no fingerprint)", i)
		}
		if amt%100 != 0 {
			t.Fatalf("order %d is not a whole rupee amount: %d", i, amt)
		}
		if d := amt - 149900; d > 900 || d < -900 {
			t.Fatalf("order %d offset out of bounds: %d", i, d)
		}
		taken[amt] = true
	}
}

// TestPickerIsDeterministic: the same price with the same taken-set always
// yields the same amount, so a retried order create is not a lottery.
func TestPickerIsDeterministic(t *testing.T) {
	taken := map[int64]bool{150500: true}
	a, ok1 := Pick(149900, CurrencyPKR, taken)
	b, ok2 := Pick(149900, CurrencyPKR, taken)
	if !ok1 || !ok2 || a != b {
		t.Fatalf("non-deterministic: %d/%v vs %d/%v", a, ok1, b, ok2)
	}
}
