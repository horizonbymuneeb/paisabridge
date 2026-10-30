// Package provider holds small, well-tested helpers shared by every rail
// adapter. Keeping them here rather than in each adapter is what makes the
// "real adapter layer" claim checkable: there is exactly one implementation
// of "how do we compare signatures", "how do we parse rupees", "how do we
// read a bounded header", and the table-driven tests in provider_test.go
// cover it once.
package provider

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/horizonbymuneeb/paisabridge/internal/adapters/clock"
)

var (
	// ErrMissingSignature means the provider forwarded no signature at all.
	// Adapters must treat this exactly like a bad signature: an unsigned
	// callback is unauthenticated, not "legacy".
	ErrMissingSignature = errors.New("provider: signature header missing")

	// ErrBadSignature means a signature was present but did not verify.
	ErrBadSignature = errors.New("provider: signature mismatch")

	// ErrMalformed is returned when an authenticated payload cannot be
	// decoded (bad amount, missing reference, impossible timestamp).
	ErrMalformed = errors.New("provider: malformed payload")
)

// EqualHex compares two hex-encoded digests in constant time and, crucially,
// rejects non-hex input instead of silently comparing strings. A provider
// that sends "0x..." or upper-case hex is normalised; anything else is a
// mismatch, never an error the caller can paper over.
func EqualHex(got, want string) bool {
	g, err1 := decodeFlexibleHex(got)
	w, err2 := decodeFlexibleHex(want)
	if err1 != nil || err2 != nil {
		return false
	}
	return len(g) == len(w) && subtle.ConstantTimeCompare(g, w) == 1
}

func decodeFlexibleHex(s string) ([]byte, error) {
	s = strings.TrimSpace(s)
	s = strings.TrimPrefix(s, "0x")
	s = strings.TrimPrefix(s, "0X")
	if s == "" {
		return nil, errors.New("empty hex")
	}
	b, err := hex.DecodeString(strings.ToLower(s))
	if err != nil {
		return nil, err
	}
	return b, nil
}

// HMACSHA256Hex returns the lower-case hex HMAC of parts concatenated in
// order. Signed providers (JazzCash, Binance Pay, custom relays) differ only
// in what goes into the concatenation.
func HMACSHA256Hex(secret string, parts ...string) string {
	mac := hmac.New(sha256.New, []byte(secret))
	for _, p := range parts {
		io.WriteString(mac, p)
	}
	return hex.EncodeToString(mac.Sum(nil))
}

// HMACSHA256Raw returns the raw digest, for providers that base64-encode it.
func HMACSHA256Raw(secret string, parts ...string) []byte {
	mac := hmac.New(sha256.New, []byte(secret))
	for _, p := range parts {
		io.WriteString(mac, p)
	}
	return mac.Sum(nil)
}

// HMACSHA256Base64 is HMACSHA256Raw encoded with standard base64, the form
// most SDK-generated webhooks use.
func HMACSHA256Base64(secret string, parts ...string) string {
	return base64.StdEncoding.EncodeToString(HMACSHA256Raw(secret, parts...))
}

// Header returns a header value bounded by clock-independent limits. A
// provider that sends a 1 MiB "signature" is either broken or hostile; both
// cases must cost us one small allocation, not a parse.
func Header(h http.Header, name string) (string, error) {
	if h == nil {
		return "", ErrMissingSignature
	}
	v := h.Get(name)
	if v == "" {
		// http.Header.Get is case-insensitive already; the only way to get
		// here is an absent header.
		return "", ErrMissingSignature
	}
	if len(v) > 4096 {
		return "", fmt.Errorf("%w: %s is %d bytes", ErrMalformed, name, len(v))
	}
	return strings.TrimSpace(v), nil
}

// Body reads at most max bytes and fails loudly when the provider sent more,
// instead of silently truncating and verifying a prefix.
func Body(r *http.Request, max int64) ([]byte, error) {
	if r == nil || r.Body == nil {
		return nil, fmt.Errorf("%w: empty body", ErrMalformed)
	}
	if max <= 0 {
		max = 1 << 20
	}
	b, err := io.ReadAll(io.LimitReader(r.Body, max+1))
	if err != nil {
		return nil, err
	}
	if int64(len(b)) > max {
		return nil, fmt.Errorf("%w: body exceeds %d bytes", ErrMalformed, max)
	}
	return b, nil
}

// ParseAmount reads a decimal amount string ("1,499.00", "Rs 1497.5",
// "0.025000") into integer minor units with the given exponent, without
// going through float64. Banks and wallets are inconsistent: Easypaisa sends
// whole rupees, USDT needs six decimals, and a float round-trip would turn
// 0.025 USDT into 24999 micro-units on some inputs. Truncation beyond the
// exponent is an error, because silently dropping money is worse than
// rejecting the notice into manual review.
func ParseAmount(s string, exp int32) (int64, error) {
	raw := s
	s = strings.TrimSpace(s)
	// Strip currency markers and thousands separators commonly present in
	// SMS/e-mail text: "Rs 1497", "PKR 1,497.00", "₨1,497".
	for _, prefix := range []string{"Rs.", "Rs", "PKR", "₨", "$", "USDT", "usdt"} {
		if strings.HasPrefix(s, prefix) {
			s = strings.TrimSpace(strings.TrimPrefix(s, prefix))
		}
	}
	s = strings.ReplaceAll(s, ",", "")
	s = strings.ReplaceAll(s, "_", "")
	if s == "" {
		return 0, fmt.Errorf("%w: empty amount %q", ErrMalformed, raw)
	}
	sign := int64(1)
	switch s[0] {
	case '-':
		sign = -1
		s = s[1:]
	case '+':
		s = s[1:]
	}
	if s == "" {
		return 0, fmt.Errorf("%w: amount %q has no digits", ErrMalformed, raw)
	}
	intPart, fracPart, hasDot := strings.Cut(s, ".")
	if hasDot {
		fracPart = strings.TrimRight(fracPart, "0")
	}
	if intPart == "" && fracPart == "" {
		return 0, fmt.Errorf("%w: amount %q has no digits", ErrMalformed, raw)
	}
	if exp < 0 {
		return 0, fmt.Errorf("%w: negative exponent", ErrMalformed)
	}
	if int32(len(fracPart)) > exp {
		return 0, fmt.Errorf("%w: amount %q has %d decimals, currency allows %d",
			ErrMalformed, raw, len(fracPart), exp)
	}
	var units int64
	if intPart != "" {
		var err error
		units, err = strconv.ParseInt(intPart, 10, 64)
		if err != nil {
			return 0, fmt.Errorf("%w: integer part of %q", ErrMalformed, raw)
		}
	}
	for i := int32(0); i < exp; i++ {
		var d int64
		if int(i) < len(fracPart) {
			if fracPart[i] < '0' || fracPart[i] > '9' {
				return 0, fmt.Errorf("%w: amount %q is not numeric", ErrMalformed, raw)
			}
			d = int64(fracPart[i] - '0')
		}
		if units > (1<<62)/10 {
			return 0, fmt.Errorf("%w: amount %q overflows", ErrMalformed, raw)
		}
		units = units*10 + d
	}
	return sign * units, nil
}

// Exponent returns the minor-unit exponent for a currency. Unknown
// currencies default to 2, which keeps the failure mode "we rejected it"
// rather than "we stored a wrong amount".
func Exponent(currency string) int32 {
	switch strings.ToUpper(currency) {
	case "USDT", "USDC":
		return 6
	case "PKR", "USD", "AED", "SAR":
		return 2
	}
	return 2
}

// ParseTimeLocal parses a provider timestamp, assuming PKT when no zone is
// present. See package clock for why that assumption is safe.
func ParseTimeLocal(s string) (time.Time, error) {
	if t, err := clock.ParseLocal(s); err == nil {
		return t, nil
	}
	// RFC 1123 and friends: bank e-mail "Date:" headers.
	for _, layout := range []string{time.RFC1123Z, time.RFC1123, time.RFC3339, time.RFC3339Nano} {
		if t, err := time.Parse(layout, s); err == nil {
			return t, nil
		}
	}
	return time.Time{}, fmt.Errorf("%w: unsupported timestamp %q", ErrMalformed, s)
}

// ParseUnixSeconds converts Binance-style integer seconds with a range check
// so a milliseconds timestamp ("1761700000000") cannot masquerade as a 55,000
// year in the future and pass a freshness window.
func ParseUnixSeconds(n int64) (time.Time, error) {
	const maxSeconds = 4102444800 // 2100-01-01
	if n <= 0 || n > maxSeconds {
		return time.Time{}, fmt.Errorf("%w: unix seconds %d out of range", ErrMalformed, n)
	}
	return time.Unix(n, 0).In(clock.PKT), nil
}
