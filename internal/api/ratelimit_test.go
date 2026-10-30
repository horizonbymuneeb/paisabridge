package api

import (
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// unit tests: the bucket itself
// ---------------------------------------------------------------------------

// fakeClock is a manually advanced clock so bucket refill can be tested
// without sleeping (and without a flaky CI).
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func newFakeClock() *fakeClock {
	return &fakeClock{t: time.Date(2026, 10, 31, 9, 0, 0, 0, time.UTC)}
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

func TestLimiterAllowsBurstThenRefuses(t *testing.T) {
	clock := newFakeClock()
	l := newLimiter(map[RateLimitClass]RateLimit{
		RateLimitOrderWrite: {PerMinute: 10, Burst: 10},
	}, clock.Now)

	for i := 0; i < 10; i++ {
		if res := l.take("orders_write|1.2.3.4", RateLimitOrderWrite); !res.Allowed {
			t.Fatalf("request %d within burst was refused", i+1)
		}
	}
	res := l.take("orders_write|1.2.3.4", RateLimitOrderWrite)
	if res.Allowed {
		t.Fatal("request 11 was allowed; burst of 10 must not admit 11")
	}
	// 10/min = one token every 6s, so the caller is told to come back in <= 6s.
	if res.RetryAfter <= 0 || res.RetryAfter > 6*time.Second+time.Second {
		t.Fatalf("RetryAfter = %v, want (0, 6s]", res.RetryAfter)
	}
}

func TestLimiterRefillsAtConfiguredRate(t *testing.T) {
	clock := newFakeClock()
	l := newLimiter(map[RateLimitClass]RateLimit{
		RateLimitOrderWrite: {PerMinute: 60, Burst: 1}, // one per second
	}, clock.Now)

	if res := l.take("orders_write|k", RateLimitOrderWrite); !res.Allowed {
		t.Fatal("first request refused")
	}
	if res := l.take("orders_write|k", RateLimitOrderWrite); res.Allowed {
		t.Fatal("second immediate request allowed with burst=1")
	}
	clock.Advance(time.Second)
	if res := l.take("orders_write|k", RateLimitOrderWrite); !res.Allowed {
		t.Fatal("request after 1s at 60/min was refused; refill is broken")
	}
}

func TestLimiterIsolatesClientsAndClasses(t *testing.T) {
	clock := newFakeClock()
	l := newLimiter(map[RateLimitClass]RateLimit{
		RateLimitOrderWrite: {PerMinute: 10, Burst: 1},
		RateLimitWebhook:    {PerMinute: 300, Burst: 1},
	}, clock.Now)

	if res := l.take("orders_write|a", RateLimitOrderWrite); !res.Allowed {
		t.Fatal("client a first request refused")
	}
	if res := l.take("orders_write|a", RateLimitOrderWrite); res.Allowed {
		t.Fatal("client a over its limit was allowed")
	}
	// A different client has its own bucket.
	if res := l.take("orders_write|b", RateLimitOrderWrite); !res.Allowed {
		t.Fatal("client b was refused because client a exhausted its bucket")
	}
	// The same client on another class has its own bucket.
	if res := l.take("webhook|a", RateLimitWebhook); !res.Allowed {
		t.Fatal("webhook class was refused because the order class was exhausted")
	}
}

func TestLimiterDisabledClassNeverRefuses(t *testing.T) {
	clock := newFakeClock()
	l := newLimiter(map[RateLimitClass]RateLimit{
		RateLimitWallet: {PerMinute: -1},
	}, clock.Now)
	for i := 0; i < 5000; i++ {
		if res := l.take("wallet|k", RateLimitWallet); !res.Allowed {
			t.Fatalf("disabled class refused request %d", i)
		}
	}
}

// TestLimiterSweepDropsIdleBuckets is the memory-growth guard: without the
// sweep, every IP that ever sent one request would keep a bucket forever.
func TestLimiterSweepDropsIdleBuckets(t *testing.T) {
	clock := newFakeClock()
	l := newLimiter(map[RateLimitClass]RateLimit{
		RateLimitWebhook: {PerMinute: 300, Burst: 5},
	}, clock.Now)

	for i := 0; i < 500; i++ {
		l.take("webhook|10.0.0."+itoa(i), RateLimitWebhook)
	}
	if got := l.len(); got != 500 {
		t.Fatalf("tracked buckets = %d, want 500 before any sweep", got)
	}

	// Not yet a full sweep interval: buckets stay, because dropping them
	// early would hand a caller a fresh burst for free.
	clock.Advance(30 * time.Second)
	l.take("webhook|10.0.0.1", RateLimitWebhook)
	if got := l.len(); got != 500 {
		t.Fatalf("tracked buckets = %d after 30s, want 500 (swept too early)", got)
	}

	// Past the interval the next request triggers a sweep. The bucket touched
	// above is only 31s idle, so it survives - a caller who shows up every 30s
	// keeps its bucket and its honesty. Everything else is reclaimed.
	clock.Advance(31 * time.Second)
	l.take("webhook|10.0.0.999", RateLimitWebhook)
	if got := l.len(); got != 2 {
		t.Fatalf("tracked buckets = %d after sweep, want 2 (the 31s-old caller + the new one)", got)
	}
	// And after a full idle minute even those two go: memory is bounded by
	// the number of clients seen in the last minute, not by the process
	// lifetime.
	clock.Advance(61 * time.Second)
	l.take("webhook|10.0.0.998", RateLimitWebhook)
	if got := l.len(); got != 1 {
		t.Fatalf("tracked buckets = %d after a long idle gap, want 1", got)
	}
}

// TestLimiterBucketCapIsEnforced checks the hard ceiling that protects the
// process when a flood of distinct source addresses defeats the time-based
// sweep.
func TestLimiterBucketCapIsEnforced(t *testing.T) {
	clock := newFakeClock()
	l := newLimiter(map[RateLimitClass]RateLimit{
		RateLimitWebhook: {PerMinute: 300, Burst: 5},
	}, clock.Now)
	// Fill the map to the cap directly; the limiter must not grow past it.
	for i := 0; i < maxBuckets; i++ {
		l.take("webhook|h"+itoa(i), RateLimitWebhook)
		// Keep the clock still so the sweep cannot reclaim anything.
	}
	before := l.len()
	l.take("webhook|overflow", RateLimitWebhook)
	after := l.len()
	if after > maxBuckets {
		t.Fatalf("bucket map grew to %d, cap is %d", after, maxBuckets)
	}
	if after > before+1 {
		t.Fatalf("bucket map grew from %d to %d for one new client", before, after)
	}
}

func TestLimiterConcurrentTakesAreRaceFree(t *testing.T) {
	clock := newFakeClock()
	l := newLimiter(map[RateLimitClass]RateLimit{
		RateLimitOrderRead: {PerMinute: 120, Burst: 40},
	}, clock.Now)

	var wg sync.WaitGroup
	var mu sync.Mutex
	allowed := 0
	for i := 0; i < 200; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if l.take("orders_read|same", RateLimitOrderRead).Allowed {
				mu.Lock()
				allowed++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	// The clock does not move, so exactly the burst is admitted: this proves
	// the limit holds under concurrency instead of leaking tokens.
	if allowed != 40 {
		t.Fatalf("allowed = %d concurrent requests, want exactly the burst (40)", allowed)
	}
}

// ---------------------------------------------------------------------------
// unit tests: identity + classification
// ---------------------------------------------------------------------------

func TestClientIPIgnoresForwardedForUnlessTrusted(t *testing.T) {
	r := httptest.NewRequest(http.MethodPost, "/api/orders", strings.NewReader("{}"))
	r.RemoteAddr = "203.0.113.7:5555"
	r.Header.Set("X-Forwarded-For", "1.1.1.1, 10.0.0.1")

	if got := clientIP(r, false); got != "203.0.113.7" {
		t.Fatalf("clientIP(untrusted) = %q, want the socket peer 203.0.113.7", got)
	}
	if got := clientIP(r, true); got != "1.1.1.1" {
		t.Fatalf("clientIP(trusted) = %q, want the leftmost forwarded address 1.1.1.1", got)
	}
}

func TestClientIPRejectsJunkForwardedFor(t *testing.T) {
	for _, xff := range []string{"unknown", "not-an-ip", "", "1.1.1.1junk"} {
		r := httptest.NewRequest(http.MethodPost, "/api/orders", strings.NewReader("{}"))
		r.RemoteAddr = "198.51.100.9:4444"
		if xff != "" {
			r.Header.Set("X-Forwarded-For", xff)
		}
		if got := clientIP(r, true); got != "198.51.100.9" {
			t.Fatalf("clientIP(xff=%q) = %q, want fallback to the socket peer", xff, got)
		}
	}
}

func TestClassifyRoutes(t *testing.T) {
	cases := []struct {
		method, path string
		want         RateLimitClass
	}{
		{"POST", "/api/orders", RateLimitOrderWrite},
		{"GET", "/api/orders/ord_123", RateLimitOrderRead},
		{"GET", "/api/orders/PB-7F3K2Q", RateLimitOrderRead},
		{"POST", "/api/orders/ord_123/claim", RateLimitOrderAction},
		{"POST", "/api/orders/ord_123/cancel", RateLimitOrderAction},
		{"POST", "/api/webhooks/easypaisa", RateLimitWebhook},
		{"POST", "/api/webhooks/bank", RateLimitWebhook},
		{"GET", "/api/wallet", RateLimitWallet},
		{"POST", "/api/wallet/topup", RateLimitWallet},
		{"POST", "/api/admin/approve", RateLimitAdmin},
		{"GET", "/api/admin/orders", RateLimitAdmin},
		{"GET", "/api/healthz", RateLimitClass(0)},
		{"GET", "/nope", RateLimitClass(0)},
	}
	for _, c := range cases {
		r := httptest.NewRequest(c.method, c.path, nil)
		if got := classify(r); got != c.want {
			t.Errorf("classify(%s %s) = %v, want %v", c.method, c.path, got, c.want)
		}
	}
}

// itoa keeps the test bodies free of fmt noise.
func itoa(i int) string { return strconv.Itoa(i) }
