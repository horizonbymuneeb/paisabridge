// Package unique computes the per-order unique payment amount.
//
// Every open order for a (method, currency) gets a distinct last-digit
// adjustment so an inbound payment can be matched by amount alone, without the
// payer ever typing a reference number.
package unique

import (
	"encoding/binary"
	"hash/fnv"
	"math"
)

// Amount is the minimum for a currency that the adjustments stay inside.
// PKR: amounts are whole rupees; USDT: six decimals.
const (
	CurrencyPKR  = "PKR"
	CurrencyUSDT = "USDT"
)

// AdjustmentMax is the largest offset in minor units we ever add or subtract.
var AdjustmentMax = map[string]int64{
	CurrencyPKR:  9,     //  0..9 paisa is invisible, so we work in rupees: 0..9 rupees
	CurrencyUSDT: 99999, // 0..0.0999 USDT
}

// Pick returns a unique amount for `price` given the amounts already taken.
//
// taken holds currently open amounts (minor units) for the same method and
// currency. The returned amount is inside a small window around price and is
// guaranteed not to collide with `taken`. If the window is exhausted the
// second return value is false and the caller should reject the order
// (a bounded window is a hard requirement: unbounded amounts would be an
// obvious fraud tell and impossible to match reliably).
func Pick(price int64, currency string, taken map[int64]bool) (int64, bool) {
	if price <= 0 {
		return 0, false
	}
	step := int64(1)
	switch currency {
	case CurrencyPKR:
		step = 100 // work in whole rupees
	case CurrencyUSDT:
		step = 1
	default:
		step = 1
	}
	max := AdjustmentMax[currency]
	if max <= 0 {
		max = 9 * step
	}

	// Skip zero: an amount equal to the list price carries no signal, and two
	// orders at the same price would both sit there until a collision forces
	// one of them away. Starting non-zero means every order carries a distinct
	// fingerprint from the first one, which is what the matching engine relies
	// on. The start is derived deterministically from the price so restarts
	// stay stable.
	h := fnv.New32a()
	h.Write([]byte(currency))
	var b [8]byte
	binary.LittleEndian.PutUint64(b[:], uint64(price))
	h.Write(b[:])
	base := int64(h.Sum32())%(max) + 1

	// Prefer adding small offsets, then subtracting.
	// Adding is tried first so the seller never collects less than the list
	// price when it can be avoided.
	for i := int64(0); i <= max; i++ {
		for _, sign := range []int64{1, -1} {
			off := ((base + i) % (max + 1)) * step
			cand := price + sign*off
			if off == 0 && sign < 0 {
				continue
			}
			if cand <= 0 || taken[cand] {
				continue
			}
			// Quantize: PKR must stay a whole rupee amount after adjustment.
			if currency == CurrencyPKR && cand%100 != 0 {
				continue
			}
			return cand, true
		}
	}
	return 0, false
}

// PercentOff returns the prepay discount amount for a wallet payment.
// basis points: 500 == 5%.
func PercentOff(amount int64, basisPoints int64) int64 {
	return int64(math.Floor(float64(amount) * float64(basisPoints) / 10000.0))
}
