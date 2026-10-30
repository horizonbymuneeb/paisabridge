package store

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

func newStoreFor(t *testing.T) *Store {
	t.Helper()
	st, err := Open(filepath.Join(t.TempDir(), "s.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

// TestIdempotencyRecordLifecycle covers the diagnostics lookups that the
// sweeper and the admin views depend on: a record is readable while pending,
// complete once finished, and prune only removes what is past the cutoff.
func TestIdempotencyRecordLifecycle(t *testing.T) {
	ctx := context.Background()
	st := newStoreFor(t)

	if _, ok, err := st.IdempotencyRecord(ctx, "orders", "missing"); err != nil || ok {
		t.Fatalf("missing record: ok=%v err=%v", ok, err)
	}

	outcome, _, err := st.ClaimIdempotencyKey(ctx, "orders", "k1", "hash-a", time.Hour)
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if outcome != IdemClaimed {
		t.Fatalf("first claim outcome = %v, want claimed", outcome)
	}
	if _, ok, err := st.IdempotencyRecord(ctx, "orders", "k1"); err != nil || !ok {
		t.Fatalf("pending record not readable: ok=%v err=%v", ok, err)
	}

	if ok, err := st.CompleteIdempotencyKey(ctx, "orders", "k1", "hash-a", "ord_1"); err != nil || !ok {
		t.Fatalf("complete: ok=%v err=%v", ok, err)
	}
	rec, ok, err := st.IdempotencyRecord(ctx, "orders", "k1")
	if err != nil || !ok {
		t.Fatalf("completed record missing: ok=%v err=%v", ok, err)
	}
	if rec.OrderID != "ord_1" {
		t.Fatalf("record order = %q, want ord_1", rec.OrderID)
	}

	// A finished record is not overwritable: the documented contract is
	// (false, nil) - "the claim is no longer ours" - not an error, because the
	// caller's correct response is to read the existing record.
	if ok, err := st.CompleteIdempotencyKey(ctx, "orders", "k1", "other-hash", "ord_2"); ok || err != nil {
		t.Fatalf("complete with a different hash = (ok=%v, err=%v), want (false, nil)", ok, err)
	}
	// And the stored order id must still be the first one.
	if rec, _, _ := st.IdempotencyRecord(ctx, "orders", "k1"); rec.OrderID != "ord_1" {
		t.Fatalf("record overwritten to %q", rec.OrderID)
	}

	n, err := st.PruneIdempotencyKeys(time.Now().Add(time.Hour))
	if err != nil {
		t.Fatalf("prune: %v", err)
	}
	if n != 1 {
		t.Fatalf("prune removed %d, want 1", n)
	}
	if _, ok, err := st.IdempotencyRecord(ctx, "orders", "k1"); err != nil || ok {
		t.Fatalf("record survived the prune: ok=%v err=%v", ok, err)
	}
}

// TestAbortIdempotencyKeyFreesTheKey: a failed create must not wedge the key, or
// the customer can never retry the same checkout.
func TestAbortIdempotencyKeyFreesTheKey(t *testing.T) {
	ctx := context.Background()
	st := newStoreFor(t)
	if _, _, err := st.ClaimIdempotencyKey(ctx, "orders", "k2", "h", time.Hour); err != nil {
		t.Fatalf("claim: %v", err)
	}
	if err := st.AbortIdempotencyKey(ctx, "orders", "k2", "h"); err != nil {
		t.Fatalf("abort: %v", err)
	}
	outcome, _, err := st.ClaimIdempotencyKey(ctx, "orders", "k2", "h", time.Hour)
	if err != nil {
		t.Fatalf("re-claim: %v", err)
	}
	if outcome != IdemClaimed {
		t.Fatalf("re-claim outcome = %v, want claimed", outcome)
	}
}

// TestDeadLetterCountsAndOpen covers the gauge an operator alerts on.
func TestDeadLetterCountsAndOpen(t *testing.T) {
	ctx := context.Background()
	st := newStoreFor(t)

	open, total, err := st.DeadLetterCounts(ctx)
	if err != nil {
		t.Fatalf("counts: %v", err)
	}
	if open != 0 || total != 0 {
		t.Fatalf("empty store counts = %d/%d, want 0/0", open, total)
	}
	if n, err := st.CountOpenDeadLetters(ctx); err != nil || n != 0 {
		t.Fatalf("count open = %d/%v, want 0", n, err)
	}
}

// TestClosedStoreReturnsErrorNotPanic: every maintenance entry point must fail
// cleanly on a closed handle instead of panicking a sweeper goroutine.
func TestClosedStoreReturnsErrorNotPanic(t *testing.T) {
	ctx := context.Background()
	st := newStoreFor(t)
	if err := st.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	if _, err := st.CountOpenDeadLetters(ctx); err == nil {
		t.Error("CountOpenDeadLetters on a closed store should error")
	}
	if _, _, err := st.DeadLetterCounts(ctx); err == nil {
		t.Error("DeadLetterCounts on a closed store should error")
	}
	if _, _, err := st.IdempotencyRecord(ctx, "s", "k"); err == nil {
		t.Error("IdempotencyRecord on a closed store should error")
	}
	if _, err := st.PruneIdempotencyKeys(time.Now()); err == nil {
		t.Error("PruneIdempotencyKeys on a closed store should error")
	}
	if _, err := st.PruneReplayGuard(time.Now()); err == nil {
		t.Error("PruneReplayGuard on a closed store should error")
	}
}
