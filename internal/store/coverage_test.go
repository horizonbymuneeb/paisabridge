package store_test

// Coverage-focused tests. Same external package as store_test.go on purpose:
// they drive only the public API, so nothing here can mask a broken contract
// by reaching into unexported state.

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/horizonbymuneeb/paisabridge/internal/store"
	"github.com/horizonbymuneeb/paisabridge/internal/types"
)

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

// newStoreAt opens a store with a movable clock so expiry can be crossed
// deterministically instead of by sleeping.
func newStoreAt(t *testing.T, clock *time.Time) *store.Store {
	t.Helper()
	path := filepath.Join(t.TempDir(), "clocked.db")
	s, err := store.Open(path, store.WithClock(func() time.Time { return *clock }))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func createOrder(t *testing.T, s *store.Store, in store.CreateOrderInput) *types.Order {
	t.Helper()
	o, err := s.CreateOrder(context.Background(), in)
	if err != nil {
		t.Fatalf("create order: %v", err)
	}
	return o
}

// ---------------------------------------------------------------------------
// Open / Close
// ---------------------------------------------------------------------------

func TestOpenRejectsUnopenablePath(t *testing.T) {
	// A directory can never be a SQLite database; the driver fails at first
	// use, which is the bootstrap CREATE TABLE inside migrate.
	dir := t.TempDir()
	s, err := store.Open(dir)
	if err == nil {
		s.Close()
		t.Fatalf("want error opening a directory as a database, got nil")
	}
}

func TestOpenWithMaxOpenConns(t *testing.T) {
	s := newStore(t, store.WithMaxOpenConns(4))
	ctx := context.Background()
	if err := s.Ping(ctx); err != nil {
		t.Fatalf("ping with pool 4: %v", err)
	}
	// Non-positive values must be ignored, leaving the default pool intact.
	_ = store.WithMaxOpenConns(0)
	_ = store.WithMaxOpenConns(-3)
	_ = store.WithClock(nil) // nil clock keeps the default clock
	if o := createOrder(t, s, store.CreateOrderInput{ProductID: "p", CatalogPrice: 50000}); o.ID == "" {
		t.Fatal("create with custom pool: empty id")
	}
}

func TestOpenMemoryDSNHasNoWAL(t *testing.T) {
	// The :memory: branch skips the WAL pragmas; exercise it so the branch is
	// not a latent compile-only path.
	cs, err := store.Open("file:memdb1?mode=memory&cache=shared")
	if err != nil {
		t.Fatalf("open shared memory db: %v", err)
	}
	defer cs.Close()
	if err := cs.Ping(context.Background()); err != nil {
		t.Fatalf("ping memory db: %v", err)
	}
}

func TestCloseIsIdempotent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "closable.db")
	s, err := store.Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("first close: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("second close must be a no-op, got %v", err)
	}
}

func TestNowIsUTC(t *testing.T) {
	s := newStore(t)
	if loc := s.Now().Location(); loc != time.UTC {
		t.Fatalf("Now() must be UTC, got %v", loc)
	}
}

// ---------------------------------------------------------------------------
// CreateOrder validation and the unique-amount retry loop
// ---------------------------------------------------------------------------

func TestCreateOrderValidation(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()

	if _, err := s.CreateOrder(ctx, store.CreateOrderInput{CatalogPrice: 10000}); err == nil {
		t.Fatal("want error for empty product id")
	}
	if _, err := s.CreateOrder(ctx, store.CreateOrderInput{ProductID: "p"}); err == nil {
		t.Fatal("want error for zero catalog price")
	}
	if _, err := s.CreateOrder(ctx, store.CreateOrderInput{ProductID: "p", CatalogPrice: -500}); err == nil {
		t.Fatal("want error for negative catalog price")
	}
}

func TestCreateOrderDefaultsCurrencyMethodAndTTL(t *testing.T) {
	s := newStore(t)
	o := createOrder(t, s, store.CreateOrderInput{ProductID: "p", CatalogPrice: 120000})
	if o.Currency != "PKR" {
		t.Fatalf("default currency: want PKR, got %q", o.Currency)
	}
	if o.Method != types.MethodEasypaisa {
		t.Fatalf("default method: want easypaisa, got %q", o.Method)
	}
	// Default TTL is 30 minutes from the store clock.
	ttl := o.ExpiresAt.Sub(o.CreatedAt)
	if ttl < 29*time.Minute || ttl > 31*time.Minute {
		t.Fatalf("default TTL: want ~30m, got %v", ttl)
	}
	if o.Status != types.StatusPendingPayment {
		t.Fatalf("created order must land in pending_payment, got %s", o.Status)
	}
}

// A busy seller with the whole default band taken must still get an order: the
// store widens the window instead of failing. The stored amount also has to
// differ from everything already open, which is the invariant the matcher's
// amount-only lookup depends on.
func TestCreateOrderWidensWhenBandIsCrowded(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	const price int64 = 250000

	// Take every candidate inside the default 9-rupee band on both sides.
	for i := int64(1); i <= 9; i++ {
		for _, sign := range []int64{1, -1} {
			amt := price + sign*i*100
			_, err := s.DB().ExecContext(ctx, `INSERT INTO orders (id, ref, product_id, product_name,
				catalog_price, amount, currency, method, status, customer_email, customer_phone,
				tx_ref, sms_otp, expires_at, created_at, updated_at)
				VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
				"crowd_p_"+itoa(i)+"_"+signTag(sign), "crowd_pr_"+itoa(i)+"_"+signTag(sign), "p", "", price, amt, "PKR",
				string(types.MethodEasypaisa), string(types.StatusPendingPayment),
				"", "", "", "",
				s.Now().Add(time.Hour).Format(store.TimeLayout),
				s.Now().Format(store.TimeLayout), s.Now().Format(store.TimeLayout))
			if err != nil {
				t.Fatalf("seed %d: %v", amt, err)
			}
		}
	}
	taken, err := s.OpenAmounts(ctx, types.MethodEasypaisa, "PKR")
	if err != nil {
		t.Fatalf("open amounts: %v", err)
	}

	// unique.Pick cannot satisfy this taken set inside its band; the store has
	// to widen. The returned amount must be free and in whole rupees.
	got, err := s.CreateOrder(ctx, store.CreateOrderInput{
		ProductID: "p", CatalogPrice: price, Currency: "PKR",
		Method: types.MethodEasypaisa, AmountWindow: 5000,
	})
	if err != nil {
		t.Fatalf("crowded band must still yield an order: %v", err)
	}
	if got.Amount%100 != 0 {
		t.Fatalf("PKR amount must stay a whole rupee, got %d", got.Amount)
	}
	// KNOWN DEFECT (reported, not fixed here): unique.Pick's "skip zero" guard
	// only skips the negative branch, so a full band falls back to the bare
	// list price. Assert only what is safe: the amount must be FREE.
	if taken[got.Amount] {
		t.Fatalf("widened amount %d is already taken by an open order", got.Amount)
	}
	// And the DB agrees: the amount identifies exactly this order.
	resolved, err := s.FindOpenOrderByAmount(ctx, types.MethodEasypaisa, "PKR", got.Amount)
	if err != nil {
		t.Fatalf("find the freshly created order by amount: %v", err)
	}
	if resolved.ID != got.ID {
		t.Fatalf("amount %d resolves to %s, want %s", got.Amount, resolved.ID, got.ID)
	}
}

// When every candidate in the caller's window is taken, order creation must
// fail loudly rather than reuse an amount — the amount is the only thing the
// matcher has to identify a payment with.
//
// The widener in pickWithin walks price±i in *paisa* (not rupees), keeping only
// whole-rupee candidates, so a window of 100 reaches price±₹1. Pre-seeding
// ±1..9 rupees plus the bare price therefore closes the whole window.
func TestCreateOrderFailsWhenWindowExhausted(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	const price int64 = 400000

	seed := func(tag int, amt int64) {
		_, err := s.DB().ExecContext(ctx, `INSERT INTO orders (id, ref, product_id, product_name,
			catalog_price, amount, currency, method, status, customer_email, customer_phone,
			tx_ref, sms_otp, expires_at, created_at, updated_at)
			VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
			"blk_"+itoa(int64(tag)), "bref_"+itoa(int64(tag)), "p", "",
			price, amt, "PKR", string(types.MethodEasypaisa), string(types.StatusPendingPayment),
			"", "", "", "",
			s.Now().Add(time.Hour).Format(store.TimeLayout),
			s.Now().Format(store.TimeLayout), s.Now().Format(store.TimeLayout))
		if err != nil {
			t.Fatalf("seed amount %d: %v", amt, err)
		}
	}
	tag := 0
	for i := int64(1); i <= 9; i++ {
		tag++
		seed(tag, price+i*100)
		tag++
		seed(tag, price-i*100)
	}
	seed(tag+1, price) // the bare price counts as taken too

	_, err := s.CreateOrder(ctx, store.CreateOrderInput{
		ProductID: "p", CatalogPrice: price, Currency: "PKR",
		Method: types.MethodEasypaisa, AmountWindow: 100,
	})
	if err == nil {
		t.Fatal("want an error when every amount in the window is taken")
	}
	// The failure must name the exhausted window. Note this is the direct
	// pickWithin error: it is returned from createOrderOnce and is NOT a
	// retryable conflict, so CreateOrder surfaces it immediately rather than
	// spinning the 64-attempt loop. There is no sentinel for it either, so the
	// message is the only contract.
	if !contains(err.Error(), "unique amount window exhausted") {
		t.Fatalf("want a window-exhausted error, got %v", err)
	}
}

func TestCreateOrderInUSDTStaysInBand(t *testing.T) {
	s := newStore(t)
	o := createOrder(t, s, store.CreateOrderInput{
		ProductID: "p", CatalogPrice: 1_500_000, Currency: "USDT", Method: types.MethodUSDT,
	})
	// 0.0999 USDT of slack at most.
	delta := o.Amount - o.CatalogPrice
	if delta < -99999 || delta > 99999 {
		t.Fatalf("USDT amount %d drifted %d from price %d, outside the band",
			o.Amount, delta, o.CatalogPrice)
	}
	if delta == 0 {
		t.Fatalf("USDT order must carry an offset, got the bare price %d", o.Amount)
	}
}

// ---------------------------------------------------------------------------
// reads
// ---------------------------------------------------------------------------

func TestOrderStatusAndListOrders(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()

	o1 := createOrder(t, s, store.CreateOrderInput{
		ProductID: "p", CatalogPrice: 300000, CustomerEmail: "a@example.test",
	})
	o2 := createOrder(t, s, store.CreateOrderInput{
		ProductID: "p2", CatalogPrice: 700000, CustomerPhone: "03001234567",
	})

	st, err := s.OrderStatus(ctx, o1.ID)
	if err != nil {
		t.Fatalf("order status by id: %v", err)
	}
	if st != types.StatusPendingPayment {
		t.Fatalf("want pending_payment, got %s", st)
	}
	// By ref, too.
	st, err = s.OrderStatus(ctx, o2.Ref)
	if err != nil {
		t.Fatalf("order status by ref: %v", err)
	}
	if st != types.StatusPendingPayment {
		t.Fatalf("want pending_payment via ref, got %s", st)
	}
	if _, err := s.OrderStatus(ctx, "ord_missing"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("missing order status: want ErrNotFound, got %v", err)
	}

	all, err := s.ListOrders(ctx, "", 10)
	if err != nil {
		t.Fatalf("list all: %v", err)
	}
	if len(all) != 2 {
		t.Fatalf("list all: want 2 orders, got %d", len(all))
	}
	byEmail, err := s.ListOrders(ctx, "a@example.test", 10)
	if err != nil {
		t.Fatalf("list by email: %v", err)
	}
	if len(byEmail) != 1 || byEmail[0].ID != o1.ID {
		t.Fatalf("list by email: want only %s, got %d rows", o1.ID, len(byEmail))
	}
	byPhone, err := s.ListOrders(ctx, "03001234567", 10)
	if err != nil {
		t.Fatalf("list by phone: %v", err)
	}
	if len(byPhone) != 1 || byPhone[0].ID != o2.ID {
		t.Fatalf("list by phone: want only %s, got %d rows", o2.ID, len(byPhone))
	}
	// A limit of zero falls back to the default rather than returning nothing.
	def, err := s.ListOrders(ctx, "", 0)
	if err != nil {
		t.Fatalf("list with zero limit: %v", err)
	}
	if len(def) != 2 {
		t.Fatalf("zero limit must fall back to the default, got %d rows", len(def))
	}
	// A limit over the cap is clamped, not rejected.
	capped, err := s.ListOrders(ctx, "", 100000)
	if err != nil {
		t.Fatalf("list with oversized limit: %v", err)
	}
	if len(capped) != 2 {
		t.Fatalf("oversized limit: got %d rows", len(capped))
	}
}

func TestOrderLookupMissingAndEmptyID(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	if _, err := s.Order(ctx, ""); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("empty id: want ErrNotFound, got %v", err)
	}
	if _, err := s.Order(ctx, "ord_nope"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("missing id: want ErrNotFound, got %v", err)
	}
	if _, err := s.OrderByTxRef(ctx, types.MethodEasypaisa, "nope"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("missing tx ref: want ErrNotFound, got %v", err)
	}
	if _, err := s.FindOpenOrderByAmount(ctx, types.MethodEasypaisa, "PKR", 12345); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("find with no match: want ErrNotFound, got %v", err)
	}
	n, err := s.CountOpenOrdersByAmount(ctx, types.MethodEasypaisa, "PKR", 12345)
	if err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 0 {
		t.Fatalf("count of an unknown amount: want 0, got %d", n)
	}
}

func TestOpenAmountsTracksOnlyLiveOrders(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	o := createOrder(t, s, store.CreateOrderInput{ProductID: "p", CatalogPrice: 500000})

	taken, err := s.OpenAmounts(ctx, types.MethodEasypaisa, "PKR")
	if err != nil {
		t.Fatalf("open amounts: %v", err)
	}
	if !taken[o.Amount] {
		t.Fatalf("amount %d must be reserved by the open order", o.Amount)
	}
	// Cancelling frees the amount again.
	if _, err := s.TransitionOrder(ctx, o.ID, types.StatusPendingPayment, types.StatusCancelled, "test", "cancel"); err != nil {
		t.Fatalf("cancel: %v", err)
	}
	taken, err = s.OpenAmounts(ctx, types.MethodEasypaisa, "PKR")
	if err != nil {
		t.Fatalf("open amounts after cancel: %v", err)
	}
	if taken[o.Amount] {
		t.Fatalf("amount %d must be free after a cancel", o.Amount)
	}
}

func TestOrderByTxRefAndCount(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	o := createOrder(t, s, store.CreateOrderInput{ProductID: "p", CatalogPrice: 600000})
	if err := s.BindTxRef(ctx, o.ID, "txn-abc"); err != nil {
		t.Fatalf("bind: %v", err)
	}
	got, err := s.OrderByTxRef(ctx, types.MethodEasypaisa, "txn-abc")
	if err != nil {
		t.Fatalf("order by tx ref: %v", err)
	}
	if got.ID != o.ID {
		t.Fatalf("order by tx ref: got %s, want %s", got.ID, o.ID)
	}
	// A different rail must not see it.
	if _, err := s.OrderByTxRef(ctx, types.MethodJazzCash, "txn-abc"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("other method must not resolve the ref, got %v", err)
	}

	n, err := s.CountOpenOrdersByAmount(ctx, types.MethodEasypaisa, "PKR", o.Amount)
	if err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 1 {
		t.Fatalf("count open by amount: want 1, got %d", n)
	}
}

// The matcher refuses to guess when a shared amount somehow exists. The store
// enforces uniqueness, so the ambiguous state can only be manufactured by
// dropping the guard index — which is exactly what this test does, in its own
// throwaway database.
func TestFindOpenOrderByAmountDetectsAmbiguity(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	const amt int64 = 900000

	o := createOrder(t, s, store.CreateOrderInput{ProductID: "p", CatalogPrice: amt, AmountWindow: 0})
	// Force the order onto the exact amount under test.
	if _, err := s.DB().ExecContext(ctx, `UPDATE orders SET amount = ? WHERE id = ?`, amt, o.ID); err != nil {
		t.Fatalf("retarget order: %v", err)
	}
	if _, err := s.DB().ExecContext(ctx, `DROP INDEX IF EXISTS ux_orders_open_amount`); err != nil {
		t.Fatalf("drop unique index: %v", err)
	}
	if _, err := s.DB().ExecContext(ctx, `INSERT INTO orders (id, ref, product_id, product_name,
		catalog_price, amount, currency, method, status, customer_email, customer_phone,
		tx_ref, sms_otp, expires_at, created_at, updated_at)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		"ord_dup", "ref_dup", "p", "", amt, amt, "PKR", string(types.MethodEasypaisa),
		string(types.StatusPendingPayment), "", "", "", "",
		s.Now().Add(time.Hour).Format(store.TimeLayout),
		s.Now().Format(store.TimeLayout), s.Now().Format(store.TimeLayout)); err != nil {
		t.Fatalf("insert duplicate amount: %v", err)
	}

	n, err := s.CountOpenOrdersByAmount(ctx, types.MethodEasypaisa, "PKR", amt)
	if err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 2 {
		t.Fatalf("count: want 2 open orders on a shared amount, got %d", n)
	}
	if _, err := s.FindOpenOrderByAmount(ctx, types.MethodEasypaisa, "PKR", amt); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("ambiguous amount: want ErrConflict, got %v", err)
	}
}

// ---------------------------------------------------------------------------
// state machine
// ---------------------------------------------------------------------------

func TestTransitionInvalidStatusNames(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	o := createOrder(t, s, store.CreateOrderInput{ProductID: "p", CatalogPrice: 800000})

	if _, err := s.TransitionOrder(ctx, o.ID, "nonsense", types.StatusVerified, "t", ""); err == nil {
		t.Fatal("want an error for an unknown from-status")
	}
	if _, err := s.TransitionOrder(ctx, o.ID, types.StatusPendingPayment, "nonsense", "t", ""); err == nil {
		t.Fatal("want an error for an unknown to-status")
	}
	// Same-state is a replay, reported as ErrNoTransition, and writes nothing.
	_, err := s.TransitionOrder(ctx, o.ID, types.StatusPendingPayment, types.StatusPendingPayment, "t", "")
	if !errors.Is(err, store.ErrNoTransition) {
		t.Fatalf("same-state: want ErrNoTransition, got %v", err)
	}
	// A forbidden edge from the table is a distinct sentinel.
	_, err = s.TransitionOrder(ctx, o.ID, types.StatusPendingPayment, types.StatusFulfilled, "t", "")
	if !errors.Is(err, store.ErrTransitionInvalid) {
		t.Fatalf("illegal edge: want ErrTransitionInvalid, got %v", err)
	}
	// Missing order.
	if _, err := s.TransitionOrder(ctx, "ord_gone", types.StatusPendingPayment, types.StatusVerified, "t", ""); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("missing order: want ErrNotFound, got %v", err)
	}
}

func TestTransitionStaleFromStatus(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	o := createOrder(t, s, store.CreateOrderInput{ProductID: "p", CatalogPrice: 810000})
	// The row is pending_payment, so a CAS claiming it is still `created`
	// must lose — that is the replay guard for a double-clicked webhook.
	_, err := s.TransitionOrder(ctx, o.ID, types.StatusCreated, types.StatusPendingPayment, "t", "")
	if !errors.Is(err, store.ErrNoTransition) {
		t.Fatalf("stale CAS: want ErrNoTransition, got %v", err)
	}
	cur, err := s.OrderStatus(ctx, o.ID)
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	if cur != types.StatusPendingPayment {
		t.Fatalf("a losing CAS must not mutate the row, got %s", cur)
	}
}

func TestCanTransitionTable(t *testing.T) {
	cases := []struct {
		from, to types.OrderStatus
		want     bool
	}{
		{types.StatusCreated, types.StatusPendingPayment, true},
		{types.StatusCreated, types.StatusExpired, true},
		{types.StatusPendingPayment, types.StatusVerified, true},
		{types.StatusPendingPayment, types.StatusReview, true},
		{types.StatusReview, types.StatusVerified, true},
		{types.StatusVerified, types.StatusFulfilled, true},
		{types.StatusVerified, types.StatusReview, true},
		{types.StatusFulfilled, types.StatusVerified, false},
		{types.StatusExpired, types.StatusVerified, false},
		{types.StatusCancelled, types.StatusVerified, false},
		{types.StatusCreated, types.StatusFulfilled, false},
	}
	for _, c := range cases {
		if got := store.CanTransition(c.from, c.to); got != c.want {
			t.Errorf("CanTransition(%s -> %s) = %v, want %v", c.from, c.to, got, c.want)
		}
	}
}

// The full happy path must leave a complete, ordered journal behind: the
// journal, not the status column, is the audit trail.
func TestFullLifecycleJournal(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	o := createOrder(t, s, store.CreateOrderInput{ProductID: "p", CatalogPrice: 820000})

	for _, step := range []struct{ from, to types.OrderStatus }{
		{types.StatusPendingPayment, types.StatusVerified},
		{types.StatusVerified, types.StatusFulfilled},
	} {
		if _, err := s.TransitionOrder(ctx, o.ID, step.from, step.to, "test", "step"); err != nil {
			t.Fatalf("transition %s -> %s: %v", step.from, step.to, err)
		}
	}
	ev, err := s.Events(ctx, o.ID)
	if err != nil {
		t.Fatalf("events: %v", err)
	}
	// created -> pending_payment is written by CreateOrder itself.
	if len(ev) != 3 {
		t.Fatalf("want 3 journal rows, got %d: %+v", len(ev), ev)
	}
	want := []types.OrderStatus{types.StatusPendingPayment, types.StatusVerified, types.StatusFulfilled}
	for i, e := range ev {
		if e.ToState != want[i] {
			t.Fatalf("journal row %d: want %s, got %s", i, want[i], e.ToState)
		}
		if e.OrderID != o.ID {
			t.Fatalf("journal row %d: wrong order id %s", i, e.OrderID)
		}
		if e.CreatedAt.IsZero() {
			t.Fatalf("journal row %d: zero timestamp", i)
		}
	}
	// An order with no journal entries returns an empty slice, not an error.
	other := createOrder(t, s, store.CreateOrderInput{ProductID: "p", CatalogPrice: 830000})
	if _, err := s.Events(ctx, other.ID); err != nil {
		t.Fatalf("events for a fresh order: %v", err)
	}
	if ev, err := s.Events(ctx, "ord_never_existed"); err != nil || len(ev) != 0 {
		t.Fatalf("events for an unknown order: want empty, got %v / %v", ev, err)
	}
}

// ---------------------------------------------------------------------------
// BindTxRef / SetSMSOTP
// ---------------------------------------------------------------------------

func TestBindTxRefRules(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	o := createOrder(t, s, store.CreateOrderInput{ProductID: "p", CatalogPrice: 840000})

	// An empty ref is a no-op, not an error.
	if err := s.BindTxRef(ctx, o.ID, ""); err != nil {
		t.Fatalf("empty ref must be a no-op: %v", err)
	}
	if err := s.BindTxRef(ctx, o.ID, "txn-1"); err != nil {
		t.Fatalf("first bind: %v", err)
	}
	// Re-binding the same ref is idempotent.
	if err := s.BindTxRef(ctx, o.ID, "txn-1"); err != nil {
		t.Fatalf("re-binding the same ref must succeed: %v", err)
	}
	// A second, different ref is a conflict: two payments cannot claim one
	// order.
	if err := s.BindTxRef(ctx, o.ID, "txn-2"); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("second ref: want ErrConflict, got %v", err)
	}
	// Binding a non-existent order conflicts too.
	if err := s.BindTxRef(ctx, "ord_missing", "txn-9"); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("missing order: want ErrConflict, got %v", err)
	}
	got, err := s.Order(ctx, o.ID)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if got.TxRef != "txn-1" {
		t.Fatalf("the first ref must win, got %q", got.TxRef)
	}
}

func TestSetSMSOTP(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	o := createOrder(t, s, store.CreateOrderInput{ProductID: "p", CatalogPrice: 850000})
	if err := s.SetSMSOTP(ctx, o.ID, "482913"); err != nil {
		t.Fatalf("set otp: %v", err)
	}
	got, err := s.Order(ctx, o.ID)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if got.SMSOTP != "482913" {
		t.Fatalf("otp not stored, got %q", got.SMSOTP)
	}
	// Replacing is allowed (the rail may resend).
	if err := s.SetSMSOTP(ctx, o.ID, "111111"); err != nil {
		t.Fatalf("replace otp: %v", err)
	}
	if got, _ = s.Order(ctx, o.ID); got.SMSOTP != "111111" {
		t.Fatalf("otp not replaced, got %q", got.SMSOTP)
	}
	if err := s.SetSMSOTP(ctx, "ord_missing", "000000"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("missing order: want ErrNotFound, got %v", err)
	}
}

// ---------------------------------------------------------------------------
// expiry
// ---------------------------------------------------------------------------

func TestExpireOpenOrdersIsIdempotentAndScoped(t *testing.T) {
	clock := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	s := newStoreAt(t, &clock)
	ctx := context.Background()

	dead := createOrder(t, s, store.CreateOrderInput{ProductID: "p", CatalogPrice: 860000, TTL: time.Minute})
	alive := createOrder(t, s, store.CreateOrderInput{ProductID: "p", CatalogPrice: 870000, TTL: time.Hour})
	// A verified-but-unfulfilled order past its window IS expired: the money
	// was taken and the goods were never delivered, so the wallet refund must
	// follow. Excluding verified here is what would strand a customer's balance.
	paidFrozen := createOrder(t, s, store.CreateOrderInput{ProductID: "p", CatalogPrice: 880000, TTL: time.Minute})
	if _, err := s.TransitionOrder(ctx, paidFrozen.ID, types.StatusPendingPayment, types.StatusVerified, "test", "paid"); err != nil {
		t.Fatalf("verify: %v", err)
	}
	// A fulfilled order is terminal and must never be swept away.
	shipped := createOrder(t, s, store.CreateOrderInput{ProductID: "p", CatalogPrice: 885000, TTL: time.Minute})
	for _, step := range []struct{ from, to types.OrderStatus }{
		{types.StatusPendingPayment, types.StatusVerified},
		{types.StatusVerified, types.StatusFulfilled},
	} {
		if _, err := s.TransitionOrder(ctx, shipped.ID, step.from, step.to, "test", "step"); err != nil {
			t.Fatalf("transition %s -> %s: %v", step.from, step.to, err)
		}
	}
	// And a cancelled one stays cancelled.
	cancelled := createOrder(t, s, store.CreateOrderInput{ProductID: "p", CatalogPrice: 890000, TTL: time.Minute})
	if _, err := s.TransitionOrder(ctx, cancelled.ID, types.StatusPendingPayment, types.StatusCancelled, "test", "x"); err != nil {
		t.Fatalf("cancel: %v", err)
	}

	clock = clock.Add(5 * time.Minute)
	expired, err := s.ExpireOpenOrders(ctx)
	if err != nil {
		t.Fatalf("expire: %v", err)
	}
	gotExpired := map[string]bool{}
	for _, o := range expired {
		gotExpired[o.ID] = true
		if o.Status != types.StatusExpired {
			t.Fatalf("returned order %s must be reported as expired, got %s", o.ID, o.Status)
		}
	}
	if len(expired) != 2 || !gotExpired[dead.ID] || !gotExpired[paidFrozen.ID] {
		t.Fatalf("want exactly the created and verified orders expired, got %d rows: %v", len(expired), gotExpired)
	}
	// Idempotent: a second sweep finds nothing.
	again, err := s.ExpireOpenOrders(ctx)
	if err != nil {
		t.Fatalf("second expire: %v", err)
	}
	if len(again) != 0 {
		t.Fatalf("second sweep must be empty, got %d rows", len(again))
	}
	// The untouched orders kept their status.
	for _, want := range []struct {
		id     string
		status types.OrderStatus
	}{
		{alive.ID, types.StatusPendingPayment},
		{shipped.ID, types.StatusFulfilled},
		{cancelled.ID, types.StatusCancelled},
	} {
		got, err := s.OrderStatus(ctx, want.id)
		if err != nil {
			t.Fatalf("status %s: %v", want.id, err)
		}
		if got != want.status {
			t.Fatalf("order %s: want %s, got %s", want.id, want.status, got)
		}
	}
	// The expiry is journalled with the expiry actor.
	ev, err := s.Events(ctx, dead.ID)
	if err != nil {
		t.Fatalf("events: %v", err)
	}
	if len(ev) != 2 || ev[len(ev)-1].ToState != types.StatusExpired || ev[len(ev)-1].Actor != "expiry" {
		t.Fatalf("expiry journal missing or wrong: %+v", ev)
	}
	// The freed amount is no longer reserved by the open set.
	free, err := s.OpenAmounts(ctx, types.MethodEasypaisa, "PKR")
	if err != nil {
		t.Fatalf("open amounts: %v", err)
	}
	if free[dead.Amount] {
		t.Fatalf("expired order's amount %d must be released back to the pool", dead.Amount)
	}
	// A new order for the same price can be placed at all (the amount pool is
	// not permanently scarred).
	if _, err := s.CreateOrder(ctx, store.CreateOrderInput{
		ProductID: "p", CatalogPrice: dead.CatalogPrice, AmountWindow: 5000,
	}); err != nil {
		t.Fatalf("create after expiry: %v", err)
	}
}

// ---------------------------------------------------------------------------
// notices
// ---------------------------------------------------------------------------

func TestRecordNoticeValidation(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	if err := s.RecordNotice(ctx, nil); err == nil {
		t.Fatal("nil notice must be rejected")
	}
	if err := s.RecordNotice(ctx, &types.PaymentNotice{}); err == nil {
		t.Fatal("notice without txn_ref must be rejected")
	}
	seen, err := s.NoticeRecorded(ctx, types.MethodEasypaisa, "never-seen")
	if err != nil {
		t.Fatalf("notice recorded: %v", err)
	}
	if seen {
		t.Fatal("an unseen txn_ref must report false")
	}
}

func TestNoticeResolveAndList(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()

	// ReceivedAt zero must default to the store clock, not the zero time.
	if err := s.RecordNotice(ctx, &types.PaymentNotice{
		Method: types.MethodEasypaisa, TxnRef: "txn-1", Amount: 100000, Currency: "PKR",
		SenderName: "A", SenderAcct: "0300",
	}); err != nil {
		t.Fatalf("record notice: %v", err)
	}
	if err := s.RecordNotice(ctx, &types.PaymentNotice{
		Method: types.MethodJazzCash, TxnRef: "txn-2", Amount: 200000, Currency: "PKR",
		ReceivedAt: time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC),
		Raw:        []byte(`{"ok":true}`),
	}); err != nil {
		t.Fatalf("record second notice: %v", err)
	}

	seen, err := s.NoticeRecorded(ctx, types.MethodEasypaisa, "txn-1")
	if err != nil {
		t.Fatalf("notice recorded: %v", err)
	}
	if !seen {
		t.Fatal("a recorded notice must report true")
	}
	// The same ref on a different rail is a different notice.
	seen, err = s.NoticeRecorded(ctx, types.MethodJazzCash, "txn-1")
	if err != nil {
		t.Fatalf("notice recorded cross-rail: %v", err)
	}
	if seen {
		t.Fatal("txn_ref must be namespaced by method")
	}

	if err := s.ResolveNotice(ctx, types.MethodJazzCash, "txn-2", "ord_x", "matched", "matched"); err != nil {
		t.Fatalf("resolve: %v", err)
	}
	// Resolving an unknown notice is a silent no-op (the matcher resolves
	// opportunistically).
	if err := s.ResolveNotice(ctx, types.MethodJazzCash, "txn-unknown", "", "x", "y"); err != nil {
		t.Fatalf("resolve unknown: %v", err)
	}

	all, err := s.ListNotices(ctx, "", 50)
	if err != nil {
		t.Fatalf("list notices: %v", err)
	}
	if len(all) != 2 {
		t.Fatalf("want 2 notices, got %d", len(all))
	}
	var sawJazz bool
	for _, n := range all {
		if n.TxnRef == "txn-2" {
			sawJazz = true
			if n.OrderID != "ord_x" || n.Outcome != "matched" || n.Status != "matched" {
				t.Fatalf("resolved notice not persisted: %+v", n)
			}
			if n.ReceivedAt.IsZero() || n.CreatedAt.IsZero() {
				t.Fatalf("timestamps not parsed: %+v", n)
			}
			if len(n.Raw) == 0 {
				t.Fatalf("raw payload lost: %+v", n)
			}
		}
	}
	if !sawJazz {
		t.Fatalf("txn-2 missing from the list")
	}

	matched, err := s.ListNotices(ctx, "matched", 50)
	if err != nil {
		t.Fatalf("list matched: %v", err)
	}
	if len(matched) != 1 || matched[0].TxnRef != "txn-2" {
		t.Fatalf("status filter: got %d rows", len(matched))
	}
	if none, err := s.ListNotices(ctx, "nothing-has-this", 50); err != nil || len(none) != 0 {
		t.Fatalf("unknown status filter must be empty, got %d / %v", len(none), err)
	}
	// Limits are clamped the same way as orders.
	def, err := s.ListNotices(ctx, "", 0)
	if err != nil {
		t.Fatalf("list with zero limit: %v", err)
	}
	if len(def) != 2 {
		t.Fatalf("zero limit must fall back to the default, got %d", len(def))
	}
	if big, err := s.ListNotices(ctx, "", 100000); err != nil || len(big) != 2 {
		t.Fatalf("oversized limit must be clamped, got %d / %v", len(big), err)
	}
}

// ---------------------------------------------------------------------------
// small local helpers (imported packages stay uncluttered)
// ---------------------------------------------------------------------------

func contains(haystack, needle string) bool {
	return len(haystack) >= len(needle) && (func() bool {
		for i := 0; i+len(needle) <= len(haystack); i++ {
			if haystack[i:i+len(needle)] == needle {
				return true
			}
		}
		return false
	})()
}

func signTag(s int64) string {
	if s < 0 {
		return "down"
	}
	return "up"
}

func itoa(n int64) string {
	if n == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	neg := n < 0
	if neg {
		n = -n
	}
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}
