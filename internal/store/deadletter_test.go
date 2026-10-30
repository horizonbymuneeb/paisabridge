package store_test

// Dead-letter storage tests. Migration 0004 is exercised for real here (the
// tests open a file database, so the embedded migrations run exactly as they do
// in production).

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/horizonbymuneeb/paisabridge/internal/store"
)

func TestDeadLetterRoundTrip(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()

	body := []byte(`{"event":"order.fulfilled","delivery_id":"d-1","order":{"id":"ord_1"},"attempt":3}`)
	dl := &store.DeadLetter{
		DeliveryID: "d-1",
		Event:      "order.fulfilled",
		OrderID:    "ord_1",
		LastError:  "outbound: status 500",
		Attempts:   3,
		Body:       body,
	}
	if err := s.PutDeadLetter(ctx, dl); err != nil {
		t.Fatalf("put: %v", err)
	}
	if dl.ID == "" {
		t.Fatal("store must assign the row id")
	}

	got, err := s.DeadLetter(ctx, dl.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if string(got.Body) != string(body) {
		t.Fatalf("body must round-trip byte for byte: got %q", got.Body)
	}
	if got.DeliveryID != "d-1" || got.Event != "order.fulfilled" || got.OrderID != "ord_1" {
		t.Fatalf("fields: %+v", got)
	}
	if got.Attempts != 3 {
		t.Fatalf("attempts: %d", got.Attempts)
	}
	if got.Resolved || !got.ResolvedAt.IsZero() {
		t.Fatalf("fresh row must be open: %+v", got)
	}
	if got.LastError != "outbound: status 500" {
		t.Fatalf("last error: %q", got.LastError)
	}
}

// TestDeadLetterPutIsIdempotentPerDelivery is the invariant that keeps an
// operator from retrying the same delivery twice: one delivery, one row.
func TestDeadLetterPutIsIdempotentPerDelivery(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()

	for _, e := range []string{"status 500", "status 502"} {
		if err := s.PutDeadLetter(ctx, &store.DeadLetter{
			DeliveryID: "same", Event: "order.fulfilled", OrderID: "ord_1",
			LastError: e, Attempts: 5, Body: []byte(`{"attempt":1}`),
		}); err != nil {
			t.Fatalf("put: %v", err)
		}
	}
	rows, err := s.DeadLetters(ctx, "all", "", 10)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("want exactly one row for one delivery, got %d", len(rows))
	}
	if rows[0].LastError != "status 502" {
		t.Fatalf("the newest error must win, got %q", rows[0].LastError)
	}
}

func TestDeadLetterPutRejectsIncompleteRows(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	cases := map[string]*store.DeadLetter{
		"no delivery id": {Event: "order.fulfilled"},
		"no event":       {DeliveryID: "d"},
		"nil":            nil,
	}
	for name, dl := range cases {
		if err := s.PutDeadLetter(ctx, dl); err == nil {
			t.Fatalf("%s: want an error", name)
		}
	}
}

// TestDeadLetterEmptyBodyIsPlaceholder: a malformed delivery still has to leave
// a trace. Losing the failure because the body was empty would be worse.
func TestDeadLetterEmptyBodyIsPlaceholder(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	if err := s.PutDeadLetter(ctx, &store.DeadLetter{DeliveryID: "empty", Event: "order.expired"}); err != nil {
		t.Fatalf("put: %v", err)
	}
	rows, _ := s.DeadLetters(ctx, "", "", 10)
	if len(rows) != 1 || string(rows[0].Body) != "{}" {
		t.Fatalf("want one row with the {} placeholder, got %+v", rows)
	}
}

func TestDeadLetterListFilters(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()

	put := func(delivery, order string) string {
		dl := &store.DeadLetter{DeliveryID: delivery, Event: "order.fulfilled", OrderID: order, Body: []byte(`{"x":1}`)}
		if err := s.PutDeadLetter(ctx, dl); err != nil {
			t.Fatalf("put: %v", err)
		}
		return dl.ID
	}
	first := put("d-1", "ord_1")
	put("d-2", "ord_2")
	put("d-3", "ord_1")

	if err := s.FinishDeadLetter(ctx, first, true, "", time.Now()); err != nil {
		t.Fatalf("finish: %v", err)
	}

	open, err := s.DeadLetters(ctx, "open", "", 10)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if len(open) != 2 {
		t.Fatalf("want 2 open rows, got %d", len(open))
	}
	done, _ := s.DeadLetters(ctx, "done", "", 10)
	if len(done) != 1 || done[0].ID != first {
		t.Fatalf("want the finished row only, got %+v", done)
	}
	all, _ := s.DeadLetters(ctx, "all", "", 10)
	if len(all) != 3 {
		t.Fatalf("want 3 rows, got %d", len(all))
	}
	byOrder, _ := s.DeadLetters(ctx, "all", "ord_1", 10)
	if len(byOrder) != 2 {
		t.Fatalf("order filter: want 2, got %d", len(byOrder))
	}
	if _, err := s.DeadLetters(ctx, "bogus", "", 10); err == nil {
		t.Fatal("an unknown status must be an error, not a silent empty list")
	}

	openN, total, err := s.DeadLetterCounts(ctx)
	if err != nil {
		t.Fatalf("counts: %v", err)
	}
	if openN != 2 || total != 3 {
		t.Fatalf("counts: want open=2 total=3, got open=%d total=%d", openN, total)
	}
}

// TestDeadLetterFinishKeepsHistory: after a successful retry the row is resolved
// but keeps the reason it existed and the original body.
func TestDeadLetterFinishKeepsHistory(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()

	dl := &store.DeadLetter{
		DeliveryID: "d-9", Event: "order.fulfilled", OrderID: "ord_9",
		LastError: "outbound: status 503", Attempts: 5, Body: []byte(`{"x":1}`),
	}
	if err := s.PutDeadLetter(ctx, dl); err != nil {
		t.Fatalf("put: %v", err)
	}
	at := time.Date(2026, 10, 31, 3, 0, 0, 0, time.UTC)
	if err := s.FinishDeadLetter(ctx, dl.ID, true, "", at); err != nil {
		t.Fatalf("finish: %v", err)
	}
	got, err := s.DeadLetter(ctx, dl.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if !got.Resolved {
		t.Fatal("row must be resolved")
	}
	if !got.ResolvedAt.Equal(at) {
		t.Fatalf("resolved_at: want %v, got %v", at, got.ResolvedAt)
	}
	if got.Attempts != 6 {
		t.Fatalf("a successful retry counts as an attempt: want 6, got %d", got.Attempts)
	}
	if got.LastError != "outbound: status 503" {
		t.Fatalf("the original error is history, not noise: %q", got.LastError)
	}
	if !got.RetriedAt.Equal(at) {
		t.Fatalf("retried_at: %v", got.RetriedAt)
	}
}

// TestDeadLetterFinishFailureReopens: a failed retry keeps the row open with the
// newest error, which is what the operator acts on next.
func TestDeadLetterFinishFailureReopens(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	dl := &store.DeadLetter{DeliveryID: "d-f", Event: "order.expired", Body: []byte(`{"x":1}`), LastError: "first"}
	if err := s.PutDeadLetter(ctx, dl); err != nil {
		t.Fatalf("put: %v", err)
	}
	if err := s.FinishDeadLetter(ctx, dl.ID, false, "second: connection refused", time.Now()); err != nil {
		t.Fatalf("finish: %v", err)
	}
	got, _ := s.DeadLetter(ctx, dl.ID)
	if got.Resolved {
		t.Fatal("a failed retry must leave the row open")
	}
	if got.LastError != "second: connection refused" {
		t.Fatalf("last error should advance: %q", got.LastError)
	}
	if got.Attempts != 1 {
		t.Fatalf("attempts: %d", got.Attempts)
	}
	// Finishing a row that does not exist is the 404 path, not a silent no-op.
	if err := s.FinishDeadLetter(ctx, "dl_missing", true, "", time.Now()); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
	if _, err := s.DeadLetter(ctx, "dl_missing"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
	if _, err := s.DeadLetter(ctx, ""); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("empty id: want ErrNotFound, got %v", err)
	}
}
