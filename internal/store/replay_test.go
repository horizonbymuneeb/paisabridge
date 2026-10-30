package store

import (
	"path/filepath"
	"testing"
	"time"
)

func newTestStore(t *testing.T) *Store {
	t.Helper()
	st, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

// TestReplayGuardClaimsOnce pins the atomic claim semantics: the first
// Remember wins, every repeat loses, and Seen agrees.
func TestReplayGuardClaimsOnce(t *testing.T) {
	st := newTestStore(t)
	id := "ntc-abc"
	if st.Seen(id) {
		t.Fatal("fresh id reported as seen")
	}
	if !st.Remember(id) {
		t.Fatal("first Remember must win")
	}
	if !st.Seen(id) {
		t.Fatal("remembered id not reported as seen")
	}
	for i := 0; i < 3; i++ {
		if st.Remember(id) {
			t.Fatalf("repeat %d must lose the claim", i)
		}
	}
}

// TestReplayGuardConcurrentClaims: exactly one of many concurrent claims wins.
func TestReplayGuardConcurrentClaims(t *testing.T) {
	st := newTestStore(t)
	const n = 25
	results := make(chan bool, n)
	for i := 0; i < n; i++ {
		go func() { results <- st.Remember("ntc-race") }()
	}
	wins := 0
	for i := 0; i < n; i++ {
		if <-results {
			wins++
		}
	}
	if wins != 1 {
		t.Fatalf("want exactly 1 winner, got %d (double-accept risk)", wins)
	}
}

func TestPruneReplayGuard(t *testing.T) {
	st := newTestStore(t)
	old := time.Now().UTC().Add(-48 * time.Hour)
	st.RememberFor("old-1", "bank", old)
	st.RememberFor("new-1", "bank", time.Now().UTC())
	n, err := st.PruneReplayGuard(time.Now().UTC().Add(-24 * time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("want 1 pruned, got %d", n)
	}
	if st.Seen("old-1") || !st.Seen("new-1") {
		t.Fatal("prune removed the wrong rows")
	}
}
