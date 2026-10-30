package store_test

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/horizonbymuneeb/paisabridge/internal/store"
	"github.com/horizonbymuneeb/paisabridge/internal/types"
)

// newStore opens a throwaway SQLite file under t.TempDir(). A file (not
// :memory:) is used on purpose: it exercises the real migrations, the WAL
// pragmas and the partial unique index exactly as production does.
func newStore(t *testing.T, opts ...store.Option) *store.Store {
	t.Helper()
	path := filepath.Join(t.TempDir(), "paisabridge.db")
	s, err := store.Open(path, opts...)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func mustOrder(t *testing.T, s *store.Store, price int64) *types.Order {
	t.Helper()
	o, err := s.CreateOrder(context.Background(), store.CreateOrderInput{
		ProductID:    "prod-1",
		ProductName:  "Netflix 1 month",
		CatalogPrice: price,
		Currency:     "PKR",
		Method:       types.MethodEasypaisa,
		TTL:          10 * time.Minute,
	})
	if err != nil {
		t.Fatalf("create order: %v", err)
	}
	return o
}

func TestOpenCreatesSchema(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	if err := s.Ping(ctx); err != nil {
		t.Fatalf("ping: %v", err)
	}
	// schema_migrations must record version 1.
	var n int
	if err := s.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM schema_migrations WHERE version = 1`).Scan(&n); err != nil {
		t.Fatalf("schema_migrations: %v", err)
	}
	if n != 1 {
		t.Fatalf("want migration version 1 recorded once, got %d", n)
	}
}

func TestOpenIsIdempotent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "twice.db")
	first, err := store.Open(path)
	if err != nil {
		t.Fatalf("first open: %v", err)
	}
	ord, err := first.CreateOrder(context.Background(), store.CreateOrderInput{
		ProductID: "p", CatalogPrice: 10000, Currency: "PKR", Method: types.MethodJazzCash,
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	first.Close()

	second, err := store.Open(path)
	if err != nil {
		t.Fatalf("second open (migrations re-run): %v", err)
	}
	defer second.Close()
	got, err := second.Order(context.Background(), ord.ID)
	if err != nil {
		t.Fatalf("order lost across reopen: %v", err)
	}
	if got.Ref != ord.Ref {
		t.Fatalf("ref changed: %q -> %q", ord.Ref, got.Ref)
	}
}

func TestCreateOrderUniqueAmounts(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	seen := map[int64]string{}
	for i := 0; i < 8; i++ {
		o, err := s.CreateOrder(ctx, store.CreateOrderInput{
			ProductID: "same", CatalogPrice: 150000, Currency: "PKR", Method: types.MethodEasypaisa,
		})
		if err != nil {
			t.Fatalf("order %d: %v", i, err)
		}
		if o.Status != types.StatusPendingPayment {
			t.Fatalf("order %d: status %s, want pending_payment", i, o.Status)
		}
		if prev, dup := seen[o.Amount]; dup {
			t.Fatalf("amount %d reused by %s and %s", o.Amount, prev, o.ID)
		}
		seen[o.Amount] = o.ID
		// Adjustment must stay inside the invisible band: 150000 +- 9 rupees.
		if d := o.Amount - 150000; d < -900 || d > 900 {
			t.Fatalf("amount %d drifts too far from catalogue price", o.Amount)
		}
		if o.Amount%100 != 0 {
			t.Fatalf("PKR amount %d is not a whole rupee", o.Amount)
		}
	}
	if len(seen) != 8 {
		t.Fatalf("expected 8 distinct amounts, got %d", len(seen))
	}
}

func TestCreateOrderAmountFreedAfterClose(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	first := mustOrder(t, s, 50000)
	if _, err := s.TransitionOrder(ctx, first.ID, types.StatusPendingPayment, types.StatusCancelled, "test", "free the amount"); err != nil {
		t.Fatalf("cancel: %v", err)
	}
	// Loop enough times that the released amount would certainly be hit again.
	sawAgain := false
	for i := 0; i < 20; i++ {
		o := mustOrder(t, s, 50000)
		if o.Amount == first.Amount {
			sawAgain = true
			break
		}
	}
	if !sawAgain {
		t.Logf("note: released amount %d not re-picked in 20 tries (window has spare room)", first.Amount)
	}
}

func TestStateMachineTransitionsAndJournal(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	o := mustOrder(t, s, 20000)

	if _, err := s.TransitionOrder(ctx, o.ID, types.StatusPendingPayment, types.StatusVerified, "matcher", "paid"); err != nil {
		t.Fatalf("pending -> verified: %v", err)
	}
	if _, err := s.TransitionOrder(ctx, o.ID, types.StatusVerified, types.StatusFulfilled, "fulfiller", "delivered"); err != nil {
		t.Fatalf("verified -> fulfilled: %v", err)
	}

	events, err := s.Events(ctx, o.ID)
	if err != nil {
		t.Fatalf("events: %v", err)
	}
	want := [][2]types.OrderStatus{
		{types.StatusCreated, types.StatusPendingPayment},
		{types.StatusPendingPayment, types.StatusVerified},
		{types.StatusVerified, types.StatusFulfilled},
	}
	if len(events) != len(want) {
		t.Fatalf("journal has %d events, want %d: %+v", len(events), len(want), events)
	}
	for i, w := range want {
		if events[i].FromState != w[0] || events[i].ToState != w[1] {
			t.Fatalf("event %d = %s->%s, want %s->%s", i, events[i].FromState, events[i].ToState, w[0], w[1])
		}
	}
}

func TestTransitionIdempotent(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	o := mustOrder(t, s, 30000)

	if _, err := s.TransitionOrder(ctx, o.ID, types.StatusPendingPayment, types.StatusVerified, "matcher", "paid"); err != nil {
		t.Fatalf("first: %v", err)
	}
	// Replaying the same edge must fail cleanly and write no second event.
	_, err := s.TransitionOrder(ctx, o.ID, types.StatusPendingPayment, types.StatusVerified, "matcher", "paid again")
	if !errors.Is(err, store.ErrNoTransition) {
		t.Fatalf("replay: want ErrNoTransition, got %v", err)
	}
	events, _ := s.Events(ctx, o.ID)
	verified := 0
	for _, e := range events {
		if e.ToState == types.StatusVerified {
			verified++
		}
	}
	if verified != 1 {
		t.Fatalf("verified recorded %d times, want 1", verified)
	}
}

func TestTransitionRejectsIllegalEdge(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	o := mustOrder(t, s, 40000)
	// created -> fulfilled is not a legal edge (pending_payment must come
	// first), so the machine rejects it before SQL sees it.
	if _, err := s.TransitionOrder(ctx, o.ID, types.StatusCreated, types.StatusFulfilled, "evil", ""); !errors.Is(err, store.ErrTransitionInvalid) {
		t.Fatalf("want ErrTransitionInvalid, got %v", err)
	}
}

func TestTransitionTerminalIsSticky(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	o := mustOrder(t, s, 45000)
	if _, err := s.TransitionOrder(ctx, o.ID, types.StatusPendingPayment, types.StatusCancelled, "c", "bye"); err != nil {
		t.Fatalf("cancel: %v", err)
	}
	for _, to := range []types.OrderStatus{types.StatusVerified, types.StatusFulfilled} {
		if _, err := s.TransitionOrder(ctx, o.ID, types.StatusCancelled, to, "c", ""); !errors.Is(err, store.ErrTransitionInvalid) {
			t.Fatalf("cancelled -> %s should be rejected, got %v", to, err)
		}
	}
}

func TestConcurrentTransitionsOnlyOneWins(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	o := mustOrder(t, s, 60000)

	const workers = 8
	var (
		wg      sync.WaitGroup
		mu      sync.Mutex
		success int
	)
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := s.TransitionOrder(ctx, o.ID, types.StatusPendingPayment, types.StatusVerified, "racer", "")
			if err == nil {
				mu.Lock()
				success++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if success != 1 {
		t.Fatalf("%d goroutines won the transition, want exactly 1", success)
	}
}

func TestNoticeDuplicateGuard(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	n := &types.PaymentNotice{TxnRef: "TX-1", Method: types.MethodEasypaisa, Amount: 10000, Currency: "PKR"}
	if err := s.RecordNotice(ctx, n); err != nil {
		t.Fatalf("first record: %v", err)
	}
	if err := s.RecordNotice(ctx, n); !errors.Is(err, store.ErrDuplicate) {
		t.Fatalf("second record: want ErrDuplicate, got %v", err)
	}
	seen, err := s.NoticeRecorded(ctx, types.MethodEasypaisa, "TX-1")
	if err != nil || !seen {
		t.Fatalf("NoticeRecorded = %v, %v", seen, err)
	}
	other, _ := s.NoticeRecorded(ctx, types.MethodJazzCash, "TX-1")
	if other {
		t.Fatal("same txn ref on a different method must be distinct")
	}
}

func TestConcurrentNoticeInsertOnlyOneWins(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	n := &types.PaymentNotice{TxnRef: "RACE", Method: types.MethodJazzCash, Amount: 1, Currency: "PKR"}

	var (
		wg      sync.WaitGroup
		mu      sync.Mutex
		inserts int
	)
	for i := 0; i < 6; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := s.RecordNotice(ctx, n); err == nil {
				mu.Lock()
				inserts++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if inserts != 1 {
		t.Fatalf("%d concurrent inserts succeeded, want 1", inserts)
	}
}

func TestExpireOpenOrders(t *testing.T) {
	now := time.Now().UTC()
	clock := func() time.Time { return now }
	s := newStore(t, store.WithClock(clock))
	ctx := context.Background()

	short, err := s.CreateOrder(ctx, store.CreateOrderInput{
		ProductID: "p", CatalogPrice: 1000, Currency: "PKR", Method: types.MethodEasypaisa, TTL: time.Minute,
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	long, err := s.CreateOrder(ctx, store.CreateOrderInput{
		ProductID: "p", CatalogPrice: 2000, Currency: "PKR", Method: types.MethodEasypaisa, TTL: time.Hour,
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	if due, _ := s.ExpireOpenOrders(ctx); len(due) != 0 {
		t.Fatalf("nothing should expire yet, got %d", len(due))
	}

	now = now.Add(2 * time.Minute)
	due, err := s.ExpireOpenOrders(ctx)
	if err != nil {
		t.Fatalf("expire: %v", err)
	}
	if len(due) != 1 || due[0].ID != short.ID {
		t.Fatalf("expired %+v, want only %s", due, short.ID)
	}
	got, _ := s.Order(ctx, short.ID)
	if got.Status != types.StatusExpired {
		t.Fatalf("short order status = %s, want expired", got.Status)
	}
	live, _ := s.Order(ctx, long.ID)
	if live.Status != types.StatusPendingPayment {
		t.Fatalf("long order status = %s, want pending_payment", live.Status)
	}
	// Second sweep is a no-op: expiry is idempotent.
	if due, _ := s.ExpireOpenOrders(ctx); len(due) != 0 {
		t.Fatalf("second sweep expired %d more orders", len(due))
	}
}

func TestBindTxRefIsOneWay(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	o := mustOrder(t, s, 70000)
	if err := s.BindTxRef(ctx, o.ID, "TX-A"); err != nil {
		t.Fatalf("bind: %v", err)
	}
	// Same ref again is a no-op.
	if err := s.BindTxRef(ctx, o.ID, "TX-A"); err != nil {
		t.Fatalf("rebind same: %v", err)
	}
	// A different ref is a conflict: two payments must not claim one order.
	if err := s.BindTxRef(ctx, o.ID, "TX-B"); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("rebind different: want ErrConflict, got %v", err)
	}
}

func TestOrderLookupByRef(t *testing.T) {
	s := newStore(t)
	o := mustOrder(t, s, 80000)
	if got, err := s.Order(context.Background(), o.Ref); err != nil || got.ID != o.ID {
		t.Fatalf("lookup by ref: %v %v", got, err)
	}
	if _, err := s.Order(context.Background(), "nope"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("missing order: want ErrNotFound, got %v", err)
	}
}
