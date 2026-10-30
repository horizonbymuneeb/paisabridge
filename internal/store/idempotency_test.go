package store

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// openTestStore gives each test its own SQLite file (a real file, not
// :memory:, because the migration and reopen behaviour is part of what is
// being tested here).
func openTestStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "idem.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestIdempotencyClaimCompleteReplay(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	out, rec, err := s.ClaimIdempotencyKey(ctx, "user-1", "k1", "hash-a", time.Minute)
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if out != IdemClaimed {
		t.Fatalf("claim outcome = %v, want IdemClaimed", out)
	}
	if rec.State != IdemPending || rec.OrderID != "" {
		t.Fatalf("fresh record = %+v, want pending with no order", rec)
	}

	// A second claim while the first is pending is "in flight", not a claim.
	if _, _, err := s.ClaimIdempotencyKey(ctx, "user-1", "k1", "hash-a", time.Minute); !errors.Is(err, ErrIdemInFlight) {
		t.Fatalf("second claim err = %v, want ErrIdemInFlight", err)
	}

	ok, err := s.CompleteIdempotencyKey(ctx, "user-1", "k1", "hash-a", "ord_abc")
	if err != nil || !ok {
		t.Fatalf("complete: ok=%v err=%v", ok, err)
	}

	out, rec, err = s.ClaimIdempotencyKey(ctx, "user-1", "k1", "hash-a", time.Minute)
	if err != nil {
		t.Fatalf("replay claim: %v", err)
	}
	if out != IdemReplayed {
		t.Fatalf("replay outcome = %v, want IdemReplayed", out)
	}
	if rec.OrderID != "ord_abc" || rec.State != IdemDone {
		t.Fatalf("replay record = %+v, want order ord_abc done", rec)
	}
	// The replay carries the stored hash so the caller can detect a changed
	// body; it is not silently updated to the new request's hash.
	if rec.RequestHash != "hash-a" {
		t.Fatalf("replay hash = %q, want the stored hash-a", rec.RequestHash)
	}
	out, rec, err = s.ClaimIdempotencyKey(ctx, "user-1", "k1", "hash-B", time.Minute)
	if err != nil || out != IdemReplayed {
		t.Fatalf("replay with different hash: out=%v err=%v", out, err)
	}
	if rec.RequestHash != "hash-a" {
		t.Fatalf("stored hash overwritten by a different request: %q", rec.RequestHash)
	}
}

func TestIdempotencyScopesAreIsolated(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	if _, _, err := s.ClaimIdempotencyKey(ctx, "user-a", "same", "h", time.Minute); err != nil {
		t.Fatalf("claim a: %v", err)
	}
	out, _, err := s.ClaimIdempotencyKey(ctx, "user-b", "same", "h", time.Minute)
	if err != nil {
		t.Fatalf("claim b: %v", err)
	}
	if out != IdemClaimed {
		t.Fatalf("claim for a different scope = %v, want IdemClaimed", out)
	}
}

func TestIdempotencyAbortReleasesTheKey(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	if _, _, err := s.ClaimIdempotencyKey(ctx, "u", "k", "h", time.Minute); err != nil {
		t.Fatalf("claim: %v", err)
	}
	if err := s.AbortIdempotencyKey(ctx, "u", "k", "h"); err != nil {
		t.Fatalf("abort: %v", err)
	}
	out, _, err := s.ClaimIdempotencyKey(ctx, "u", "k", "h", time.Minute)
	if err != nil || out != IdemClaimed {
		t.Fatalf("claim after abort: out=%v err=%v, want claimed", out, err)
	}
}

func TestIdempotencyAbortCannotDeleteAnotherOwnersClaim(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	if _, _, err := s.ClaimIdempotencyKey(ctx, "u", "k", "mine", time.Minute); err != nil {
		t.Fatalf("claim: %v", err)
	}
	// A request with a different hash does not own the claim and must not be
	// able to release it: otherwise a client could clear another buyer's retry
	// guard by sending the wrong body.
	if err := s.AbortIdempotencyKey(ctx, "u", "k", "theirs"); err != nil {
		t.Fatalf("abort: %v", err)
	}
	if _, _, err := s.ClaimIdempotencyKey(ctx, "u", "k", "mine", time.Minute); !errors.Is(err, ErrIdemInFlight) {
		t.Fatalf("claim after foreign abort err = %v, want still in flight", err)
	}
}

func TestIdempotencyStaleClaimIsTakenOver(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	// Claim, then pretend the owner hung: the row is pending and old.
	if _, _, err := s.ClaimIdempotencyKey(ctx, "u", "k", "old-hash", time.Millisecond); err != nil {
		t.Fatalf("claim: %v", err)
	}
	time.Sleep(5 * time.Millisecond)

	out, _, err := s.ClaimIdempotencyKey(ctx, "u", "k", "new-hash", 2*time.Millisecond)
	if err != nil {
		t.Fatalf("takeover claim: %v", err)
	}
	if out != IdemClaimed {
		t.Fatalf("takeover outcome = %v, want IdemClaimed", out)
	}
	// The original owner can no longer complete: its hash is gone.
	if ok, err := s.CompleteIdempotencyKey(ctx, "u", "k", "old-hash", "ord_ghost"); err != nil || ok {
		t.Fatalf("stale owner incomplete: ok=%v err=%v, want ok=false", ok, err)
	}
	if ok, err := s.CompleteIdempotencyKey(ctx, "u", "k", "new-hash", "ord_new"); err != nil || !ok {
		t.Fatalf("new owner complete: ok=%v err=%v", ok, err)
	}
}

func TestIdempotencyPruneDropsOldRows(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	if _, _, err := s.ClaimIdempotencyKey(ctx, "u", "old", "h", time.Minute); err != nil {
		t.Fatalf("claim old: %v", err)
	}
	if _, _, err := s.ClaimIdempotencyKey(ctx, "u", "new", "h", time.Minute); err != nil {
		t.Fatalf("claim new: %v", err)
	}
	// Everything is "created in the past" from the pruner's point of view.
	n, err := s.PruneIdempotencyKeys(s.Now().Add(time.Second))
	if err != nil {
		t.Fatalf("prune: %v", err)
	}
	if n != 2 {
		t.Fatalf("pruned %d rows, want 2", n)
	}
	if _, found, err := s.IdempotencyRecord(ctx, "u", "old"); err != nil || found {
		t.Fatalf("old record after prune: found=%v err=%v", found, err)
	}
	// A pruned key can be used again, which is the point of pruning.
	if out, _, err := s.ClaimIdempotencyKey(ctx, "u", "old", "h2", time.Minute); err != nil || out != IdemClaimed {
		t.Fatalf("reclaim after prune: out=%v err=%v", out, err)
	}
}

func TestIdempotencyConcurrentClaimsHaveOneWinner(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	const n = 16
	var wg sync.WaitGroup
	var mu sync.Mutex
	claimed := 0
	inflight := 0
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			out, _, err := s.ClaimIdempotencyKey(ctx, "u", "race", "h", time.Minute)
			mu.Lock()
			defer mu.Unlock()
			switch {
			case errors.Is(err, ErrIdemInFlight):
				inflight++
			case err != nil:
				t.Errorf("claim error: %v", err)
			case out == IdemClaimed:
				claimed++
			}
		}()
	}
	wg.Wait()
	if claimed != 1 {
		t.Fatalf("%d concurrent claims won, want exactly 1 (in flight: %d)", claimed, inflight)
	}
	if inflight != n-1 {
		t.Fatalf("in-flight answers = %d, want %d", inflight, n-1)
	}
}

func TestIdempotencyRejectsEmptyScopeOrKey(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	if _, _, err := s.ClaimIdempotencyKey(ctx, "", "k", "h", time.Minute); err == nil {
		t.Fatal("empty scope accepted")
	}
	if _, _, err := s.ClaimIdempotencyKey(ctx, "u", "", "h", time.Minute); err == nil {
		t.Fatal("empty key accepted")
	}
}
