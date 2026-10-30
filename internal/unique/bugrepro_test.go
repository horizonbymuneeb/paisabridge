package unique

import "testing"

// TestFullBandNeverReturnsBarePrice is the regression test for the reported
// bug: with every offset in the band already taken, the picker used an iterator
// that skipped zero only on the negative branch, so it returned the bare list
// price - an amount with no fingerprint that collides with every other order at
// that price. Either a real offset or a clean refusal is acceptable; the bare
// price is not.
func TestFullBandNeverReturnsBarePrice(t *testing.T) {
	for _, price := range []int64{250000, 400000, 860000} {
		taken := map[int64]bool{}
		for i := int64(1); i <= 9; i++ {
			taken[price+i*100] = true
			taken[price-i*100] = true
		}
		amt, ok := Pick(price, CurrencyPKR, taken)
		if ok && amt == price {
			t.Fatalf("price %d: returned the bare list price (offset 0)", price)
		}
		if ok && taken[amt] {
			t.Fatalf("price %d: returned an already-taken amount %d", price, amt)
		}
	}
}
