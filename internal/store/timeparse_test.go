package store

import (
	"context"
	"testing"
	"time"
)

// TestParseTimeAcceptsRealWorldShapes: rows are not always written by this
// package. A migration, a hand-run UPDATE or an import carries fewer fractional
// digits, and a strict parse used to turn those into the zero time - which made
// an order look a thousand years overdue.
func TestParseTimeAcceptsRealWorldShapes(t *testing.T) {
	want := time.Date(2026, 10, 30, 22, 31, 6, 0, time.UTC)
	cases := []string{
		"2026-10-30T22:31:06.000000000Z",
		"2026-10-30T22:31:06.032206Z",
		"2026-10-30T22:31:06.032206123Z",
		"2026-10-30T22:31:06Z",
		"2026-10-30 22:31:06",
	}
	for _, in := range cases {
		got, err := parseTime(in)
		if err != nil {
			t.Fatalf("parseTime(%q): %v", in, err)
		}
		if got.IsZero() {
			t.Fatalf("parseTime(%q) returned the zero time", in)
		}
		if !got.Truncate(time.Second).Equal(want) {
			t.Fatalf("parseTime(%q) = %s, want %s", in, got, want)
		}
	}
}

// TestParseTimeEmptyIsZero: an unwritten column is ” and means "not set".
func TestParseTimeEmptyIsZero(t *testing.T) {
	got, err := parseTime("")
	if err != nil || !got.IsZero() {
		t.Fatalf("parseTime(\"\") = %s, %v", got, err)
	}
}

// TestParseTimeRejectsGarbage: a malformed timestamp must be an error, not a
// silent zero that money logic would treat as "expired long ago".
func TestParseTimeRejectsGarbage(t *testing.T) {
	for _, in := range []string{"not-a-date", "2026-13-45T99:99:99Z", "1700000000"} {
		if _, err := parseTime(in); err == nil {
			t.Fatalf("parseTime(%q) accepted garbage", in)
		}
	}
}

// TestListOrdersParsesSixDigitTimestamps is the end-to-end regression: an order
// row written with microsecond precision must come back with a real UpdatedAt.
func TestListOrdersParsesSixDigitTimestamps(t *testing.T) {
	st := newStoreFor(t)
	ctx := context.Background()

	if _, err := st.db.ExecContext(ctx, `INSERT INTO orders
		(id, ref, product_id, product_name, catalog_price, amount, currency, method, status,
		 user_id, customer_email, customer_phone, tx_ref, sms_otp, expires_at, created_at, updated_at)
		VALUES ('ord_x','ref_x','p','P',100000,100000,'PKR','bank','review','','','','','',
		'2026-10-30T23:00:00.000000Z','2026-10-30T22:30:58.402624Z','2026-10-30T22:31:06.032206Z')`); err != nil {
		t.Fatalf("insert: %v", err)
	}
	orders, err := st.ListOrders(ctx, "", 10)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(orders) != 1 {
		t.Fatalf("got %d orders", len(orders))
	}
	if orders[0].UpdatedAt.IsZero() {
		t.Fatal("UpdatedAt parsed as zero from a microsecond-precision row")
	}
	if got := orders[0].UpdatedAt.UTC().Format(time.RFC3339); got != "2026-10-30T22:31:06Z" {
		t.Fatalf("UpdatedAt = %s", got)
	}
}
