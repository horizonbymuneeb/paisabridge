package unique

import "testing"

func TestPickNoCollision(t *testing.T) {
	taken := map[int64]bool{}
	for i := 0; i < 10; i++ {
		amt, ok := Pick(149900, CurrencyPKR, taken)
		if !ok {
			t.Fatalf("picker exhausted after %d orders", i)
		}
		if taken[amt] {
			t.Fatalf("collision: %d", amt)
		}
		if amt%100 != 0 {
			t.Fatalf("PKR amount not a whole rupee: %d", amt)
		}
		if d := amt - 149900; d > 900 || d < -900 {
			t.Fatalf("offset too large: %d paisa", d)
		}
		taken[amt] = true
	}
}

func TestPickExhausted(t *testing.T) {
	taken := map[int64]bool{}
	// PKR window is price..price+9 and price-1..price-9 -> 19 values, but
	// whole-rupee quantization leaves 10 positive + 9 negative = 19 slots.
	n := 0
	for {
		amt, ok := Pick(5000, CurrencyPKR, taken)
		if !ok {
			break
		}
		taken[amt] = true
		n++
		if n > 100 {
			t.Fatal("picker never exhausts")
		}
	}
	if n == 0 {
		t.Fatal("no amount pickable")
	}
}

func TestPercentOff(t *testing.T) {
	if got := PercentOff(149900, 500); got != 7495 {
		t.Fatalf("want 7495 paisa off, got %d", got)
	}
}
