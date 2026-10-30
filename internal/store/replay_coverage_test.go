package store_test

// Coverage for the webhook replay guard's less-travelled paths: the empty-id
// rejection, the method/timestamp recording, and pruning by cutoff.

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/horizonbymuneeb/paisabridge/internal/store"
)

func TestRememberForRejectsEmptyID(t *testing.T) {
	s := newStore(t)
	// An empty envelope id carries no idempotency signal, so it must never be
	// claimed — and never occupy a row that would block a real id later.
	if s.RememberFor("", "easypaisa", time.Now()) {
		t.Fatal("empty envelope id must not be claimable")
	}
	if s.Remember("") {
		t.Fatal("empty envelope id must not be claimable via Remember")
	}
	if s.Seen("") {
		t.Fatal("empty envelope id must not be reported as seen")
	}
}

func TestRememberForRecordsMethodAndTimestamp(t *testing.T) {
	s := newStore(t)
	at := time.Date(2026, 3, 4, 5, 6, 7, 0, time.UTC)
	if !s.RememberFor("env-1", "jazzcash", at) {
		t.Fatal("first claim must win")
	}
	if s.RememberFor("env-1", "jazzcash", at) {
		t.Fatal("second claim of the same id must lose")
	}
	if !s.Seen("env-1") {
		t.Fatal("a claimed id must be reported as seen")
	}

	var method, firstSeen string
	if err := s.DB().QueryRow(`SELECT method, first_seen FROM webhook_replay WHERE envelope_id = ?`, "env-1").
		Scan(&method, &firstSeen); err != nil {
		t.Fatalf("read replay row: %v", err)
	}
	if method != "jazzcash" {
		t.Fatalf("method not recorded, got %q", method)
	}
	// Remember must default the rail to empty rather than inventing one.
	if !s.Remember("env-2") {
		t.Fatal("Remember must claim a fresh id")
	}
	if err := s.DB().QueryRow(`SELECT method FROM webhook_replay WHERE envelope_id = ?`, "env-2").Scan(&method); err != nil {
		t.Fatalf("read replay row: %v", err)
	}
	if method != "" {
		t.Fatalf("Remember must not attribute a rail, got %q", method)
	}
}

func TestPruneReplayGuardHonoursCutoff(t *testing.T) {
	s := newStore(t)
	base := time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC)
	for i, at := range []time.Time{
		base.Add(-48 * time.Hour), // old
		base.Add(-25 * time.Hour), // old
		base.Add(-1 * time.Hour),  // inside the window
		base.Add(1 * time.Hour),   // future-dated: must survive
	} {
		if !s.RememberFor("env-"+itoa(int64(i)), "easypaisa", at) {
			t.Fatalf("claim %d failed", i)
		}
	}

	n, err := s.PruneReplayGuard(base.Add(-24 * time.Hour))
	if err != nil {
		t.Fatalf("prune: %v", err)
	}
	if n != 2 {
		t.Fatalf("prune: want 2 rows removed, got %d", n)
	}
	for i, wantAlive := range []bool{false, false, true, true} {
		id := "env-" + itoa(int64(i))
		if got := s.Seen(id); got != wantAlive {
			t.Fatalf("%s: seen = %v, want %v", id, got, wantAlive)
		}
	}
	// Pruning again with the same cutoff removes nothing.
	if n, err := s.PruneReplayGuard(base.Add(-24 * time.Hour)); err != nil || n != 0 {
		t.Fatalf("second prune: got %d / %v", n, err)
	}
}

// A pruned id can be claimed again: expiry of the guard row is what makes a
// late replay land on the duplicate-notice guard instead.
func TestPrunedIDCanBeClaimedAgain(t *testing.T) {
	s := newStore(t)
	old := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	if !s.RememberFor("env-reclaim", "easypaisa", old) {
		t.Fatal("first claim must win")
	}
	if _, err := s.PruneReplayGuard(old.Add(time.Hour)); err != nil {
		t.Fatalf("prune: %v", err)
	}
	if s.Seen("env-reclaim") {
		t.Fatal("pruned id must not be seen")
	}
	if !s.RememberFor("env-reclaim", "easypaisa", old.Add(2*time.Hour)) {
		t.Fatal("a pruned id must be claimable again")
	}
}

// The guard must be shared across handles on the same file: two processes
// claiming the same id cannot both win.
func TestReplayGuardIsCrossHandle(t *testing.T) {
	path := filepath.Join(t.TempDir(), "shared.db")
	a, err := store.Open(path)
	if err != nil {
		t.Fatalf("open a: %v", err)
	}
	defer a.Close()
	b, err := store.Open(path)
	if err != nil {
		t.Fatalf("open b: %v", err)
	}
	defer b.Close()

	if !a.Remember("env-shared") {
		t.Fatal("handle a must claim the id")
	}
	if b.Remember("env-shared") {
		t.Fatal("handle b must not claim the same id")
	}
	if !b.Seen("env-shared") {
		t.Fatal("handle b must see the id claimed by a")
	}
}
