package api

import (
	"math"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

// ---------------------------------------------------------------------------
// Rate limiting
// ---------------------------------------------------------------------------
//
// Why this exists: every route on this server is either a way to create work
// (POST /api/orders picks a unique amount by scanning open orders) or a way to
// spend money (webhooks, wallet credit, admin approval). Without a per-caller
// ceiling one script can exhaust the unique-amount window, hammer the ledger,
// or brute-force the admin token. The limiter is therefore the cheapest
// possible defence: an in-process token bucket per (client, route class), no
// dependencies, no database write per request.
//
// Design choices worth knowing before changing anything here:
//
//   - A bucket belongs to a *route class*, not a URL. Otherwise every distinct
//     order id would get its own bucket and the limit would mean nothing.
//   - The bucket is a token bucket: it holds at most `burst` requests, refills
//     at `perMinute/60` tokens per second, and one request costs one token.
//     A caller who stays under the sustained rate never waits; a short spike
//     up to `burst` is absorbed; anything past that is 429.
//   - Buckets are swept, but only when the map actually grows. A server that
//     has seen nothing but the health check never pays for a timer.
//   - State is per-process. Behind a load balancer with N replicas the
//     effective limit is N times the numbers below. That is a deliberate
//     trade: no shared counter means no extra hop on the hot path.
//   - The limiter fails open on configuration mistakes (see Config), never
//     closed, because a mis-typed limit must not take payments down.

// RateLimitClass names a group of routes that share one bucket per caller.
// It is the key callers use in Config.RateLimits, so it has to be exported.
type RateLimitClass uint8

// The classes the built-in route table uses.
const (
	RateLimitOrderWrite  RateLimitClass = iota + 1 // POST /api/orders
	RateLimitOrderRead                             // GET /api/orders/{id}
	RateLimitOrderAction                           // POST /api/orders/{id}/claim|cancel
	RateLimitWebhook                               // POST /api/webhooks/{method}
	RateLimitWallet                                // GET /api/wallet, POST /api/wallet/topup
	RateLimitAdmin                                 // /api/admin/*
)

// String is the label used in log lines and bucket keys.
func (c RateLimitClass) String() string {
	switch c {
	case RateLimitOrderWrite:
		return "orders_write"
	case RateLimitOrderRead:
		return "orders_read"
	case RateLimitOrderAction:
		return "orders_action"
	case RateLimitWebhook:
		return "webhook"
	case RateLimitWallet:
		return "wallet"
	case RateLimitAdmin:
		return "admin"
	}
	return "none"
}

// RateLimit is one token-bucket definition.
type RateLimit struct {
	// PerMinute is the sustained refill rate. Zero or negative means
	// "no limit" for this class.
	PerMinute int
	// Burst is how many requests can be answered instantly. Zero means
	// "one second's worth", with a floor of 1.
	Burst int
}

// normalised returns a copy with Burst filled in.
func (rl RateLimit) normalised() RateLimit {
	if rl.PerMinute <= 0 {
		return RateLimit{}
	}
	if rl.Burst <= 0 {
		// One second of traffic, never less than a single request, so even a
		// 1/min limit lets the first request through.
		rl.Burst = rl.PerMinute / 60
		if rl.Burst < 1 {
			rl.Burst = 1
		}
	}
	return rl
}

// classNone marks a route the limiter ignores (health checks, 404s).
const classNone = RateLimitClass(0)

// DefaultRateLimits is what a zero-valued Config gets. The numbers are chosen
// against how a real deployment is shaped, not round numbers:
//
//   - orders_write 10/min/IP: one legitimate buyer creates one order per
//     purchase, retries a couple of times. Ten a minute is generous for a
//     human and stops an amount-window scan cold.
//   - orders_read 120/min/IP: the status endpoint is polled by a checkout page
//     every few seconds; 2/s per caller is a comfortable ceiling.
//   - orders_action 20/min/IP: claim/cancel are user actions, not polled.
//     Lower than orders_read because each one can touch the wallet ledger.
//   - webhook 300/min/IP: providers retry in bursts and may share one egress
//     IP for every merchant. This must be high enough that a provider retry
//     storm is never the reason a real payment is dropped. Burst raised to a
//     full minute's worth so a backlog delivers in one push.
//   - wallet 30/min/IP: balance reads plus top-ups; top-ups are already
//     idempotent, so the limit only has to stop a login-stuffing style sweep.
//   - admin 20/min/IP: approve is the one endpoint that can complete an order
//     without a verified payment fact, so it is tight, fails fast, and every
//     refusal is logged.
var DefaultRateLimits = map[RateLimitClass]RateLimit{
	RateLimitOrderWrite:  {PerMinute: 10, Burst: 10},
	RateLimitOrderRead:   {PerMinute: 120, Burst: 40},
	RateLimitOrderAction: {PerMinute: 20, Burst: 8},
	RateLimitWebhook:     {PerMinute: 300, Burst: 60},
	RateLimitWallet:      {PerMinute: 30, Burst: 10},
	RateLimitAdmin:       {PerMinute: 20, Burst: 5},
}

// bucket is the mutable state for one (client, class) pair.
type bucket struct {
	tokens   float64
	lastSeen time.Time
}

// limiter holds every bucket. One limiter per Server.
type limiter struct {
	mu      sync.Mutex
	limits  map[RateLimitClass]RateLimit
	buckets map[string]*bucket
	// lastSweep bounds how often the map may be walked.
	lastSweep time.Time
	// now is injectable so tests can advance time without sleeping.
	now func() time.Time
}

// rateLimitSweepInterval is how stale a bucket must be before it is deleted.
// A bucket that has refilled completely is indistinguishable from a new one,
// so keeping it holds no information the limiter needs.
const rateLimitSweepInterval = time.Minute

// cleanupEvery is the minimum time between two sweeps of the bucket map.
// Ordinary requests never pay for a full walk more than once a minute.
const cleanupEvery = time.Minute

// maxBuckets is a hard ceiling on tracked clients. If a flood of spoofed
// source addresses ever pushes past it, the limiter sweeps, and if the map is
// still full it evicts the oldest bucket instead of growing without bound.
const maxBuckets = 100_000

func newLimiter(limits map[RateLimitClass]RateLimit, now func() time.Time) *limiter {
	if now == nil {
		now = func() time.Time { return time.Now().UTC() }
	}
	cp := make(map[RateLimitClass]RateLimit, len(limits))
	for k, v := range limits {
		cp[k] = v.normalised()
	}
	return &limiter{
		limits:  cp,
		buckets: make(map[string]*bucket),
		now:     now,
	}
}

// limitFor returns the configured (normalised) limit for a class. A missing
// class is unlimited.
func (l *limiter) limitFor(c RateLimitClass) RateLimit {
	if l == nil || c == classNone {
		return RateLimit{}
	}
	return l.limits[c]
}

// takeResult is the outcome of one Take call.
type takeResult struct {
	Allowed bool
	// RetryAfter is how long the caller must wait for a token, rounded up to
	// whole seconds, with a floor of 1 (a Retry-After: 0 tells a client to
	// retry immediately, which is the opposite of what we want).
	RetryAfter time.Duration
}

// take consumes one token for key and reports whether the request may run.
// The key already includes the class, so callers pass one flat string.
func (l *limiter) take(key string, c RateLimitClass) takeResult {
	rl := l.limitFor(c)
	if rl.PerMinute <= 0 {
		return takeResult{Allowed: true}
	}

	now := l.now()
	l.mu.Lock()
	defer l.mu.Unlock()

	if l.lastSweep.IsZero() || now.Sub(l.lastSweep) >= cleanupEvery || len(l.buckets) >= maxBuckets {
		l.sweepLocked(now)
	}

	b, ok := l.buckets[key]
	if !ok {
		// A new caller starts full: the first request is always allowed.
		if len(l.buckets) >= maxBuckets {
			l.evictOldestLocked()
		}
		b = &bucket{tokens: float64(rl.Burst), lastSeen: now}
		l.buckets[key] = b
	} else {
		l.refill(b, rl, now)
	}

	if b.tokens >= 1 {
		b.tokens--
		return takeResult{Allowed: true}
	}
	// Missing tokens divided by tokens-per-second == seconds to wait.
	missing := 1 - b.tokens
	perSecond := float64(rl.PerMinute) / 60
	wait := time.Duration(missing/perSecond*float64(time.Second)) + time.Millisecond
	secs := time.Duration(math.Ceil(wait.Seconds())) * time.Second
	if secs < time.Second {
		secs = time.Second
	}
	return takeResult{Allowed: false, RetryAfter: secs}
}

// refill adds the tokens earned since the bucket was last used, capped at the
// burst size.
func (l *limiter) refill(b *bucket, rl RateLimit, now time.Time) {
	elapsed := now.Sub(b.lastSeen)
	if elapsed <= 0 {
		b.lastSeen = now
		return
	}
	b.tokens += elapsed.Seconds() * float64(rl.PerMinute) / 60
	if b.tokens > float64(rl.Burst) {
		b.tokens = float64(rl.Burst)
	}
	b.lastSeen = now
}

// sweepLocked drops buckets that have not been touched for a full sweep
// interval. A bucket untouched that long has refilled completely (no limit in
// DefaultRateLimits needs more than a few seconds to refill), so dropping it
// cannot give a caller a free burst.
func (l *limiter) sweepLocked(now time.Time) {
	for k, b := range l.buckets {
		if now.Sub(b.lastSeen) >= rateLimitSweepInterval {
			delete(l.buckets, k)
		}
	}
	l.lastSweep = now
}

// evictOldestLocked removes the least recently seen bucket. Only reached when
// the map is at maxBuckets even after a sweep, i.e. under address-spoofing
// pressure; dropping the coldest entry is better than unbounded memory.
func (l *limiter) evictOldestLocked() {
	var oldestKey string
	var oldest time.Time
	for k, b := range l.buckets {
		if oldestKey == "" || b.lastSeen.Before(oldest) {
			oldestKey, oldest = k, b.lastSeen
		}
	}
	if oldestKey != "" {
		delete(l.buckets, oldestKey)
	}
}

// len reports the number of tracked buckets (tests and diagnostics).
func (l *limiter) len() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.buckets)
}

// sweep forces a sweep regardless of the interval (tests and the sweeper).
func (l *limiter) sweep(now time.Time) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.sweepLocked(now)
}

// ---------------------------------------------------------------------------
// identity + classification
// ---------------------------------------------------------------------------

// clientIP is the address a limit is charged to.
//
// X-Forwarded-For is attacker-controlled and only meaningful when the server
// actually sits behind a proxy that overwrites (not appends to) the header, so
// it is honoured only when Config.TrustProxy is set. When it is:
//
//   - the leftmost entry is the original client, which is what a proxy chain
//     appends, and is the only entry a proxy did not write itself;
//   - a value that is not an IP, or one of the sentinel strings some proxies
//     send (e.g. "unknown"), falls back to the socket address rather than
//     becoming a bucket of its own;
//   - the rightmost entry is deliberately NOT used: with `proxy_set_header
//     X-Forwarded-For $proxy_add_x_forwarded_for` the client can prepend a
//     fake address to the right.
func clientIP(r *http.Request, trustProxy bool) string {
	if trustProxy {
		if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
			first := xff
			if i := strings.IndexByte(xff, ','); i >= 0 {
				first = xff[:i]
			}
			first = strings.TrimSpace(first)
			if ip := net.ParseIP(first); ip != nil {
				return ip.String()
			}
		}
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	if ip := net.ParseIP(host); ip != nil {
		return ip.String()
	}
	if host == "" {
		// No usable address (tests, unix sockets): share one bucket rather
		// than treat every request as a new caller.
		return "unknown"
	}
	return host
}

// classify maps a request to its bucket class. Ordering matters: the webhook
// and admin prefixes are checked before the generic order routes.
func classify(r *http.Request) RateLimitClass {
	p := r.URL.Path
	switch {
	case p == "/api/orders" && r.Method == http.MethodPost:
		return RateLimitOrderWrite
	case strings.HasPrefix(p, "/api/webhooks/"):
		return RateLimitWebhook
	case strings.HasPrefix(p, "/api/admin/"):
		return RateLimitAdmin
	case strings.HasPrefix(p, "/api/wallet"):
		return RateLimitWallet
	case strings.HasPrefix(p, "/api/orders/"):
		if strings.HasSuffix(p, "/claim") || strings.HasSuffix(p, "/cancel") {
			return RateLimitOrderAction
		}
		if r.Method == http.MethodGet {
			return RateLimitOrderRead
		}
		return RateLimitOrderWrite
	}
	return classNone
}

// ---------------------------------------------------------------------------
// middleware
// ---------------------------------------------------------------------------

// rateLimit admits or refuses the request. Refusals use the same error
// envelope as every other endpoint ({"error":..,"code":"rate_limited"}) plus a
// Retry-After header in whole seconds, so a client can back off without
// parsing prose.
func (s *Server) rateLimit(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		class := classify(r)
		if class == classNone || s.limiter == nil {
			next.ServeHTTP(w, r)
			return
		}
		key := class.String() + "|" + clientIP(r, s.cfg.TrustProxy)
		res := s.limiter.take(key, class)
		if res.Allowed {
			next.ServeHTTP(w, r)
			return
		}

		secs := int(math.Ceil(res.RetryAfter.Seconds()))
		if secs < 1 {
			secs = 1
		}
		w.Header().Set("Retry-After", strconv.Itoa(secs))
		// Log at info, not warn: a rate-limited client is expected traffic
		// under attack and must not be able to flood the error log.
		s.log.Info("rate limited",
			"class", class.String(), "ip", clientIP(r, s.cfg.TrustProxy),
			"path", r.URL.Path, "retry_after_s", secs)
		// The envelope is the documented {"error":..,"code":..}; the class and
		// the wait are both machine-readable (Retry-After + detail).
		s.noteMetrics(func(m Recorder) { m.RateLimited(class.String()) })
		writeErr(w, http.StatusTooManyRequests, rateLimitedCode,
			"limit="+class.String(), "retry_after_s="+strconv.Itoa(secs))
	})
}

// rateLimitedCode is the machine code for a 429 in the documented envelope.
const rateLimitedCode = "rate_limited"
