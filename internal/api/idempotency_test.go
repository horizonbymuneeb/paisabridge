package api_test

//
// Order-create idempotency, tested through the real HTTP server.
//
// The behaviour promised to integrators (docs/API.md, "Idempotent order
// creation"):
//
//	same key + same buyer + same body     -> 200, the ORIGINAL order (no new one)
//	same key + same buyer + other body    -> 409 idempotency_conflict
//	key still in flight                   -> 409 idempotency_in_flight
//	no key                                -> a new order every time (unchanged)
//
// "Same body" ignores fields that do not change the result: an omitted
// currency/method/ttl and an explicit default are the same request.

import (
	"context"
	"net/http"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/horizonbymuneeb/paisabridge/internal/api"
	"github.com/horizonbymuneeb/paisabridge/internal/store"
	"github.com/horizonbymuneeb/paisabridge/internal/types"
)

// orderBody builds a create-order request, overriding fields via the map.
func orderBody(key string, extra map[string]any) map[string]any {
	b := map[string]any{
		"product_id":          "netflix-1m",
		"product_name":        "Netflix 1 month",
		"catalog_price_minor": int64(150000),
		"currency":            "PKR",
		"method":              "easypaisa",
	}
	if key != "" {
		b["idempotency_key"] = key
	}
	for k, v := range extra {
		b[k] = v
	}
	return b
}

// openOrderCount counts the orders the store currently holds.
func (h *harness) openOrderCount() int {
	h.t.Helper()
	orders, err := h.store.ListOrders(context.Background(), "", 500)
	if err != nil {
		h.t.Fatalf("list orders: %v", err)
	}
	return len(orders)
}

func TestIdempotentCreateReplaysTheSameOrder(t *testing.T) {
	h := newHarness(t)

	code, body := h.do("POST", "/api/orders", orderBody("key-retry-1", nil), nil)
	if code != http.StatusCreated {
		t.Fatalf("first create: %d %s", code, body)
	}
	var first types.Order
	decodeInto(t, body, &first)

	code, body = h.do("POST", "/api/orders", orderBody("key-retry-1", nil), nil)
	if code != http.StatusOK {
		t.Fatalf("retry: %d %s, want 200 (replay of the original order)", code, body)
	}
	var second types.Order
	decodeInto(t, body, &second)

	if second.ID != first.ID {
		t.Fatalf("retry returned order %s, want the original %s", second.ID, first.ID)
	}
	if second.Amount != first.Amount {
		t.Fatalf("retry amount = %d, want the original %d", second.Amount, first.Amount)
	}
	if n := h.openOrderCount(); n != 1 {
		t.Fatalf("store holds %d orders after a retried create, want exactly 1", n)
	}

	// A third retry must still be the same order: replay is not one-shot.
	code, body = h.do("POST", "/api/orders", orderBody("key-retry-1", nil), nil)
	if code != http.StatusOK {
		t.Fatalf("second retry: %d %s", code, body)
	}
	var third types.Order
	decodeInto(t, body, &third)
	if third.ID != first.ID {
		t.Fatalf("second retry returned %s, want %s", third.ID, first.ID)
	}
	// A different key creates a genuinely new order, which is the control:
	// without it the test above would pass even if creates were broken.
	code, body = h.do("POST", "/api/orders", orderBody("key-retry-2", nil), nil)
	if code != http.StatusCreated {
		t.Fatalf("different key: %d %s", code, body)
	}
	if n := h.openOrderCount(); n != 2 {
		t.Fatalf("store holds %d orders, want 2", n)
	}
}

func TestIdempotentCreateRejectsDifferentBody(t *testing.T) {
	h := newHarness(t)

	code, body := h.do("POST", "/api/orders", orderBody("key-conflict", nil), nil)
	if code != http.StatusCreated {
		t.Fatalf("first create: %d %s", code, body)
	}

	// Same key, different product: this is a client bug, not a retry, and must
	// not silently hand back an order for the wrong product.
	code, body = h.do("POST", "/api/orders",
		orderBody("key-conflict", map[string]any{"product_id": "spotify-1m"}), nil)
	if code != http.StatusConflict {
		t.Fatalf("changed body: %d %s, want 409", code, body)
	}
	var e struct {
		Error string `json:"error"`
		Code  string `json:"code"`
	}
	decodeInto(t, body, &e)
	if e.Code != "idempotency_conflict" {
		t.Fatalf("code = %q, want idempotency_conflict (body %s)", e.Code, body)
	}
	if n := h.openOrderCount(); n != 1 {
		t.Fatalf("store holds %d orders after a conflicting retry, want 1", n)
	}
}

func TestIdempotentCreateTreatsExplicitDefaultAsSameRequest(t *testing.T) {
	h := newHarness(t)

	// First attempt omits the defaultable fields entirely.
	code, body := h.do("POST", "/api/orders", map[string]any{
		"idempotency_key":     "key-defaults",
		"product_id":          "netflix-1m",
		"catalog_price_minor": int64(90000),
	}, nil)
	if code != http.StatusCreated {
		t.Fatalf("first create: %d %s", code, body)
	}
	var first types.Order
	decodeInto(t, body, &first)

	// Second attempt spells out the same defaults (currency PKR, easypaisa,
	// the server's order TTL). That is the same request and must replay.
	code, body = h.do("POST", "/api/orders", map[string]any{
		"idempotency_key":     "key-defaults",
		"product_id":          "netflix-1m",
		"catalog_price_minor": int64(90000),
		"currency":            "PKR",
		"method":              "easypaisa",
	}, nil)
	if code != http.StatusOK {
		t.Fatalf("retry with explicit defaults: %d %s, want 200", code, body)
	}
	var second types.Order
	decodeInto(t, body, &second)
	if second.ID != first.ID {
		t.Fatalf("retry returned %s, want %s", second.ID, first.ID)
	}
}

func TestIdempotencyKeysAreScopedPerBuyer(t *testing.T) {
	h := newHarness(t)

	code, body := h.do("POST", "/api/orders",
		orderBody("shared-key", map[string]any{"user_id": "buyer-a"}), nil)
	if code != http.StatusCreated {
		t.Fatalf("buyer a create: %d %s", code, body)
	}
	var aOrder types.Order
	decodeInto(t, body, &aOrder)

	// Buyer b reusing the same key string must get its own order, not buyer
	// a's. If the scope were global the second call would 200 with aOrder.
	code, body = h.do("POST", "/api/orders",
		orderBody("shared-key", map[string]any{"user_id": "buyer-b"}), nil)
	if code != http.StatusCreated {
		t.Fatalf("buyer b create: %d %s, want 201 (keys are scoped per buyer)", code, body)
	}
	var bOrder types.Order
	decodeInto(t, body, &bOrder)
	if bOrder.ID == aOrder.ID {
		t.Fatal("buyer b was handed buyer a's order through a shared idempotency key")
	}
}

// TestIdempotencyConcurrentRetriesCreateOneOrder fires the same key from many
// goroutines at once, which is what a client that retries on a slow response
// actually does. Exactly one 201 must appear; the rest are replays (200) or a
// retryable in-flight answer (409) - never a second order.
func TestIdempotencyConcurrentRetriesCreateOneOrder(t *testing.T) {
	h := newHarness(t, func(c *api.Config) {
		// Lift the create limit so the test measures idempotency, not the
		// limiter (the limiter has its own tests).
		c.RateLimits = map[api.RateLimitClass]api.RateLimit{
			api.RateLimitOrderWrite: {PerMinute: 6000, Burst: 100},
		}
	})

	const n = 12
	var wg sync.WaitGroup
	codes := make([]int, n)
	bodies := make([]string, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			code, body := h.do("POST", "/api/orders", orderBody("key-race", nil), nil)
			codes[i], bodies[i] = code, string(body)
		}(i)
	}
	wg.Wait()

	created, replayed := 0, 0
	ids := map[string]bool{}
	for i, code := range codes {
		switch code {
		case http.StatusCreated:
			created++
			var o types.Order
			decodeInto(t, []byte(bodies[i]), &o)
			ids[o.ID] = true
		case http.StatusOK:
			replayed++
			var o types.Order
			decodeInto(t, []byte(bodies[i]), &o)
			ids[o.ID] = true
		case http.StatusConflict:
			var e struct {
				Code string `json:"code"`
			}
			decodeInto(t, []byte(bodies[i]), &e)
			if e.Code != "idempotency_in_flight" {
				t.Fatalf("409 with code %q, want idempotency_in_flight", e.Code)
			}
		default:
			t.Fatalf("request %d: unexpected status %d %s", i, code, bodies[i])
		}
	}
	if created != 1 {
		t.Fatalf("got %d 201 responses for one key, want exactly 1", created)
	}
	if len(ids) != 1 {
		t.Fatalf("responses named %d distinct orders (%v), want 1", len(ids), ids)
	}
	if n := h.openOrderCount(); n != 1 {
		t.Fatalf("store holds %d orders after %d concurrent retries of one key, want 1", n, n)
	}
	t.Logf("concurrent retries: %d created, %d replayed, %d in-flight", created, replayed, n-created-replayed)
}

// TestIdempotentCreateFailureFreesTheKey proves a rejected request does not
// burn the key: the same key must work once the client fixes the request.
func TestIdempotentCreateFailureFreesTheKey(t *testing.T) {
	h := newHarness(t)

	code, body := h.do("POST", "/api/orders", orderBody("key-fix", map[string]any{
		"catalog_price_minor": int64(-5),
	}), nil)
	if code != http.StatusBadRequest {
		t.Fatalf("invalid price: %d %s, want 400", code, body)
	}

	code, body = h.do("POST", "/api/orders", orderBody("key-fix", map[string]any{
		"catalog_price_minor": int64(120000),
	}), nil)
	if code != http.StatusCreated {
		t.Fatalf("corrected retry: %d %s, want 201 (a failed attempt must not hold the key)", code, body)
	}
}

func TestIdempotencyKeyIsRecordedDurably(t *testing.T) {
	h := newHarness(t)

	code, body := h.do("POST", "/api/orders", orderBody("key-durable", map[string]any{
		"user_id": "buyer-z",
	}), nil)
	if code != http.StatusCreated {
		t.Fatalf("create: %d %s", code, body)
	}
	var ord types.Order
	decodeInto(t, body, &ord)

	// The record lives in the database, not in server memory: that is what
	// makes a retry after a restart still replay instead of duplicating.
	rec, ok, err := h.store.IdempotencyRecord(context.Background(), "buyer-z", "key-durable")
	if err != nil {
		t.Fatalf("read idempotency record: %v", err)
	}
	if !ok {
		t.Fatal("no idempotency record persisted")
	}
	if rec.OrderID != ord.ID {
		t.Fatalf("record order_id = %q, want %q", rec.OrderID, ord.ID)
	}
	if string(rec.State) != "done" {
		t.Fatalf("record state = %q, want done", rec.State)
	}
	if rec.RequestHash == "" {
		t.Fatal("record has no request hash; a changed body could not be detected")
	}
}

func TestCreateWithoutKeyAlwaysCreates(t *testing.T) {
	h := newHarness(t)
	for i := 0; i < 3; i++ {
		code, body := h.do("POST", "/api/orders", orderBody("", nil), nil)
		if code != http.StatusCreated {
			t.Fatalf("create %d: %d %s", i, code, body)
		}
	}
	if n := h.openOrderCount(); n != 3 {
		t.Fatalf("store holds %d orders from 3 keyless creates, want 3", n)
	}
}

// TestIdempotentReplayAfterRestart documents why the record lives in the
// database and not in server memory: the handler path that replays is the same
// one that reads the store, so a restarted process replays too. The store
// method is exercised directly here because the HTTP harness is built around
// one store per process.
func TestIdempotentReplayAfterRestart(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "restart.db")

	first, err := store.Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	ord, err := first.CreateOrder(context.Background(), store.CreateOrderInput{
		ProductID: "netflix-1m", CatalogPrice: 150000,
		Currency: "PKR", Method: types.MethodEasypaisa,
	})
	if err != nil {
		t.Fatalf("create order: %v", err)
	}
	hash := api.OrderRequestHash(api.OrderHashInput{
		ProductID: "netflix-1m", CatalogPrice: 150000,
		Currency: "PKR", Method: string(types.MethodEasypaisa), TTLSeconds: 30 * 60,
	})
	if _, _, err := first.ClaimIdempotencyKey(context.Background(), "anon", "key-restart", hash, time.Minute); err != nil {
		t.Fatalf("claim: %v", err)
	}
	if ok, err := first.CompleteIdempotencyKey(context.Background(), "anon", "key-restart", hash, ord.ID); err != nil || !ok {
		t.Fatalf("complete: ok=%v err=%v", ok, err)
	}
	first.Close()

	// "Restart": a brand new *store.Store over the same file.
	second, err := store.Open(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer second.Close()

	rec, found, err := second.IdempotencyRecord(context.Background(), "anon", "key-restart")
	if err != nil || !found {
		t.Fatalf("record after reopen: found=%v err=%v", found, err)
	}
	if rec.OrderID != ord.ID || rec.State != store.IdemDone {
		t.Fatalf("record after reopen = %+v, want order %s in state done", rec, ord.ID)
	}
}

// TestIdempotencyInFlightReturns409 pins the "another request holds the key"
// answer, which the concurrency test can only observe by luck. Holding the
// claim by hand makes it deterministic.
func TestIdempotencyInFlightReturns409(t *testing.T) {
	h := newHarness(t)

	// OrderRequestHash is what the handler stores; pre-claiming with it makes
	// the row look exactly like a request that is still being processed.
	hash := api.OrderRequestHash(api.OrderHashInput{
		ProductID:    "netflix-1m",
		CatalogPrice: 150000,
		Currency:     "PKR",
		Method:       "easypaisa",
		TTLSeconds:   15 * 60,
	})
	outcome, _, err := h.store.ClaimIdempotencyKey(context.Background(), "anon", "key-busy", hash, time.Minute)
	if err != nil {
		t.Fatalf("pre-claim: %v", err)
	}
	if outcome != store.IdemClaimed {
		t.Fatalf("pre-claim outcome = %v, want IdemClaimed", outcome)
	}

	code, body := h.do("POST", "/api/orders", orderBody("key-busy", nil), nil)
	if code != http.StatusConflict {
		t.Fatalf("create while key in flight: %d %s, want 409", code, body)
	}
	var e struct {
		Error string `json:"error"`
		Code  string `json:"code"`
	}
	decodeInto(t, body, &e)
	if e.Code != "idempotency_in_flight" {
		t.Fatalf("code = %q, want idempotency_in_flight", e.Code)
	}
	// Nothing may have been created while another request owned the key.
	if n := h.openOrderCount(); n != 0 {
		t.Fatalf("store holds %d orders, want 0 (the key was not ours)", n)
	}
}
