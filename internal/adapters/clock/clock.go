// Package clock centralises time and timezone handling for the adapter layer.
//
// Adapters never call time.Now directly. Two reasons:
//
//   - tests must be deterministic;
//   - every payment rail reports timestamps differently. Binance Pay sends
//     UNIX seconds, banks send RFC 1123/mail dates, Payoneer sends naive
//     "2026-10-31 14:05:12" strings. All of them are reported in Punjab time
//     (Asia/Karachi, UTC+5, no DST since 2009) unless the payload says
//     otherwise, so without a fixed zone an interval such as
//     [01:30, 03:00] silently shifts by five hours.
//
// Asia/Karachi is exported as a time.FixedZone value instead of
// time.LoadLocation so the binary keeps working on hosts without the IANA
// tzdata package installed (Go stdlib only, no embed dependency).
package clock

import (
	"errors"
	"fmt"
	"time"
)

// PKT is Pakistan Standard Time, permanently UTC+5 (DST was abolished in 2009).
var PKT = time.FixedZone("PKT", 5*60*60)

// Clock returns the current instant. Live is the production implementation;
// tests use a fixed one so replay-window and expiry assertions are stable.
type Clock interface {
	Now() time.Time
}

// Live reads the system clock and normalises it to PKT so logs and windows
// are human-readable for the operators this gateway serves.
type Live struct{}

// Now implements Clock.
func (Live) Now() time.Time { return time.Now().In(PKT) }

// Fixed returns a Clock pinned to t. Passing the zero time disables expiry
// checks in every adapter that treats zero as "not configured".
func Fixed(t time.Time) Clock { return fixed(t) }

type fixed time.Time

func (f fixed) Now() time.Time { return time.Time(f) }

// ParseLocal interprets a timestamp string as PKT when it carries no zone
// designator. Supported layouts are tried in order, newest first.
func ParseLocal(s string) (time.Time, error) {
	for _, layout := range localLayouts {
		if t, err := time.ParseInLocation(layout, s, PKT); err == nil {
			return t, nil
		}
	}
	return time.Time{}, fmt.Errorf("clock: unsupported timestamp %q", s)
}

var localLayouts = []string{
	"2006-01-02 15:04:05",
	"2006-01-02T15:04:05",
	"2006-01-02 15:04",
	"02-01-2006 15:04:05",
	"02/01/2006 15:04:05",
	"02 Jan 2006 15:04:05",
	"02-Jan-2006 15:04:05",
	"2006-01-02",
}

// DateLayout is the calendar-date layout used by bank email alert parsing:
// "31/10/2026" (day-first, as used by Pakistani banks).
const DateLayout = "02/01/2006"

// ErrNonMonotonic is returned when a caller asks to advance the clock
// backwards, which would make replay windows unsound.
var ErrNonMonotonic = errors.New("clock: time went backwards")
