package metrics

// This file is the single place where every metric family is declared. The
// API and the outbound sender only ever reach into the exported handles below,
// never into a stringly-typed lookup, so a typo cannot silently create a second
// family with a slightly different name.
//
// Label values are closed sets by construction: route classes come from the
// API's own class table, rails from types.Method, matcher outcomes from
// match.Reason*, and outbound results from outboundResults. HTTP status is
// reduced to its class (2xx/4xx/5xx) on purpose, so a scanner probing random
// paths cannot grow the registry.

// Handles over the process registry (Default). Tests build their own Registry
// with Register and assert on those, so a test never observes another test's
// counters.
var (
	// The zero-value handles are inert (every method is nil-safe), so code
	// that runs before Register — or a test that never calls it — cannot
	// panic, it just records nothing.

	// Orders counts created orders by rail and initial status.
	Orders CounterVec
	// Payments counts inbound payment notices by outcome. The reason label is
	// empty for a matched notice so the common case has one series.
	Payments CounterVec
	// WebhookFailures counts webhooks rejected before the matcher.
	WebhookFailures CounterVec
	// RateLimited counts 429s by route class.
	RateLimited CounterVec
	// Outbound counts outbound deliveries by result.
	Outbound CounterVec
	// HTTPDuration observes request latency by route class.
	HTTPDuration histogramHandle
	// HTTPRequests counts requests by route class and status class.
	HTTPRequests CounterVec
)

// histogramHandle wraps HistogramVec so the zero value is usable before
// Register runs (the API may record a request during New).
type histogramHandle struct{ v HistogramVec }

// Observe records one duration for a route class.
func (h histogramHandle) Observe(seconds float64, class string) {
	h.v.Observe(seconds, class)
}

// Register declares every family on r and points the package handles at it.
// It is called once by the server's constructor (and by tests with a private
// registry). Calling it twice on the same registry is harmless: Counter and
// Histogram are idempotent for identical label sets.
func Register(r *Registry) {
	Orders = r.Counter(OrdersCreated,
		"Orders created via POST /api/orders, by payment rail and initial status.",
		"method", "status")
	Payments = r.Counter(PaymentsMatched,
		"Inbound payment notices, by outcome (matched|unmatched) and matcher reason.",
		"outcome", "reason")
	WebhookFailures = r.Counter(WebhookSignatureFailures,
		"Inbound webhooks rejected for a bad or missing signature, by rail. A sustained rise is a forgery attempt or a secret mismatch.",
		"method")
	RateLimited = r.Counter(RateLimitedRequests,
		"Requests refused with 429 by the per-caller rate limiter, by route class.",
		"class")
	Outbound = r.Counter(OutboundDeliveries,
		"Outbound webhook deliveries by result: delivered (first attempt), retried (succeeded after a retry), failed (parked in the dead-letter table), permanent (4xx, never retried).",
		"event", "result")
	HTTPRequests = r.Counter(HTTPRequestTotal,
		"HTTP requests handled, by route class and status class.",
		"class", "status")
	HTTPDuration = histogramHandle{v: r.Histogram(HTTPDurationSeconds,
		"HTTP request duration in seconds, by route class.",
		HTTPDurationBuckets, "class")}
}
