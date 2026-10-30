package provider

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestParseAmountTable(t *testing.T) {
	tests := []struct {
		in   string
		exp  int32
		want int64
		ok   bool
	}{
		{"1497", 2, 149700, true},
		{"1497.00", 2, 149700, true},
		{"Rs 1,497.00", 2, 149700, true},
		{"Rs. 1497", 2, 149700, true},
		{"PKR 1,497.5", 2, 149750, true},
		{"₨1,497", 2, 149700, true},
		{"1497.000", 2, 149700, true}, // trailing zeros beyond exponent are fine
		{"1497.005", 2, 0, false},     // real precision loss -> reject
		{"0.025", 6, 25000, true},     // USDT micro-units
		{"12.5", 6, 12500000, true},   // 12.5 USDT
		{"0.000001", 6, 1, true},      // one micro-USDT
		{"0.0000001", 6, 0, false},    // sub-micro precision -> reject
		{"", 2, 0, false},
		{"abc", 2, 0, false},
		{"1,4,97", 2, 149700, true}, // separators stripped wherever they appear
		{"-100", 2, -10000, true},   // signed parse, caller checks positivity
		{"9999999999999999999999", 2, 0, false},
	}
	for _, tc := range tests {
		got, err := ParseAmount(tc.in, tc.exp)
		if tc.ok {
			if err != nil {
				t.Fatalf("ParseAmount(%q,%d): unexpected error %v", tc.in, tc.exp, err)
			}
			if got != tc.want {
				t.Fatalf("ParseAmount(%q,%d) = %d, want %d", tc.in, tc.exp, got, tc.want)
			}
			continue
		}
		if err == nil {
			t.Fatalf("ParseAmount(%q,%d) = %d, want error", tc.in, tc.exp, got)
		}
		if !errors.Is(err, ErrMalformed) {
			t.Fatalf("ParseAmount(%q,%d) error class: %v", tc.in, tc.exp, err)
		}
	}
}

func TestParseAmountNeverRoundsThroughFloat(t *testing.T) {
	// The classic float trap: 0.1+0.2 style values lose precision. This table
	// pins exact micro-unit conversion for amounts a float64 would misround.
	for _, tc := range []struct {
		in   string
		want int64
	}{
		{"0.29", 290000},
		{"1.005", 1005000},
		{"12.345678", 12345678},
		{"0.07", 70000},
	} {
		got, err := ParseAmount(tc.in, 6)
		if err != nil {
			t.Fatalf("%q: %v", tc.in, err)
		}
		if got != tc.want {
			t.Fatalf("%q = %d, want %d", tc.in, got, tc.want)
		}
	}
}

func TestEqualHex(t *testing.T) {
	tests := []struct {
		name string
		got  string
		want string
		ok   bool
	}{
		{"exact", "deadBEEF", "deadbeef", true},
		{"0x prefix", "0xdeadbeef", "deadbeef", true},
		{"whitespace", " deadbeef ", "deadbeef", true},
		{"different", "deadbeef", "deadbeee", false},
		{"empty", "", "deadbeef", false},
		{"non-hex", "zz", "deadbeef", false},
		{"odd length", "abc", "abcd", false},
	}
	for _, tc := range tests {
		if got := EqualHex(tc.got, tc.want); got != tc.ok {
			t.Fatalf("%s: EqualHex(%q,%q) = %v, want %v", tc.name, tc.got, tc.want, got, tc.ok)
		}
	}
}

func TestHMACHelpersAgree(t *testing.T) {
	if got, want := HMACSHA256Hex("k", "a", "b"), HMACSHA256Hex("k", "ab"); got != want {
		t.Fatal("concatenation must be equivalent to joined parts")
	}
	if HMACSHA256Hex("k", "a", "b") == HMACSHA256Hex("k", "b", "a") {
		t.Fatal("part order must matter")
	}
	if HMACSHA256Base64("k", "x") == "" || HMACSHA256Hex("k", "x") == "" {
		t.Fatal("helpers returned empty digests")
	}
}

func TestHeaderBound(t *testing.T) {
	h := http.Header{}
	if _, err := Header(h, "X-Missing"); !errors.Is(err, ErrMissingSignature) {
		t.Fatalf("want ErrMissingSignature, got %v", err)
	}
	h.Set("X-Big", strings.Repeat("a", 5000))
	if _, err := Header(h, "X-Big"); !errors.Is(err, ErrMalformed) {
		t.Fatalf("oversize header must be rejected, got %v", err)
	}
	h.Set("X-Ok", "  v1  ")
	got, err := Header(h, "X-Ok")
	if err != nil || got != "v1" {
		t.Fatalf("Header returned %q, %v", got, err)
	}
}

func TestBodyBounds(t *testing.T) {
	r := httptest.NewRequest(http.MethodPost, "/x", strings.NewReader("abcdef"))
	if _, err := Body(r, 3); !errors.Is(err, ErrMalformed) {
		t.Fatalf("oversize body must be rejected, got %v", err)
	}
	r = httptest.NewRequest(http.MethodPost, "/x", strings.NewReader("abc"))
	b, err := Body(r, 3)
	if err != nil || string(b) != "abc" {
		t.Fatalf("Body = %q, %v", b, err)
	}
}

func TestParseTimeLocal(t *testing.T) {
	for _, in := range []string{
		"2026-10-31 12:00:00",
		"2026-10-31T12:00:00",
		"2026-10-31 12:00",
		"31/10/2026 12:00:00",
		"Sat, 31 Oct 2026 12:00:00 +0500",
		"2026-10-31T12:00:00+05:00",
	} {
		if _, err := ParseTimeLocal(in); err != nil {
			t.Fatalf("ParseTimeLocal(%q): %v", in, err)
		}
	}
	if _, err := ParseTimeLocal("yesterday"); err == nil {
		t.Fatal("garbage timestamp must be rejected")
	}
}

func TestParseUnixSeconds(t *testing.T) {
	if _, err := ParseUnixSeconds(0); err == nil {
		t.Fatal("zero must be rejected")
	}
	if _, err := ParseUnixSeconds(-1); err == nil {
		t.Fatal("negative must be rejected")
	}
	if _, err := ParseUnixSeconds(1761908400000); err == nil {
		t.Fatal("milliseconds must be rejected")
	}
	if _, err := ParseUnixSeconds(1761908400); err != nil {
		t.Fatalf("valid seconds rejected: %v", err)
	}
}

func TestExponent(t *testing.T) {
	if Exponent("USDT") != 6 || Exponent("usdt") != 6 {
		t.Fatal("USDT must be 6 decimals")
	}
	if Exponent("PKR") != 2 {
		t.Fatal("PKR must be 2 decimals")
	}
	if Exponent("XYZ") != 2 {
		t.Fatal("unknown currency must default to 2, not guess")
	}
}
